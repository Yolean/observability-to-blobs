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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// category is one kind of history: where it is, and how far the backfill
// and the collector that follows it have shipped it. collect emits its
// rows and returns what saves the category's state once they are shipped.
type category struct {
	name, about string
	collect     func(*backfill) (save func() error, err error)
}

// categories, in the order a run takes them. A category added here is
// shipped from its beginning by the next run, whatever earlier runs shipped
// of the others.
var categories = []category{
	tailCategory("claude-transcript", "Claude Code's session transcripts (the claude collector)", tail{
		db: "claude-transcripts.db", glob: ".claude/projects/*/*.jsonl", table: "claude", kind: "transcript",
		path: regexp.MustCompile(`/projects/(?<project>[^/]+)/(?<session_id>[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})[^/]*\.jsonl$`),
		time: "timestamp"}),
	tailCategory("claude-subagent", "its subagents' transcripts (the claude collector)", tail{
		db: "claude-subagents.db", glob: ".claude/projects/*/*/subagents/*.jsonl", table: "claude", kind: "subagent",
		path: regexp.MustCompile(`/projects/(?<project>[^/]+)/(?<session_id>[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/subagents/[^/]+\.jsonl$`),
		time: "timestamp"}),
	tailCategory("claude-history", "its prompt history (the claude collector)", tail{
		db: "claude-history.db", glob: ".claude/history.jsonl", table: "claude", kind: "history", time: "timestamp"}),
	{name: "claude-memory", about: "its agents' memory files (the claude-memory collector)", collect: memory},
	tailCategory("hammerspoon", "Hammerspoon's activity log (the hammerspoon collector)", tail{
		db: "hammerspoon.db", glob: "Reposoft/productivity-logs/hammerspoon-activity-log.jsonl", table: "hammerspoon", time: "ts"}),
	{name: "cmux", about: "cmux's agent journal (the cmux collector)", collect: cmux},
	{name: "git", about: "reflogs, the commits they brought and remotes (the git collector)", collect: gitHistory},
}

// tail is a category that a collector follows with fluent-bit's tail
// (developer-feed/collectors/collectors.d/): its files, under the home
// directory, and the rows the collector's processors make of their lines.
type tail struct {
	db          string         // the collector's position database, in the agent's state
	glob        string         // its path, under the home directory
	table, kind string         // its modify processor's
	path        *regexp.Regexp // its content_modifier extract, on path
	time        string         // the line's field with its own time: RFC 3339, or epoch milliseconds
}

// tailState is how far the backfill has shipped each file.
type tailState struct {
	Files map[string]fileState `json:"files"`
}

type fileState struct {
	Inode   uint64 `json:"inode"`
	Shipped int64  `json:"shipped"` // the backfill has shipped the file up to here
	// the collector's registration (its database's created) when the
	// backfill handed the file to it: shipping is done until another
	HandedOff int64 `json:"handed_off,omitempty"`
}

// liveFile is the collector's row for a file.
type liveFile struct {
	Name    string `json:"name"`
	Offset  int64  `json:"offset"`
	Inode   uint64 `json:"inode"`
	Created int64  `json:"created"`
}

// startWindow is how long after the agent starts its tail inputs may still
// be registering the files they found (s): they register a file found at
// the start at its end, and a file found later from its first line. On the
// Mac this was written on, 375 files were registered within the second.
// Too short would leave the head of a file found at a slow start
// unshipped; too long ships a file that appeared just after a start twice,
// which readers drop.
const startWindow = 15

// tailDBs are the tail categories' databases, as tailCategory makes them.
var tailDBs []string

func tailCategory(name, about string, t tail) category {
	tailDBs = append(tailDBs, t.db)
	return category{name: name, about: about, collect: func(b *backfill) (func() error, error) { return b.tail(name, t) }}
}

// tail ships each of the category's files from where the backfill stopped,
// and decides from the collector's database how far:
//   - not in the database (the collector has not run, or not found the file
//     yet): to the file's last whole line, and the next run goes on from
//     there;
//   - registered when the agent started (within startWindow after one):
//     the collector began at what was then the end, so to the collector's
//     offset now, which is past that; the lines from there to its offset are
//     shipped twice, and readers drop the second;
//   - registered later: the collector read it from its first line, so
//     nothing.
//
// Once the collector has the file, it is handed off, and later runs leave
// it alone until the collector registers it again (a new file at the path).
func (b *backfill) tail(name string, t tail) (func() error, error) {
	st := b.stats[name]
	statePath := filepath.Join(b.dir, name+".json")
	var prev tailState
	if err := loadJSON(statePath, &prev); err != nil {
		return nil, err
	}
	live, err := b.liveFiles(t.db)
	if err != nil {
		return nil, err
	}
	starts, err := b.starts()
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(b.home, t.glob))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	next := tailState{Files: map[string]fileState{}}
	var unfollowed, atEnd, fromStart, done int
	for _, p := range files {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		ino := inodeOf(fi)
		fs := prev.Files[p]
		if fs.Inode != ino {
			fs = fileState{Inode: ino}
		}
		upto := fs.Shipped
		lf, followed := live[p]
		switch {
		case followed && lf.Inode == ino && fs.HandedOff == lf.Created:
			done++
		case followed && lf.Inode == ino:
			if registeredAtStart(lf.Created, starts) {
				atEnd++
				upto = max(upto, lf.Offset)
			} else {
				fromStart++
			}
			fs.HandedOff = lf.Created
		default:
			unfollowed++
			if upto, err = lastLineEnd(p, fi.Size()); err != nil {
				return nil, err
			}
		}
		if upto > fs.Shipped {
			if err := b.tailLines(name, t, p, fs.Shipped, upto, fi.ModTime()); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			fs.Shipped = upto
		}
		next.Files[p] = fs
	}
	st.note("%d files: %d up to where the agent began, %d it read whole, %d it does not follow, %d handed off before", len(files), atEnd, fromStart, unfollowed, done)
	return func() error { return saveJSON(statePath, next) }, nil
}

func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

func registeredAtStart(created int64, starts []int64) bool {
	for _, s := range starts {
		if created >= s-2 && created <= s+startWindow {
			return true
		}
	}
	return false
}

// starts are the agent's starts: the times in agent-starts, which
// dev-feed-agent appends to, and the earliest registration in any of the
// tail databases, which is the agent's first start, whatever the file says.
func (b *backfill) starts() ([]int64, error) {
	if b.startTimes != nil {
		return b.startTimes, nil
	}
	starts := []int64{}
	data, err := os.ReadFile(filepath.Join(b.state, "agent-starts"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, l := range strings.Fields(string(data)) {
		if n, err := strconv.ParseInt(l, 10, 64); err == nil {
			starts = append(starts, n)
		}
	}
	var first int64
	for _, db := range tailDBs {
		live, err := b.liveFiles(db)
		if err != nil {
			return nil, err
		}
		for _, f := range live {
			if first == 0 || f.Created < first {
				first = f.Created
			}
		}
	}
	if first != 0 {
		starts = append(starts, first)
	}
	b.startTimes = starts
	return starts, nil
}

// liveFiles is a tail database's files, by path; none if the collector has
// never run. fluent-bit holds the database locked while it runs (db.locking),
// so this reads a copy: a copy taken while it writes can only be older,
// which ships fewer lines that the collector has shipped anyway.
func (b *backfill) liveFiles(db string) (map[string]liveFile, error) {
	if m, ok := b.live[db]; ok {
		return m, nil
	}
	src := filepath.Join(b.state, db)
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		b.live[db] = map[string]liveFile{}
		return b.live[db], nil
	}
	dir, err := os.MkdirTemp(b.tmp, "db-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		for _, suffix := range []string{"", "-wal"} {
			if err := copyFile(src+suffix, filepath.Join(dir, db+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		out, err := exec.Command("sqlite3", "-json", filepath.Join(dir, db),
			"SELECT name, offset, inode, created FROM in_tail_files").Output()
		if err != nil {
			lastErr = fmt.Errorf("reading a copy of %s: %v", src, err)
			time.Sleep(time.Second)
			continue
		}
		var rows []liveFile
		if len(bytes.TrimSpace(out)) > 0 {
			if err := json.Unmarshal(out, &rows); err != nil {
				return nil, fmt.Errorf("%s: %v", src, err)
			}
		}
		m := map[string]liveFile{}
		for _, r := range rows {
			m[r.Name] = r
		}
		b.live[db] = m
		return m, nil
	}
	return nil, lastErr
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// lastLineEnd is the offset after the file's last newline: a line still
// being written is the next run's.
func lastLineEnd(path string, size int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	for end := size; end > 0; {
		start := max(0, end-int64(len(buf)))
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}

// tailLines emits the lines of path in [from, upto) as the collector's rows:
// a line verbatim in json, the empty and the overlong ones left out as tail
// leaves them out. A line's time is its own field, or the nearest earlier
// line's that has one, or for the lines before the first that has one,
// that line's; a range without any is at the file's modification time.
func (b *backfill) tailLines(name string, t tail, path string, from, upto int64, mtime time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(io.LimitReader(f, upto-from), 1<<20)
	fields := map[string]any{"table": t.table, "path": path}
	if t.kind != "" {
		fields["kind"] = t.kind
	}
	if t.path != nil {
		if m := t.path.FindStringSubmatch(path); m != nil {
			for i, n := range t.path.SubexpNames() {
				if n != "" {
					fields[n] = m[i]
				}
			}
		}
	}
	var pending []string
	var last time.Time
	emit := func(ts time.Time, line string) error {
		row := make(map[string]any, len(fields)+5)
		for k, v := range fields {
			row[k] = v
		}
		row["json"] = line
		return b.emit(name, ts, row)
	}
	skipped := 0
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
			case len(line) > maxLine:
				skipped++
			default:
				if ts, ok := lineTime(line, t.time); ok {
					for _, p := range pending {
						if err := emit(ts, p); err != nil {
							return err
						}
					}
					pending, last = nil, ts
				}
				if last.IsZero() {
					pending = append(pending, line)
				} else if err := emit(last, line); err != nil {
					return err
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	for _, p := range pending {
		if err := emit(mtime, p); err != nil {
			return err
		}
	}
	if skipped > 0 {
		b.stats[name].note("%s: %d lines longer than %d MB left out, as the collector does", path, skipped, maxLine>>20)
	}
	return nil
}

// lineTime is a JSON line's field key: an RFC 3339 string or epoch
// milliseconds.
func lineTime(line, key string) (time.Time, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &m) != nil {
		return time.Time{}, false
	}
	v, ok := m[key]
	if !ok {
		return time.Time{}, false
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		t, err := time.Parse(time.RFC3339Nano, s)
		return t, err == nil
	}
	var ms float64
	if json.Unmarshal(v, &ms) == nil && ms > 0 {
		return time.UnixMilli(int64(ms)), true
	}
	return time.Time{}, false
}

// memory ships the memory files as dev-feed-snapshot does, while its
// collector has not run: a file's content when it is new or changed since
// the previous backfill, a row when it is gone. The collector's first run
// ships every file there is, so once it has run there is nothing to do.
func memory(b *backfill) (func() error, error) {
	st := b.stats["claude-memory"]
	if _, err := os.Stat(filepath.Join(b.state, "snapshot-claude-memory.tsv")); err == nil {
		st.note("handed off: the collector's first run shipped every memory there was")
		return nil, nil
	}
	statePath := filepath.Join(b.dir, "claude-memory.json")
	prev := map[string]string{}
	if err := loadJSON(statePath, &prev); err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(b.home, ".claude", "projects", "*", "memory", "*.md"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	project := regexp.MustCompile(`/projects/([^/]+)/memory/[^/]+$`)
	next := map[string]string{}
	for _, p := range files {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(content)
		sha := hex.EncodeToString(sum[:])
		next[p] = sha
		if prev[p] == sha {
			continue
		}
		row := map[string]any{"table": "claude", "kind": "memory", "path": p, "content": string(content), "sha256": sha, "deleted": false}
		if m := project.FindStringSubmatch(p); m != nil {
			row["project"] = m[1]
		}
		if err := b.emit("claude-memory", fi.ModTime(), row); err != nil {
			return nil, err
		}
	}
	for p := range prev {
		if _, ok := next[p]; ok {
			continue
		}
		row := map[string]any{"table": "claude", "kind": "memory", "path": p, "deleted": true}
		if m := project.FindStringSubmatch(p); m != nil {
			row["project"] = m[1]
		}
		if err := b.emit("claude-memory", b.now, row); err != nil {
			return nil, err
		}
	}
	return func() error { return saveJSON(statePath, next) }, nil
}

// command is a sibling of this binary, else one on PATH.
func (b *backfill) command(name string, args ...string) *exec.Cmd {
	path := filepath.Join(b.bin, name)
	if _, err := os.Stat(path); err != nil {
		path = name
	}
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), "HOST_STATE_DIR="+b.state, "HOME="+b.home)
	cmd.Stderr = os.Stderr
	return cmd
}

// lines runs cmd and hands fn each line it prints, as JSON.
func lines(cmd *exec.Cmd, fn func(map[string]any) error) error {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r := bufio.NewReaderSize(out, 1<<20)
	var ferr error
	for ferr == nil {
		l, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(l)) > 0 {
			d := json.NewDecoder(bytes.NewReader(l))
			d.UseNumber()
			var row map[string]any
			if ferr = d.Decode(&row); ferr == nil {
				ferr = fn(row)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				ferr = err
			}
			break
		}
	}
	if ferr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ferr
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s: %v", strings.Join(cmd.Args, " "), err)
	}
	return nil
}

// cmux ships the journal's rows from after the previous backfill: up to
// the collector's sequence once it has one (it began past where it first
// stood, so the rows from there are shipped twice, and readers drop the
// second by event_id), else up to the newest.
func cmux(b *backfill) (func() error, error) {
	st := b.stats["cmux"]
	statePath := filepath.Join(b.dir, "cmux.json")
	var s struct {
		Shipped   int64 `json:"shipped"`
		HandedOff bool  `json:"handed_off"`
	}
	if err := loadJSON(statePath, &s); err != nil {
		return nil, err
	}
	if s.HandedOff {
		st.note("handed off before")
		return nil, nil
	}
	args := []string{"rows", strconv.FormatInt(s.Shipped, 10)}
	if seq, err := os.ReadFile(filepath.Join(b.state, "cmux-journal.seq")); err == nil {
		upto, err := strconv.ParseInt(strings.TrimSpace(string(seq)), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cmux-journal.seq: %v", err)
		}
		args = append(args, strconv.FormatInt(upto, 10))
		s.HandedOff = true
		s.Shipped = max(s.Shipped, upto)
		st.note("up to the collector's sequence %d, then handed off", upto)
	}
	err := lines(b.command("dev-feed-cmux", args...), func(row map[string]any) error {
		if n, err := row["sequence"].(json.Number).Int64(); err == nil && n > s.Shipped {
			s.Shipped = n
		}
		ts, err := time.Parse(time.RFC3339Nano, fmt.Sprint(row["occurred_at"]))
		if err != nil {
			return fmt.Errorf("a row without occurred_at: %v", row)
		}
		return b.emit("cmux", ts, row)
	})
	if err != nil {
		return nil, err
	}
	return func() error { return saveJSON(statePath, s) }, nil
}

// gitHistory ships what dev-feed-git backfill prints, and moves its next
// states into place once shipped. A ref-update is at its reflog time, a
// commit at its committer time, a remote at the run's.
func gitHistory(b *backfill) (func() error, error) {
	dir := filepath.Join(b.dir, "git")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	stale, _ := filepath.Glob(filepath.Join(dir, "*.json.next"))
	for _, s := range stale {
		if err := os.Remove(s); err != nil {
			return nil, err
		}
	}
	repos := map[string]bool{}
	err := lines(b.command("dev-feed-git", "backfill", dir), func(row map[string]any) error {
		repos[fmt.Sprint(row["repo"])] = true
		ts := b.now
		for _, k := range []string{"reflog_time", "committer_time"} {
			if v, ok := row[k].(string); ok {
				t, err := time.Parse(time.RFC3339, v)
				if err != nil {
					return fmt.Errorf("%s %q: %v", k, v, err)
				}
				ts = t
				break
			}
		}
		return b.emit("git", ts, row)
	})
	if err != nil {
		return nil, err
	}
	b.stats["git"].note("%d repositories with history to ship", len(repos))
	return func() error {
		next, _ := filepath.Glob(filepath.Join(dir, "*.json.next"))
		for _, n := range next {
			if err := os.Rename(n, strings.TrimSuffix(n, ".next")); err != nil {
				return err
			}
		}
		return nil
	}, nil
}
