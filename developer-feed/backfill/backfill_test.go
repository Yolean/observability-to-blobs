package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// fakeBucket keeps what is PUT, by key.
type fakeBucket struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut || r.Header.Get("Content-MD5") == "" || !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		http.Error(w, "PutObject only, signed, with Content-MD5", http.StatusForbidden)
		return
	}
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.objects[strings.TrimPrefix(r.URL.Path, "/bucket/")] = b
	f.mu.Unlock()
}

// rows is every row in the objects, decompressed.
func (f *fakeBucket) rows(t *testing.T) []map[string]any {
	t.Helper()
	dec, _ := zstd.NewReader(nil)
	var rows []map[string]any
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw, err := dec.DecodeAll(f.objects[k], nil)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var r map[string]any
			if err := json.Unmarshal([]byte(l), &r); err != nil {
				t.Fatalf("%s: %q: %v", k, l, err)
			}
			r["_key"] = k
			rows = append(rows, r)
		}
	}
	return rows
}

type fixture struct {
	t      *testing.T
	s      settings
	bucket *fakeBucket
	srv    *httptest.Server
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	base := t.TempDir()
	f := &fixture{t: t, bucket: &fakeBucket{objects: map[string][]byte{}}, now: time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)}
	f.srv = httptest.NewServer(f.bucket)
	t.Cleanup(f.srv.Close)
	f.s = settings{home: filepath.Join(base, "home"), state: filepath.Join(base, "state"),
		bucket: "bucket", endpoint: f.srv.URL, region: "us-east-1", root: "dev1", cluster: "smack3", node: "smack-3"}
	for _, d := range []string{f.s.home, f.s.state} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// ship runs a backfill of the categories and returns the rows it put.
func (f *fixture) ship(names string) []map[string]any {
	f.t.Helper()
	cats, err := choose(names)
	if err != nil {
		f.t.Fatal(err)
	}
	f.bucket.objects = map[string][]byte{}
	f.now = f.now.Add(time.Minute)
	dest := newBucket(f.s.endpoint, f.s.bucket, f.s.region, credential{AccessKeyId: "k", SecretAccessKey: "s"})
	var out strings.Builder
	if err := backfillRun(f.s, cats, dest, "", f.now, &out); err != nil {
		f.t.Fatalf("%v\n%s", err, out.String())
	}
	f.t.Log(out.String())
	return f.bucket.rows(f.t)
}

func (f *fixture) write(rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.s.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) appendTo(p, content string) {
	f.t.Helper()
	w, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		f.t.Fatal(err)
	}
	w.Close()
}

// follow registers p in the collector's database as fluent-bit does: at
// offset, created at the given time.
func (f *fixture) follow(db, p string, offset int64, created time.Time) {
	f.t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		f.t.Fatal(err)
	}
	ino := inodeOf(fi)
	sql := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS in_tail_files (id INTEGER PRIMARY KEY, name TEXT NOT NULL, offset INTEGER,
		inode INTEGER, created INTEGER, rotated INTEGER DEFAULT 0, offset_marker INTEGER DEFAULT 0, offset_marker_size INTEGER DEFAULT 0);
		DELETE FROM in_tail_files WHERE name = '%s';
		INSERT INTO in_tail_files (name, offset, inode, created) VALUES ('%s', %d, %d, %d);`, p, p, offset, ino, created.Unix())
	if out, err := exec.Command("sqlite3", filepath.Join(f.s.state, db), sql).CombinedOutput(); err != nil {
		f.t.Fatalf("sqlite3: %v: %s", err, out)
	}
}

func (f *fixture) started(at ...time.Time) {
	var b strings.Builder
	for _, t := range at {
		fmt.Fprintf(&b, "%d\n", t.Unix())
	}
	if err := os.WriteFile(filepath.Join(f.s.state, "agent-starts"), []byte(b.String()), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func messages(rows []map[string]any) []string {
	var m []string
	for _, r := range rows {
		var line struct{ M string }
		_ = json.Unmarshal([]byte(fmt.Sprint(r["json"])), &line)
		m = append(m, line.M)
	}
	return m
}

func line(m, ts string) string {
	if ts == "" {
		return fmt.Sprintf(`{"type":"mode","m":%q}`+"\n", m)
	}
	return fmt.Sprintf(`{"type":"user","m":%q,"timestamp":%q}`+"\n", m, ts)
}

const session = "11111111-1111-4111-8111-111111111111"

// A transcript the collector does not follow: up to its last whole line,
// then on from there; rows as the claude collector makes them.
func TestTailUnfollowed(t *testing.T) {
	f := newFixture(t)
	p := f.write(".claude/projects/-Users-dev-repo/"+session+".jsonl",
		line("a", "")+line("b", "2026-09-01T10:00:00.000Z")+line("c", "")+`{"partial`)
	rows := f.ship("claude-transcript")
	if got := strings.Join(messages(rows), ","); got != "a,b,c" {
		t.Fatalf("lines %s, want a,b,c and not the partial one", got)
	}
	r := rows[0]
	want := map[string]any{"table": "claude", "kind": "transcript", "path": p, "project": "-Users-dev-repo", "session_id": session,
		"cluster": "smack3", "node": "smack-3", "ts": "2026-09-01T10:00:00.000000Z", "backfill": "20261024T200100Z"}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("%s = %v, want %v", k, r[k], v)
		}
	}
	if k := r["_key"].(string); !regexp.MustCompile(`^dev1/smack3/2026-09-01/10/smack-3/0000-[0-9A-Za-z]{8}\.jsonl\.zst$`).MatchString(k) {
		t.Errorf("key %s", k)
	}
	if again := f.ship("claude-transcript"); len(again) != 0 {
		t.Fatalf("again: %v", messages(again))
	}
	f.appendTo(p, `": 1}`+"\n"+line("d", "2026-09-02T11:00:00.000Z"))
	if got := strings.Join(messages(f.ship("claude-transcript")), ","); got != ",d" {
		t.Fatalf("after appending: %s, want the completed line and d", got)
	}
}

// Registered when the agent started: up to the collector's offset, once.
// Registered later: the collector read it whole, so nothing.
func TestTailHandOff(t *testing.T) {
	f := newFixture(t)
	start := time.Date(2026, 10, 24, 15, 49, 45, 0, time.UTC)
	f.started(start)
	old := f.write(".claude/projects/p/"+session+".jsonl", line("before-1", "2026-10-01T00:00:00Z")+line("before-2", "2026-10-01T00:00:01Z"))
	fi, _ := os.Stat(old)
	f.appendTo(old, line("after", "2026-10-24T16:00:00Z"))
	f.follow("claude-transcripts.db", old, fi.Size()+int64(len(line("after", "2026-10-24T16:00:00Z"))), start.Add(time.Second))
	later := f.write(".claude/projects/p/22222222-2222-4222-8222-222222222222.jsonl", line("new-session", "2026-10-24T17:00:00Z"))
	f.follow("claude-transcripts.db", later, 0, start.Add(time.Hour))

	if got := strings.Join(messages(f.ship("claude-transcript")), ","); got != "before-1,before-2,after" {
		t.Fatalf("hand-off: %s, want the file up to the collector's offset and not the session it read whole", got)
	}
	f.appendTo(old, line("more", "2026-10-24T18:00:00Z"))
	if again := f.ship("claude-transcript"); len(again) != 0 {
		t.Fatalf("after the hand-off: %v", messages(again))
	}
}

// Shipped before the collector ran, then registered at an agent start:
// from where the backfill stopped to the collector's offset.
func TestTailCollectorLater(t *testing.T) {
	f := newFixture(t)
	p := f.write("Reposoft/productivity-logs/hammerspoon-activity-log.jsonl", `{"ts":"2026-10-20T08:00:00.000Z","m":"one"}`+"\n")
	if got := strings.Join(messages(f.ship("hammerspoon")), ","); got != "one" {
		t.Fatalf("first: %s", got)
	}
	f.appendTo(p, `{"ts":"2026-10-21T08:00:00.000Z","m":"two"}`+"\n")
	start := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	f.started(start)
	fi, _ := os.Stat(p)
	f.appendTo(p, `{"ts":"2026-10-22T09:30:00.000Z","m":"three"}`+"\n")
	f.follow("hammerspoon.db", p, fi.Size(), start)
	rows := f.ship("hammerspoon")
	if got := strings.Join(messages(rows), ","); got != "two" {
		t.Fatalf("hand-off: %s, want two only", got)
	}
	if rows[0]["kind"] != nil || rows[0]["table"] != "hammerspoon" {
		t.Errorf("row %v: the hammerspoon collector's has no kind", rows[0])
	}
}

// A category added to a feed whose others are backfilled ships alone; the
// history's epoch milliseconds are its time.
func TestCategoryAlone(t *testing.T) {
	f := newFixture(t)
	f.write(".claude/projects/p/"+session+".jsonl", line("t", "2026-09-01T10:00:00Z"))
	f.ship("claude-transcript")
	f.write(".claude/history.jsonl", `{"display":"a prompt","timestamp":1785148817449}`+"\n")
	rows := f.ship("claude-transcript,claude-subagent,claude-history,claude-memory,hammerspoon")
	var kinds []string
	for _, r := range rows {
		if r["table"] == "claude" {
			kinds = append(kinds, fmt.Sprint(r["kind"]))
		}
	}
	if strings.Join(kinds, ",") != "history" {
		t.Fatalf("claude rows %v, want the history only", kinds)
	}
	for _, r := range rows {
		if r["kind"] == "history" && r["ts"] != "2026-07-27T10:40:17.449000Z" {
			t.Errorf("history ts %v", r["ts"])
		}
	}
}

// An object holds one hour, in the order of the lines' times, and is cut
// at maxObject.
func TestObjects(t *testing.T) {
	sp := newSpool(t.TempDir())
	at := func(s string) time.Time { tt, _ := time.Parse(time.RFC3339, s); return tt }
	for _, l := range []struct{ ts, line string }{
		{"2026-09-01T10:30:00Z", `{"n":2}`}, {"2026-09-01T10:05:00Z", `{"n":1}`}, {"2026-09-01T11:00:00Z", `{"n":3}`},
	} {
		if err := sp.add(at(l.ts), []byte(l.line)); err != nil {
			t.Fatal(err)
		}
	}
	dec, _ := zstd.NewReader(nil)
	var got []string
	err := sp.objects(func(o object) error {
		raw, err := dec.DecodeAll(o.body, nil)
		got = append(got, o.key[:strings.LastIndex(o.key, "-")]+" "+strings.ReplaceAll(strings.TrimSpace(string(raw)), "\n", " "))
		return err
	}, "dev1", "c", "n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`dev1/c/2026-09-01/10/n/0500 {"n":1} {"n":2}`, `dev1/c/2026-09-01/11/n/0000 {"n":3}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("objects\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
