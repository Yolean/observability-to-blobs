package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sh runs a git command in dir with a fixed identity, failing the test on error.
func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func run(t *testing.T, repo, state string) []row {
	t.Helper()
	var buf bytes.Buffer
	if err := collect(repo, state, &buf, time.Now()); err != nil {
		t.Fatal(err)
	}
	var rows []row
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var r row
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		rows = append(rows, r)
	}
	return rows
}

func kinds(rows []row) map[string]int {
	m := map[string]int{}
	for _, r := range rows {
		m[r.Kind]++
	}
	return m
}

// A clone of a bare origin, and another clone that pushes as someone else:
// the developer's clone sees its own push and, after a fetch, the other's.
func TestCollect(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	sh(t, base, "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(base, "seed")
	sh(t, base, "clone", "-q", origin, seed)
	sh(t, seed, "commit", "-q", "--allow-empty", "-m", "initial")
	sh(t, seed, "push", "-q", "origin", "main")

	repo := filepath.Join(base, "dev")
	sh(t, base, "clone", "-q", origin, repo)
	state := filepath.Join(base, "state")
	repo, _ = filepath.EvalSymlinks(repo)

	first := run(t, repo, state)
	if k := kinds(first); k["remote"] != 1 || len(first) != 1 {
		t.Fatalf("first run: %+v, want the remote only", first)
	}
	if r := first[0]; r.Remote != "origin" || r.FetchURL != origin || r.PushURL != origin || r.Repo != repo || r.Table != "git" {
		t.Errorf("remote row %+v", r)
	}
	if again := run(t, repo, state); len(again) != 0 {
		t.Fatalf("nothing happened, yet: %+v", again)
	}

	msg := "feature: the requirement\n\nWhy it was asked for.\n\nCo-Authored-By: Claude <noreply@anthropic.com>"
	sh(t, repo, "commit", "-q", "--allow-empty", "-m", msg)
	sha := sh(t, repo, "rev-parse", "HEAD")
	sh(t, repo, "push", "-q", "origin", "main")
	sh(t, seed, "pull", "-q", "origin", "main")
	sh(t, seed, "commit", "-q", "--allow-empty", "-m", "someone else's")
	other := sh(t, seed, "rev-parse", "HEAD")
	sh(t, seed, "push", "-q", "origin", "main")
	sh(t, repo, "fetch", "-q", "origin")

	rows := run(t, repo, state)
	var commits, updates []row
	for _, r := range rows {
		switch r.Kind {
		case "commit":
			commits = append(commits, r)
		case "ref-update":
			updates = append(updates, r)
		}
	}
	if len(commits) != 2 || commits[0].Sha != sha || commits[1].Sha != other {
		t.Fatalf("commits %+v, want %s then %s", commits, sha, other)
	}
	if commits[0].Message != msg || len(commits[0].Parents) != 1 || commits[0].AuthorEmail != "dev@example.com" || commits[0].CommitterTime == "" {
		t.Errorf("commit row %+v", commits[0])
	}
	var push, fetch, commit bool
	for _, u := range updates {
		switch {
		case u.Ref == "refs/remotes/origin/main" && u.Sha == sha && u.Message == "update by push":
			push = true
		case u.Ref == "refs/remotes/origin/main" && u.Sha == other && strings.HasPrefix(u.Message, "fetch"):
			fetch = true
		case u.Ref == "refs/heads/main" && u.Sha == sha && strings.HasPrefix(u.Message, "commit"):
			commit = true
		}
		if _, err := time.Parse(time.RFC3339, u.ReflogTime); err != nil {
			t.Errorf("ref-update at %q", u.ReflogTime)
		}
	}
	if !push || !fetch || !commit {
		t.Errorf("ref-updates %+v: push %v, fetch %v, commit %v", updates, push, fetch, commit)
	}
	if again := run(t, repo, state); len(again) != 0 {
		t.Fatalf("the same again: %+v", again)
	}

	sh(t, repo, "remote", "set-url", "--push", "origin", "git@example.com:o/r.git")
	if r := run(t, repo, state); len(r) != 1 || r[0].Kind != "remote" || r[0].PushURL != "git@example.com:o/r.git" {
		t.Errorf("a changed push URL: %+v", r)
	}
}

func TestDiscover(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	for _, d := range []string{a, b, filepath.Join(a, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sh(t, a, "init", "-q")
	sh(t, b, "init", "-q")
	home := filepath.Join(base, "home")
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	docs := filepath.Join(home, "Documents", "repo")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	sh(t, docs, "init", "-q")
	hist := `{"display":"x","project":"` + filepath.Join(a, "sub") + `"}` + "\n" +
		`{"display":"y","project":"` + filepath.Join(base, "not-a-repo") + `"}` + "\n" +
		`{"display":"z","project":"` + docs + `"}` + "\n" + "not json\n"
	os.WriteFile(filepath.Join(home, ".claude", "history.jsonl"), []byte(hist), 0o644)
	got := discover(home, filepath.Join(base, "b*")+":"+a)
	if strings.Join(got, ",") != a+","+b {
		t.Errorf("discover: %v, want %s and %s once each, and not the history's %s", got, a, b, docs)
	}
	if got := discover(home, docs); len(got) != 2 || got[0] != a || got[1] != docs {
		t.Errorf("discover with %s in the roots: %v", docs, got)
	}
}

// commit backfill's next states, as dev-feed-backfill does after an upload
func commitNext(t *testing.T, dir string) {
	t.Helper()
	next, _ := filepath.Glob(filepath.Join(dir, "*.json.next"))
	for _, n := range next {
		if err := os.Rename(n, strings.TrimSuffix(n, ".next")); err != nil {
			t.Fatal(err)
		}
	}
}

func runBackfill(t *testing.T, repo, dir, live string) []row {
	t.Helper()
	var buf bytes.Buffer
	if err := backfill(repo, dir, live, &buf); err != nil {
		t.Fatal(err)
	}
	commitNext(t, dir)
	var rows []row
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var r row
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		rows = append(rows, r)
	}
	return rows
}

func messages(rows []row, kind string) []string {
	var m []string
	for _, r := range rows {
		if r.Kind == kind {
			m = append(m, r.Message)
		}
	}
	return m
}

// Before the collector has run: the history and the remotes, then from
// where the previous backfill stopped. After its first run: up to now once,
// then nothing.
func TestBackfill(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	sh(t, base, "init", "-q", "--bare", "-b", "main", origin)
	repo := filepath.Join(base, "dev")
	sh(t, base, "clone", "-q", origin, repo)
	repo, _ = filepath.EvalSymlinks(repo)
	sh(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	sh(t, repo, "commit", "-q", "--allow-empty", "-m", "two")
	sh(t, repo, "push", "-q", "origin", "main")
	dir, live := filepath.Join(base, "backfill"), filepath.Join(base, "live")

	first := runBackfill(t, repo, dir, live)
	if k := kinds(first); k["remote"] != 1 || k["commit"] != 2 || k["ref-update"] != 3 {
		t.Fatalf("first backfill: %v, want the remote, both commits, and their two entries and the push", k)
	}
	if got := messages(first, "commit"); strings.Join(got, ",") != "one,two" {
		t.Errorf("commits %v, want one,two, oldest first", got)
	}
	if again := runBackfill(t, repo, dir, live); len(again) != 0 {
		t.Fatalf("nothing happened, yet: %+v", again)
	}

	sh(t, repo, "commit", "-q", "--allow-empty", "-m", "three")
	more := runBackfill(t, repo, dir, live)
	if k := kinds(more); k["commit"] != 1 || k["ref-update"] != 1 || len(more) != 2 {
		t.Fatalf("after a commit: %+v, want it and its reflog entry", more)
	}

	// the collector's first run, then a push it ships itself
	run(t, repo, live)
	sh(t, repo, "commit", "-q", "--allow-empty", "-m", "four")
	sh(t, repo, "push", "-q", "origin", "main")
	if live := run(t, repo, live); len(messages(live, "commit")) != 1 {
		t.Fatalf("the collector: %+v, want commit four", live)
	}
	handoff := runBackfill(t, repo, dir, live)
	if k := kinds(handoff); k["remote"] != 0 || k["commit"] != 1 || k["ref-update"] != 2 {
		t.Fatalf("the hand-off: %v, want no remote (the collector shipped it), commit four, its commit and its push", k)
	}
	sh(t, repo, "commit", "-q", "--allow-empty", "-m", "five")
	if after := runBackfill(t, repo, dir, live); len(after) != 0 {
		t.Fatalf("after the hand-off: %+v, want nothing", after)
	}
}
