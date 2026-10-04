package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The logs API over fixture tiers: raw objects written as the agent writes
// them, folded (and rolled up) by the compactor, plus unfolded raw objects
// of the current and the previous hour, served through the API's real
// mux, so huma's parameter parsing and error bodies are part of every
// test.

var (
	lh10 = day.Add(10 * time.Hour)
	lh11 = day.Add(11 * time.Hour)
	lh12 = day.Add(12 * time.Hour)
	lnow = lh12.Add(20 * time.Minute)
)

// fixLine is one line a fixture pod logs.
type fixLine struct {
	t         time.Time
	ns, pod   string
	uid       string
	container string
	restart   int
	msg       string
}

func (l fixLine) raw() line {
	return logLine(l.t, l.ns, l.pod, l.uid, l.container, l.restart, l.msg)
}

// logsFixture is what a world holds and what the API should answer.
type logsFixture struct {
	lines []fixLine
}

// of returns the lines of an incarnation, and of one container unless
// container is "", in the API's order: time, then container, restart,
// message.
func (f *logsFixture) of(uid, container string) []fixLine {
	var out []fixLine
	for _, l := range f.lines {
		if l.uid == uid && (container == "" || l.container == container) {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.t.Equal(b.t) {
			return a.t.Before(b.t)
		}
		if a.container != b.container {
			return a.container < b.container
		}
		if a.restart != b.restart {
			return a.restart < b.restart
		}
		return a.msg < b.msg
	})
	return out
}

// body is the API's text for lines.
func body(lines []fixLine, pod string, timestamps, prefix bool) string {
	var b strings.Builder
	for _, l := range lines {
		if prefix {
			b.WriteString("[pod/" + pod + "/" + l.container + "] ")
		}
		if timestamps {
			b.WriteString(l.t.UTC().Format(criLayout) + " ")
		}
		b.WriteString(l.msg + "\n")
	}
	return b.String()
}

// putLines writes the lines that fall in hour h as one raw object, with
// extra lines (podindex) appended.
func (f *logsFixture) put(w *world, h time.Time, node, name string, lines []fixLine, extra ...line) string {
	var raw []line
	for _, l := range lines {
		raw = append(raw, l.raw())
	}
	f.lines = append(f.lines, lines...)
	return w.put(rawKey(h, node, name), append(raw, extra...)...)
}

func between(lines []fixLine, from, to time.Time) []fixLine {
	var out []fixLine
	for _, l := range lines {
		if !l.t.Before(from) && l.t.Before(to) {
			out = append(out, l)
		}
	}
	return out
}

// webLines: pod ns1/web, incarnation w1, with containers init, app and
// side. app logs 150 lines, 30 s apart from 10:00:05, restarting at
// 10:40; every tenth line carries ANSI colour; side logs at the same
// instants as some of app's; init logs three lines first.
func webLines() []fixLine {
	var out []fixLine
	for i := 0; i < 3; i++ {
		out = append(out, fixLine{lh10.Add(time.Duration(i) * 100 * time.Millisecond), "ns1", "web", "w1", "init", 0, fmt.Sprintf("init line %d", i)})
	}
	for i := 0; i < 150; i++ {
		t := lh10.Add(5*time.Second + time.Duration(i)*30*time.Second)
		restart := 0
		if t.After(lh10.Add(40 * time.Minute)) {
			restart = 1
		}
		msg := fmt.Sprintf("app line %03d", i)
		if i%10 == 0 {
			msg = "\x1b[31mERROR\x1b[0m " + msg
		}
		out = append(out, fixLine{t, "ns1", "web", "w1", "app", restart, msg})
		if i%25 == 0 {
			out = append(out, fixLine{t, "ns1", "web", "w1", "side", 0, fmt.Sprintf("side line %03d", i)})
		}
	}
	return out
}

// newLogsWorld fills w: web (running), job (two incarnations: j1 exits 0
// and goes in hour 10, j2 exits 1 and goes in hour 11), quiet (no lines),
// all folded in full at lnow, and then raw objects the sidecars lack: one
// of hour 11, a late upload whose lines interleave the folded ones and
// repeat one of them, and one of hour 12, the current hour.
func newLogsFixture(w *world) *logsFixture {
	f := &logsFixture{}
	web := webLines()
	f.put(w, lh10, "n1", "0000-web", between(web, lh10, lh11),
		podLine("pod-observed", lh10, "ns1", "web", "w1"),
		containerLine(lh10, "ns1", "web", "w1", "init", 0),
		containerLine(lh10.Add(time.Second), "ns1", "web", "w1", "app", 0),
		containerLine(lh10.Add(2*time.Second), "ns1", "web", "w1", "side", 0),
		containerLine(lh10.Add(40*time.Minute), "ns1", "web", "w1", "app", 1))
	f.put(w, lh11, "n1", "0000-web", between(web, lh11, lh12), podLine("pod-checkpoint", lh11, "ns1", "web", "w1"))
	j1 := []fixLine{
		{lh10.Add(5 * time.Minute), "ns1", "job", "j1", "job", 0, "job one starts"},
		{lh10.Add(6 * time.Minute), "ns1", "job", "j1", "job", 0, "job one done"},
	}
	f.put(w, lh10, "n1", "0500-job", j1,
		podLine("pod-observed", lh10.Add(5*time.Minute), "ns1", "job", "j1"),
		containerLine(lh10.Add(5*time.Minute), "ns1", "job", "j1", "job", 0),
		exitLine(lh10.Add(6*time.Minute), "ns1", "job", "j1", "job", 0, 0),
		podLine("pod-deleted", lh10.Add(10*time.Minute), "ns1", "job", "j1"))
	j2 := []fixLine{
		{lh11.Add(5 * time.Minute), "ns1", "job", "j2", "job", 0, "job two starts"},
		{lh11.Add(6 * time.Minute), "ns1", "job", "j2", "job", 0, "job two fails"},
	}
	f.put(w, lh11, "n1", "0500-job", j2,
		podLine("pod-observed", lh11.Add(5*time.Minute), "ns1", "job", "j2"),
		containerLine(lh11.Add(5*time.Minute), "ns1", "job", "j2", "job", 0),
		exitLine(lh11.Add(6*time.Minute), "ns1", "job", "j2", "job", 0, 1),
		podLine("pod-deleted", lh11.Add(10*time.Minute), "ns1", "job", "j2"))
	w.put(rawKey(lh10, "n1", "0900-quiet"), podLine("pod-observed", lh10, "ns1", "quiet", "q1"),
		containerLine(lh10, "ns1", "quiet", "q1", "app", 0))
	// another namespace's pod of the same name, which must not mix in
	f.put(w, lh10, "n2", "0000-web", []fixLine{{lh10.Add(time.Minute), "ns2", "web", "x1", "app", 0, "other namespace"}},
		podLine("pod-observed", lh10, "ns2", "web", "x1"))
	w.fold(lh10, lnow)
	w.fold(lh11, lnow)

	// unfolded: a late key of hour 11, between the folded lines, with one
	// line that the folded objects have too (a retried upload)
	late := []fixLine{
		{lh11.Add(10*time.Minute + 7*time.Second), "ns1", "web", "w1", "app", 1, "late upload line"},
		{lh11.Add(20*time.Minute + 3*time.Second), "ns1", "web", "w1", "side", 0, "late side line"},
	}
	f.put(w, lh11, "n2", "5900-late", late, between(web, lh11, lh12)[0].raw())
	cur := []fixLine{
		{lh12.Add(time.Minute), "ns1", "web", "w1", "app", 1, "current hour line 1"},
		{lh12.Add(2 * time.Minute), "ns1", "web", "w1", "app", 1, "\x1b[1;33mWARN\x1b[0m current hour line 2"},
	}
	f.put(w, lh12, "n1", "0100-web", cur)
	return f
}

// logsAPI is the API over a world, its clock at now.
type logsAPI struct {
	t   *testing.T
	r   *logsReader
	h   *logsHandler
	srv *httptest.Server
}

func serveLogs(t *testing.T, w *world, now time.Time, lookback time.Duration) *logsAPI {
	t.Helper()
	db, err := open("", initStmt{sql: "SET force_mbedtls_unsafe = true"}, initStmt{sql: "SET TimeZone = 'UTC'"})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &logsReader{l: w.c.l, tiers: w.c.tiers, raw: w.c.raw, db: db, keysDir: w.c.keysDir, lookback: lookback,
		now: func() time.Time { return now }, log: log}
	r.init()
	if err := r.loadKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := newLogsHandler(r, 30*time.Second, 4, log)
	mux := http.NewServeMux()
	registerLogs(newLogsAPI(mux), h)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); db.Close() })
	return &logsAPI{t: t, r: r, h: h, srv: srv}
}

type answer struct {
	status int
	header http.Header
	body   string
}

func (a *logsAPI) get(path string) answer {
	a.t.Helper()
	res, err := http.Get(a.srv.URL + path)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	return answer{res.StatusCode, res.Header, string(b)}
}

// ok fails unless the answer is a 200 of the plain text body, with the
// headers of lines.
func (a answer) ok(t *testing.T, what, wantBody string, phase PodPhase, tail TailEffect) {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("%s: %d %s", what, a.status, a.body)
	}
	if a.body != wantBody {
		t.Fatalf("%s: body\n%q\nwant\n%q", what, a.body, wantBody)
	}
	want := map[string]string{"Content-Type": textPlain, "X-Logs-Source": "blob", "X-Logs-Pod-Phase": string(phase),
		"X-Logs-Tail-Effect": string(tail)}
	for k, v := range want {
		if got := a.header.Get(k); got != v {
			t.Fatalf("%s: %s %q, want %q", what, k, got, v)
		}
	}
}

// problem fails unless the answer is huma's problem JSON with status and
// detail.
func (a answer) problem(t *testing.T, what string, status int, detail string) {
	t.Helper()
	if a.status != status {
		t.Fatalf("%s: %d %s, want %d", what, a.status, a.body, status)
	}
	if ct := a.header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("%s: Content-Type %q", what, ct)
	}
	var p struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(a.body), &p); err != nil {
		t.Fatalf("%s: %v: %s", what, err, a.body)
	}
	if p.Status != status || p.Title != http.StatusText(status) || (detail != "" && p.Detail != detail) {
		t.Fatalf("%s: problem %+v, want status %d detail %q", what, p, status, detail)
	}
}

func last(lines []fixLine, n int) []fixLine {
	if n > 0 && len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

func strip(lines []fixLine) []fixLine {
	out := make([]fixLine, len(lines))
	for i, l := range lines {
		l.msg = removeAnsi(l.msg)
		out[i] = l
	}
	return out
}

func grepped(lines []fixLine, grep string) []fixLine {
	var out []fixLine
	for _, l := range lines {
		if matchGrep(l.msg, grep) {
			out = append(out, l)
		}
	}
	return out
}

// Both routes and every query parameter, over the hourly tier and the
// unfolded raw of the previous and the current hour.
func TestLogsRoutesAndParameters(t *testing.T) {
	w := newWorld(t)
	f := newLogsFixture(w)
	api := serveLogs(t, w, lnow, 48*time.Hour)
	app, all := f.of("w1", "app"), f.of("w1", "")
	if len(app) <= 100 {
		t.Fatalf("fixture: %d app lines, want more than the default tail", len(app))
	}

	// defaults: tail 100, no timestamps, no prefix
	a := api.get("/api/v1/logs/n/ns1/web/app")
	a.ok(t, "app, defaults", body(last(app, 100), "web", false, false), PodPhaseRunning, TailEffectCap)
	if got, want := a.header.Get("X-Logs-Timestamp-End"), app[len(app)-1].t.Format(criLayout); got != want {
		t.Fatalf("X-Logs-Timestamp-End %q, want %q", got, want)
	}
	if !strings.HasSuffix(a.body, "current hour line 2\n") || !strings.Contains(a.body, "late upload line\n") {
		t.Fatalf("the unfolded raw lines are missing:\n%s", a.body)
	}
	// every restart of the container, in time order, the hourly tier and
	// the raw interleaved, and the line that both have once
	api.get("/api/v1/logs/n/ns1/web/app?tail=0&timestamps=true").
		ok(t, "app, all", body(app, "web", true, false), PodPhaseRunning, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/web/app?tail=-1").
		ok(t, "app, negative tail", body(app, "web", false, false), PodPhaseRunning, TailEffectAll)
	api.get(fmt.Sprintf("/api/v1/logs/n/ns1/web/app?tail=%d", len(app))).
		ok(t, "app, tail = all lines", body(app, "web", false, false), PodPhaseRunning, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/web/app?tail=5&timestamps=true&prefix=true").
		ok(t, "app, tail 5, timestamps, prefix", body(last(app, 5), "web", true, true), PodPhaseRunning, TailEffectCap)

	// all containers, merged by timestamp; side's lines at app's instants
	// come after app's
	api.get("/api/v1/logs/n/ns1/web?tail=0&prefix=true&timestamps=true").
		ok(t, "all containers", body(all, "web", true, true), PodPhaseRunning, TailEffectAll)
	a = api.get("/api/v1/logs/n/ns1/web?tail=7")
	a.ok(t, "all containers, tail 7", body(last(all, 7), "web", false, false), PodPhaseRunning, TailEffectCap)
	if !strings.HasPrefix(api.get("/api/v1/logs/n/ns1/web?tail=0&prefix=true").body, "[pod/web/init] init line 0\n") {
		t.Fatal("all containers: the init container's first line does not come first")
	}

	// grep: substring, anchored, then tail; and with ansi=strip, which
	// strips before the grep
	api.get("/api/v1/logs/n/ns1/web/app?grep=line%2001&tail=0").
		ok(t, "grep", body(grepped(app, "line 01"), "web", false, false), PodPhaseRunning, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/web/app?grep=ERROR&tail=3").
		ok(t, "grep, tail 3", body(last(grepped(app, "ERROR"), 3), "web", false, false), PodPhaseRunning, TailEffectCap)
	if a := api.get("/api/v1/logs/n/ns1/web/app?grep=%5EERROR&tail=0"); a.body != "" {
		t.Fatalf("^ERROR without ansi=strip matched lines that start with an escape:\n%s", a.body)
	}
	api.get("/api/v1/logs/n/ns1/web/app?grep=%5EERROR&tail=0&ansi=strip").
		ok(t, "^ERROR, ansi=strip", body(grepped(strip(app), "^ERROR"), "web", false, false), PodPhaseRunning, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/web/app?grep=%5EWARN&ansi=strip").
		ok(t, "^WARN, ansi=strip, from the current hour's raw", "WARN current hour line 2\n", PodPhaseRunning, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/web?ansi=strip&tail=0").
		ok(t, "ansi=strip", body(strip(all), "web", false, false), PodPhaseRunning, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/web?ansi=keep&tail=0").
		ok(t, "ansi other than strip", body(all, "web", false, false), PodPhaseRunning, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/web/app?grep=%5E&tail=2").
		ok(t, "grep ^ alone", body(last(app, 2), "web", false, false), PodPhaseRunning, TailEffectCap)
	a = api.get("/api/v1/logs/n/ns1/web/app?grep=no-such-text")
	a.ok(t, "grep without matches", "", PodPhaseRunning, TailEffectAll)
	if _, ok := a.header["X-Logs-Timestamp-End"]; ok {
		t.Fatal("X-Logs-Timestamp-End on an answer without lines")
	}

	// source: blob or none; k8s is a 400, as this server has no live
	// source; another value fails huma's enum check, a 422
	api.get("/api/v1/logs/n/ns1/web/app?source=blob&tail=1").
		ok(t, "source=blob", body(last(app, 1), "web", false, false), PodPhaseRunning, TailEffectCap)
	api.get("/api/v1/logs/n/ns1/web?source=blob&tail=1").
		ok(t, "source=blob, all containers", body(last(all, 1), "web", false, false), PodPhaseRunning, TailEffectCap)
	api.get("/api/v1/logs/n/ns1/web/app?source=k8s").
		problem(t, "source=k8s", http.StatusBadRequest, "invalid source value: this server has no k8s source, use 'blob' or omit it")
	api.get("/api/v1/logs/n/ns1/web/app?source=magic").problem(t, "source=magic", http.StatusUnprocessableEntity, "")

	// the other namespace's pod of the same name
	api.get("/api/v1/logs/n/ns2/web").ok(t, "ns2/web", "other namespace\n", PodPhaseRunning, TailEffectAll)
}

// A pod name used by two incarnations resolves to the one that started
// last, with its own phase; a pod with no lines answers 200 and no body.
func TestLogsReusedNameAndEmptyPod(t *testing.T) {
	w := newWorld(t)
	f := newLogsFixture(w)
	api := serveLogs(t, w, lnow, 48*time.Hour)
	api.get("/api/v1/logs/n/ns1/job?timestamps=true").
		ok(t, "job", body(f.of("j2", ""), "job", true, false), PodPhaseFailed, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/job/job").ok(t, "job/job", "job two starts\njob two fails\n", PodPhaseFailed, TailEffectAll)
	// before j2 started, j1 is the latest, and it went with exit 0
	api1 := serveLogs(t, w, lh10.Add(30*time.Minute), 48*time.Hour)
	api1.get("/api/v1/logs/n/ns1/job").ok(t, "job before j2", "job one starts\njob one done\n", PodPhaseSucceeded, TailEffectAll)

	a := api.get("/api/v1/logs/n/ns1/quiet/app")
	a.ok(t, "quiet/app", "", PodPhaseRunning, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/quiet").ok(t, "quiet", "", PodPhaseRunning, TailEffectAll)
}

// The errors, their status codes and their problem JSON bodies.
func TestLogsErrors(t *testing.T) {
	w := newWorld(t)
	newLogsFixture(w)
	api := serveLogs(t, w, lnow, 48*time.Hour)
	for _, p := range []string{"/api/v1/logs/n/ns1/nonexistent-pod", "/api/v1/logs/n/ns1/nonexistent-pod/app",
		"/api/v1/logs/n/ns3/web"} {
		ns, pod, _ := strings.Cut(strings.TrimPrefix(p, "/api/v1/logs/n/"), "/")
		pod, _, _ = strings.Cut(pod, "/")
		api.get(p).problem(t, p, http.StatusNotFound, "no archived logs for "+ns+"/"+pod)
	}
	api.get("/api/v1/logs/n/ns1/web/nonexistent").problem(t, "unknown container", http.StatusNotFound,
		`container "nonexistent" not found in pod ns1/web, known containers: [init, app, side]`)
	api.get("/api/v1/logs/n/ns1/job/app").problem(t, "a container of the latest incarnation only", http.StatusNotFound,
		`container "app" not found in pod ns1/job, known containers: [job]`)

	// a listing that fails
	api.r.tiers = failingList{w.c.tiers}
	a := api.get("/api/v1/logs/n/ns1/web")
	a.problem(t, "list failure", http.StatusInternalServerError, "")
	if !strings.Contains(a.body, "blob list failed: ") {
		t.Fatalf("list failure: %s", a.body)
	}
	api.r.tiers = w.c.tiers

	// an object that does not read
	if err := os.WriteFile(filepath.Join(w.dir, filepath.FromSlash(w.obj(w.c.l.hourlyStem("logs", lh11)).key)), []byte("not parquet"), 0o644); err != nil {
		t.Fatal(err)
	}
	a = api.get("/api/v1/logs/n/ns1/web")
	a.problem(t, "read failure", http.StatusInternalServerError, "")
	if !strings.Contains(a.body, "blob read failed: ") {
		t.Fatalf("read failure: %s", a.body)
	}
}

type failingList struct{ TierStore }

func (failingList) List(context.Context, string) ([]Object, error) {
	return nil, errors.New("network timeout")
}

// Encrypted tiers under two keys, a rotation between the hours, read per
// key: the same answers as plain tiers. A key missing from the directory
// is a 500 that names it.
func TestLogsEncrypted(t *testing.T) {
	plain := newWorld(t)
	f := newLogsFixture(plain)
	enc := newKeyedWorld(t, "k1")
	// the same objects, the rotation after hour 10's fold
	encFixture := &logsFixture{}
	web := webLines()
	encFixture.put(enc, lh10, "n1", "0000-web", between(web, lh10, lh11),
		podLine("pod-observed", lh10, "ns1", "web", "w1"),
		containerLine(lh10, "ns1", "web", "w1", "init", 0),
		containerLine(lh10.Add(time.Second), "ns1", "web", "w1", "app", 0),
		containerLine(lh10.Add(2*time.Second), "ns1", "web", "w1", "side", 0))
	enc.fold(lh10, lnow)
	enc.encrypt("k2", "k1")
	encFixture.put(enc, lh11, "n1", "0000-web", between(web, lh11, lh12), podLine("pod-checkpoint", lh11, "ns1", "web", "w1"))
	enc.fold(lh11, lnow)
	if k10, k11 := enc.keyOf(enc.c.l.hourlyStem("logs", lh10)), enc.keyOf(enc.c.l.hourlyStem("logs", lh11)); k10 != "k1" || k11 != "k2" {
		t.Fatalf("hourly logs keys %s, %s; want k1, k2", k10, k11)
	}
	api := serveLogs(t, enc, lnow, 48*time.Hour)
	foldedWeb := between(f.of("w1", ""), lh10, lh12)
	var want []fixLine
	for _, l := range foldedWeb {
		if l.msg != "late upload line" && l.msg != "late side line" {
			want = append(want, l)
		}
	}
	api.get("/api/v1/logs/n/ns1/web?tail=0&timestamps=true&prefix=true").
		ok(t, "encrypted", body(want, "web", true, true), PodPhaseRunning, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/web/app?grep=ERROR&ansi=strip&tail=4").
		ok(t, "encrypted, grep", body(last(grepped(strip(encFixture.of("w1", "app")), "ERROR"), 4), "web", false, false),
			PodPhaseRunning, TailEffectCap)

	// k1's file gone: the reader keeps keys it loaded, as the compactor
	// does, so a new reader shows the refusal
	if err := os.Remove(filepath.Join(enc.keysDir, "k1")); err != nil {
		t.Fatal(err)
	}
	api = serveLogs(t, enc, lnow, 48*time.Hour)
	a := api.get("/api/v1/logs/n/ns1/web")
	a.problem(t, "missing key", http.StatusInternalServerError, "")
	if !strings.Contains(a.body, "parquet key k1, which PARQUET_KEYS_DIR lacks") {
		t.Fatalf("missing key: %s", a.body)
	}
}

// A rolled-up day is read from the daily tier: the same lines; the phase
// is Archived, since nothing more recent than the daily tier has the pod.
func TestLogsDailyTier(t *testing.T) {
	w := newWorld(t)
	f := newLogsFixture(w)
	// fold the unfolded objects too, and roll the day up
	w.fold(lh11, lnow)
	w.fold(lh12, lh12.Add(90*time.Minute))
	w.rollup(day)
	next := day.Add(26 * time.Hour)
	api := serveLogs(t, w, next, 48*time.Hour)
	if p, err := api.r.plan(context.Background(), mustConn(t, api.r)); err != nil || len(p.dailyDays) != 1 || len(p.hourlyLogs) != 0 {
		t.Fatalf("plan: %+v %v; want the day from the daily tier", p, err)
	}
	api.get("/api/v1/logs/n/ns1/web?tail=0&timestamps=true&prefix=true").
		ok(t, "daily", body(f.of("w1", ""), "web", true, true), PodPhaseArchived, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/job").ok(t, "daily job", "job two starts\njob two fails\n", PodPhaseArchived, TailEffectAll)
	api.get("/api/v1/logs/n/ns1/web/nope").problem(t, "daily, unknown container", http.StatusNotFound,
		`container "nope" not found in pod ns1/web, known containers: [init, app, side]`)

	// a lookback that ends before the day: nothing
	api = serveLogs(t, w, day.Add(72*time.Hour+time.Minute), 48*time.Hour)
	api.get("/api/v1/logs/n/ns1/web").problem(t, "out of the window", http.StatusNotFound, "no archived logs for ns1/web")
}

func mustConn(t *testing.T, r *logsReader) *sql.Conn {
	t.Helper()
	c, err := r.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// The window bounds the lines: a lookback of an hour from 12:20 returns
// the lines from 11:20 on, and a pod with no activity in it is not found.
func TestLogsLookback(t *testing.T) {
	w := newWorld(t)
	f := newLogsFixture(w)
	api := serveLogs(t, w, lnow, time.Hour)
	from := lnow.Add(-time.Hour)
	var want []fixLine
	for _, l := range f.of("w1", "app") {
		if !l.t.Before(from) {
			want = append(want, l)
		}
	}
	api.get("/api/v1/logs/n/ns1/web/app?tail=0&timestamps=true").
		ok(t, "an hour", body(want, "web", true, false), PodPhaseRunning, TailEffectAll)
	// j1 and j2 both went before 11:20, but j2's pods row of hour 11 is
	// in the window's hours: its activity (deleted 11:10) is not
	api.get("/api/v1/logs/n/ns1/job").problem(t, "job before the window", http.StatusNotFound, "no archived logs for ns1/job")
}

// Spool mode: the run process's API reads the spool's unfolded files.
func TestLogsSpool(t *testing.T) {
	sw := newSpoolWorld(t)
	web := webLines()
	sw.upload(rawKey(lh11, "n1", "0000-web"), append(rawLines(between(web, lh11, lh12)),
		podLine("pod-observed", lh10, "ns1", "web", "w1"))...)
	sw.fold(lh11, lnow)
	cur := []fixLine{{lh12.Add(time.Minute), "ns1", "web", "w1", "app", 1, "spooled, not folded"}}
	sw.upload(rawKey(lh12, "n1", "0100-web"), rawLines(cur)...)
	api := serveLogs(t, sw.world, lnow, 48*time.Hour)
	want := append(between(web, lh11, lh12), cur...)
	f := &logsFixture{lines: want}
	api.get("/api/v1/logs/n/ns1/web?tail=0").ok(t, "spool", body(f.of("w1", ""), "web", false, false), PodPhaseRunning, TailEffectAll)
}

func rawLines(lines []fixLine) []line {
	out := make([]line, len(lines))
	for i, l := range lines {
		out[i] = l.raw()
	}
	return out
}

// The engine's grep and ANSI stripping, which narrow the rows before
// formatOutput, give removeAnsi's and matchGrep's results.
func TestGrepSQLMatchesGo(t *testing.T) {
	db, err := open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	alphabet := []string{"\x1b", "[", "0", "3", ";", "m", "K", "A", "~", "@", "?", " ", "x", "é", "\x7f", "^", "/", "!"}
	rnd := rand.New(rand.NewSource(1))
	word := func(n int) string {
		var b strings.Builder
		for i := rnd.Intn(n); i >= 0; i-- {
			b.WriteString(alphabet[rnd.Intn(len(alphabet))])
		}
		return b.String()
	}
	for i := 0; i < 3000; i++ {
		msg, grep, stripIt := word(12), word(3), i%2 == 0
		if i%3 == 0 {
			grep = "^" + grep
		}
		var stripped string
		var match bool
		q := "SELECT " + ansiSQL + ", " + grepSQL(grep, stripIt) + " FROM (SELECT ?::VARCHAR AS message)"
		if err := db.QueryRow(q, msg).Scan(&stripped, &match); err != nil {
			t.Fatalf("%q: %v", msg, err)
		}
		if stripped != removeAnsi(msg) {
			t.Fatalf("strip %q: engine %q, removeAnsi %q", msg, stripped, removeAnsi(msg))
		}
		m := msg
		if stripIt {
			m = removeAnsi(msg)
		}
		if match != matchGrep(m, grep) {
			t.Fatalf("grep %q in %q (strip %v): engine %v", grep, msg, stripIt, match)
		}
	}
}

// The reference's removeAnsi cases.
func TestRemoveAnsi(t *testing.T) {
	for in, want := range map[string]string{
		"hello world":                           "hello world",
		"\x1b[31mred\x1b[0m":                    "red",
		"\x1b[1;33mwarning\x1b[0m":              "warning",
		"\x1b[32mOK\x1b[0m \x1b[31mFAIL\x1b[0m": "OK FAIL",
		"\x1b[38;5;196mred\x1b[0m":              "red",
		"\x1b[38;2;255;0;0mred\x1b[0m":          "red",
		"\x1b[2Khello":                          "hello",
		"":                                      "",
		"\x1bhello":                             "\x1bhello",
		"pre\x1b[":                              "pre",
	} {
		if got := removeAnsi(in); got != want {
			t.Errorf("removeAnsi(%q) = %q, want %q", in, got, want)
		}
	}
}

// The reference's handler cases that apply here, on the handler without
// HTTP: an invalid source is a 400 that says so; not found errors name
// the namespace, the pod, the container and the known containers.
func TestLogsHandlerReferenceCases(t *testing.T) {
	w := newWorld(t)
	newLogsFixture(w)
	api := serveLogs(t, w, lnow, 48*time.Hour)
	ctx := context.Background()
	for _, c := range []struct {
		ns, pod, container, source string
		status                     int
		says                       []string
	}{
		{"ns1", "web", "", "magic", 400, []string{"source"}},
		{"ns1", "web", "", "k8s", 400, []string{"source", "k8s"}},
		{"prod", "nonexistent-pod", "", "", 404, []string{"prod", "nonexistent-pod"}},
		{"ns1", "ghost", "", "", 404, []string{"ghost"}},
		{"ns1", "web", "missing", "", 404, []string{"missing", "init", "app", "side"}},
	} {
		_, err := api.h.getLogs(ctx, c.ns, c.pod, c.container, false, false, 100, "", false, c.source)
		var se interface{ GetStatus() int }
		if !errors.As(err, &se) || se.GetStatus() != c.status {
			t.Fatalf("%+v: %v, want status %d", c, err, c.status)
		}
		for _, s := range c.says {
			if !strings.Contains(err.Error(), s) {
				t.Fatalf("%+v: %q does not say %q", c, err.Error(), s)
			}
		}
	}
	out, err := api.h.getLogs(ctx, "ns1", "quiet", "app", false, false, 100, "", false, "")
	if err != nil || len(out.Body) != 0 || out.LogsSource != LogsSourceBlob {
		t.Fatalf("quiet: %v %+v", err, out)
	}
}

// The API's listener serves the spec and the docs.
func TestLogsServesSpecAndDocs(t *testing.T) {
	w := newWorld(t)
	api := serveLogs(t, w, lnow, 48*time.Hour)
	a := api.get("/openapi.json")
	var spec struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal([]byte(a.body), &spec); a.status != http.StatusOK || err != nil ||
		spec.Paths["/api/v1/logs/n/{namespace}/{pod}/{container}"] == nil || spec.Paths["/api/v1/logs/n/{namespace}/{pod}"] == nil {
		t.Fatalf("/openapi.json: %d %v %s", a.status, err, a.body)
	}
	if a := api.get("/docs"); a.status != http.StatusOK || !strings.HasPrefix(a.header.Get("Content-Type"), "text/html") {
		t.Fatalf("/docs: %d %s", a.status, a.header.Get("Content-Type"))
	}
}
