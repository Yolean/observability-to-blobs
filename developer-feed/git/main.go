// dev-feed-git: the git collector of a developer feed. fluent-bit's exec
// input runs it every few minutes (developer-feed/collectors/collectors.d/
// git.yaml); each run prints one JSON line per thing that happened in the
// developer's repositories since the previous run, and keeps what it has
// seen in a state file per repository:
//
//	remote      a remote's fetch and push URLs, when they are new or change
//	ref-update  an entry of a branch's or remote-tracking branch's reflog:
//	            "update by push" from this clone, "fetch ..." of a push by
//	            anyone, "commit", "rebase ...", with the time and the new tip
//	commit      a commit that a ref reached since the previous run, with its
//	            parents, author, committer and full message
//
// Live only: the first run of a repository ships its remotes and records
// where its reflogs and tips stand, and nothing older; the history before
// that is the backfill binary's. A crash between printing and saving the
// state ships some lines twice, never loses one.
//
// It reads, never writes: only for-each-ref, log, rev-list and remote, with
// core.fsmonitor off and GIT_OPTIONAL_LOCKS=0, never status or fetch, so a
// repository's own config does not make it run anything and it takes no
// lock the developer's git could trip over.
//
// Env:
//
//	HOST_STATE_DIR      the agent's state directory; state goes in git/ there
//	HOME                ~/.claude/history.jsonl names the repositories
//	DEV_FEED_GIT_ROOTS  more repositories: colon-separated paths or globs
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	stateDir := os.Getenv("HOST_STATE_DIR")
	if stateDir == "" {
		fmt.Fprintln(os.Stderr, "dev-feed-git: HOST_STATE_DIR is not set (the agent's launcher sets it)")
		os.Exit(2)
	}
	repos := discover(os.Getenv("HOME"), os.Getenv("DEV_FEED_GIT_ROOTS"))
	out := bufio.NewWriter(os.Stdout)
	for _, r := range repos {
		if err := collect(r, filepath.Join(stateDir, "git"), out, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "dev-feed-git: %s: %v\n", r, err)
		}
	}
	if err := out.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "dev-feed-git:", err)
		os.Exit(1)
	}
}

// row is one line of the git table; empty fields are left out.
type row struct {
	Table          string   `json:"table"`
	Kind           string   `json:"kind"`
	Repo           string   `json:"repo"`
	Remote         string   `json:"remote,omitempty"`
	FetchURL       string   `json:"fetch_url,omitempty"`
	PushURL        string   `json:"push_url,omitempty"`
	Ref            string   `json:"ref,omitempty"`
	Sha            string   `json:"sha,omitempty"`
	ReflogTime     string   `json:"reflog_time,omitempty"` // the reflog entry's time, RFC 3339
	Parents        []string `json:"parents,omitempty"`
	AuthorName     string   `json:"author_name,omitempty"`
	AuthorEmail    string   `json:"author_email,omitempty"`
	AuthorTime     string   `json:"author_time,omitempty"`
	CommitterName  string   `json:"committer_name,omitempty"`
	CommitterEmail string   `json:"committer_email,omitempty"`
	CommitterTime  string   `json:"committer_time,omitempty"`
	Message        string   `json:"message,omitempty"` // the reflog message, or the commit's whole message
}

// seen is how much of a reflog a run read: its number of entries and the
// newest one's time. Entries are added at the newest end, so the ones
// beyond the count are new; when expiry has made the reflog shorter, the
// ones newer than the time are.
type seen struct {
	Count  int   `json:"count"`
	Newest int64 `json:"newest"`
}

// state is what a repository's previous run saw.
type state struct {
	Remotes map[string][2]string `json:"remotes"` // name: fetch, push
	Reflogs map[string]seen      `json:"reflogs"` // ref: how far its reflog was read
	Tips    map[string]string    `json:"tips"`    // ref: sha
}

func git(repo string, args ...string) ([]byte, error) {
	return gitIn(repo, "", args...)
}

// gitIn runs git with stdin: revisions go there rather than on the command
// line, which a repository with thousands of refs would overflow.
func gitIn(repo, stdin string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", repo, "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// protected are the folders macOS asks the user about before a process
// may read them (TCC), relative to the home directory. A git run there by
// the agent shows a dialog naming fluent-bit, the process it is charged to,
// and asks again for every new build of it.
var protected = []string{"Documents", "Desktop", "Downloads", "Library/Mobile Documents",
	"Pictures", "Movies", "Music"}

// isProtected tells by the path alone, without touching it.
func isProtected(home, path string) bool {
	for _, p := range protected {
		dir := filepath.Join(home, p)
		if path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// discover is every repository the feed follows: the projects of Claude
// Code's prompt history and DEV_FEED_GIT_ROOTS, each as its top level,
// once, sorted. A path that is not in a repository is skipped, and so is a
// history project in a folder macOS protects (protected); one named in
// DEV_FEED_GIT_ROOTS is read even there, as naming it is the user's choice.
func discover(home, roots string) []string {
	var candidates []string
	if home != "" {
		for _, p := range historyProjects(filepath.Join(home, ".claude", "history.jsonl")) {
			if !isProtected(home, p) {
				candidates = append(candidates, p)
			}
		}
	}
	for _, g := range strings.Split(roots, ":") {
		if g == "" {
			continue
		}
		m, _ := filepath.Glob(g)
		candidates = append(candidates, m...)
	}
	seen := map[string]bool{}
	var repos []string
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		top, err := git(c, "rev-parse", "--show-toplevel")
		if err != nil {
			continue
		}
		t := strings.TrimSpace(string(top))
		if !seen["top:"+t] {
			seen["top:"+t] = true
			repos = append(repos, t)
		}
	}
	sort.Strings(repos)
	return repos
}

func historyProjects(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var line struct {
			Project string `json:"project"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Project != "" {
			out = append(out, line.Project)
		}
	}
	return out
}

func collect(repo, stateDir string, w io.Writer, now time.Time) error {
	sum := sha256.Sum256([]byte(repo))
	stateFile := filepath.Join(stateDir, hex.EncodeToString(sum[:8])+".json")
	prev, first, err := loadState(stateFile)
	if err != nil {
		return err
	}
	cur := state{Remotes: map[string][2]string{}, Reflogs: map[string]seen{}, Tips: map[string]string{}}
	var rows []row

	remotes, err := listRemotes(repo)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cur.Remotes[n] = remotes[n]
		if p, ok := prev.Remotes[n]; !ok || p != remotes[n] {
			rows = append(rows, row{Kind: "remote", Remote: n, FetchURL: remotes[n][0], PushURL: remotes[n][1]})
		}
	}

	// one git for the tips; the reflogs are read as the files they are,
	// as a git per ref took minutes in a repository with thousands of them
	refsOut, err := git(repo, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/remotes")
	if err != nil {
		return err
	}
	commonOut, err := git(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common := strings.TrimSpace(string(commonOut))
	var tips []string
	for _, l := range strings.Split(strings.TrimSpace(string(refsOut)), "\n") {
		ref, sha, ok := strings.Cut(l, " ")
		if !ok || strings.HasSuffix(ref, "/HEAD") {
			continue
		}
		cur.Tips[ref] = sha
		tips = append(tips, sha)
		entries, err := reflog(common, ref)
		if err != nil {
			return err
		}
		p := prev.Reflogs[ref]
		fresh := entries[:0:0]
		switch {
		case first:
		case len(entries) >= p.Count:
			fresh = entries[:len(entries)-p.Count] // newest first
		default:
			for _, e := range entries {
				if e.unix > p.Newest {
					fresh = append(fresh, e)
				}
			}
		}
		for i := len(fresh) - 1; i >= 0; i-- {
			e := fresh[i]
			rows = append(rows, row{Kind: "ref-update", Ref: ref, Sha: e.sha, ReflogTime: time.Unix(e.unix, 0).UTC().Format(time.RFC3339), Message: e.msg})
		}
		c := seen{Count: len(entries)}
		if len(entries) > 0 {
			c.Newest = entries[0].unix
		}
		cur.Reflogs[ref] = c
	}

	if !first && len(tips) > 0 {
		commits, err := newCommits(repo, tips, prev.Tips)
		if err != nil {
			return err
		}
		rows = append(rows, commits...)
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		r.Table, r.Repo = "git", repo
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return saveState(stateFile, cur)
}

func listRemotes(repo string) (map[string][2]string, error) {
	out, err := git(repo, "remote", "-v")
	if err != nil {
		return nil, err
	}
	rs := map[string][2]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		v := rs[f[0]]
		switch f[2] {
		case "(fetch)":
			v[0] = f[1]
		case "(push)":
			v[1] = f[1]
		}
		rs[f[0]] = v
	}
	return rs, nil
}

type reflogEntry struct {
	sha  string
	unix int64
	msg  string
}

// reflog is a ref's reflog, newest first, read from its file in the
// repository's common git directory: one line per entry, oldest first,
// "<old> <new> <name> <<email>> <unix time> <zone>\t<message>". A ref
// without one (core.logAllRefUpdates off, or a ref that never moved here)
// has none.
func reflog(common, ref string) ([]reflogEntry, error) {
	b, err := os.ReadFile(filepath.Join(common, "logs", filepath.FromSlash(ref)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var es []reflogEntry
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		head, msg, _ := strings.Cut(l, "\t")
		f := strings.Fields(head)
		if len(f) < 5 {
			continue
		}
		t, err := strconv.ParseInt(f[len(f)-2], 10, 64)
		if err != nil {
			continue
		}
		es = append(es, reflogEntry{sha: f[1], unix: t, msg: msg})
	}
	for i, j := 0, len(es)-1; i < j; i, j = i+1, j-1 {
		es[i], es[j] = es[j], es[i]
	}
	return es, nil
}

// newCommits is every commit the current tips reach that the previous
// run's tips did not, oldest first.
func newCommits(repo string, tips []string, prevTips map[string]string) ([]row, error) {
	// a previous tip that was garbage collected can't be excluded: ask for
	// all of them in one cat-file
	var check strings.Builder
	prev := make([]string, 0, len(prevTips))
	for _, sha := range prevTips {
		prev = append(prev, sha)
		check.WriteString(sha + "^{commit}\n")
	}
	var revs strings.Builder
	for _, t := range tips {
		revs.WriteString(t + "\n")
	}
	if len(prev) > 0 {
		out, err := gitIn(repo, check.String(), "cat-file", "--batch-check=%(objectname)")
		if err != nil {
			return nil, err
		}
		for i, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if i < len(prev) && !strings.HasSuffix(l, " missing") {
				revs.WriteString("^" + prev[i] + "\n")
			}
		}
	}
	out, err := gitIn(repo, revs.String(), "log", "--stdin", "--topo-order", "--reverse",
		"--format=%H%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%cn%x1f%ce%x1f%cI%x1f%B%x1e")
	if err != nil {
		return nil, err
	}
	var rows []row
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		f := strings.SplitN(rec, "\x1f", 9)
		if len(f) != 9 {
			continue
		}
		var parents []string
		if f[1] != "" {
			parents = strings.Fields(f[1])
		}
		rows = append(rows, row{Kind: "commit", Sha: f[0], Parents: parents,
			AuthorName: f[2], AuthorEmail: f[3], AuthorTime: f[4],
			CommitterName: f[5], CommitterEmail: f[6], CommitterTime: f[7],
			Message: strings.TrimRight(f[8], "\n")})
	}
	return rows, nil
}

func loadState(path string) (s state, first bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, true, nil
	}
	if err != nil {
		return state{}, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		// a damaged state file starts the repository again, as a first run
		return state{}, true, nil
	}
	return s, false, nil
}

// saveState writes the state whole, then moves it into place.
func saveState(path string, s state) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
