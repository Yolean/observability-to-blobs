package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// fixture is a blobs directory with one raw object for 2026-10-11 09:xx of
// cluster c on node n1, and that hour's logs in the hourly tier, written
// through the views themselves.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lines := []map[string]any{
		{"table": "logs", "time": "2026-10-11T09:01:00.000000001Z", "ts": "2026-10-11T09:01:00.000000Z", "cluster": "c", "node": "n1",
			"namespace": "qa", "pod": "web-1", "pod_uid": "u1", "container": "app", "restart": 0, "stream": "stdout",
			"message": "tab\there\nnewline"},
		{"table": "logs", "time": "2026-10-11T09:02:00Z", "ts": "2026-10-11T09:02:00Z", "cluster": "c", "node": "n1",
			"namespace": "qa", "pod": "web-1", "pod_uid": "u1", "container": "app", "restart": 0, "stream": "stderr",
			"message": "error: boom"},
		{"table": "logs", "ts": "2026-10-11T09:03:00Z", "cluster": "c", "node": "n1", "stream": "journal",
			"unit": "kubelet.service", "priority": 6, "message": "journal line"},
		{"table": "metrics", "time": "2026-10-11T09:01:30Z", "ts": "2026-10-11T09:01:30Z", "cluster": "c", "node": "n1",
			"scrape": "pod", "instance": "10.0.0.1:9090", "namespace": "qa", "pod": "web-1", "pod_uid": "u1", "container": "app", "restart": 0,
			"samples": []map[string]any{{"name": "http_requests_total", "type": "counter", "labels": map[string]string{"code": "200"}, "value": 42}}},
		{"table": "podindex", "time": "2026-10-11T09:04:00Z", "ts": "2026-10-11T09:04:00Z", "cluster": "c", "node": "n1",
			"kind": "container-exit", "namespace": "qa", "pod": "web-1", "pod_uid": "u1", "container": "app", "restart": 0,
			"exit_code": 137, "exit_reason": "OOMKilled"},
	}
	raw := filepath.Join(dir, "raw1/c/2026-10-11/09/n1")
	if err := os.MkdirAll(raw, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	for _, l := range lines {
		b, _ := json.Marshal(l)
		zw.Write(append(b, '\n'))
	}
	zw.Close()
	if err := os.WriteFile(filepath.Join(raw, "0100-a.jsonl.zst"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// the hourly tier's logs object for 09, from the raw view
	ctx := context.Background()
	sf := &sessionFlags{cluster: "c", blobsDir: dir}
	s, err := openSession(ctx, sf, testWindow)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.prepare(ctx, map[string]bool{"raw": true}, 10); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "hourly/c/logs/2026-10-11")
	os.MkdirAll(out, 0o755)
	if err := s.exec(ctx, "COPY (SELECT * EXCLUDE (filename) FROM logs) TO "+sqlString(filepath.Join(out, "09.parquet"))); err != nil {
		t.Fatal(err)
	}
	return dir
}

var testWindow = window{from: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC), to: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)}

func runSQLTest(t *testing.T, dir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var o, e bytes.Buffer
	args = append([]string{"--cluster", "c", "--blobs-dir", dir, "--from", "2026-10-11 09:00", "--to", "2026-10-11 10:00"}, args...)
	err = runSQL(context.Background(), args, &o, &e, testWindow.to)
	return o.String(), e.String(), err
}

func exitOf(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestSQLRawAndHourly(t *testing.T) {
	dir := fixture(t)
	out, errOut, err := runSQLTest(t, dir, "SELECT namespace, pod, stream, message FROM logs ORDER BY time")
	if err != nil {
		t.Fatal(err, errOut)
	}
	want := "namespace\tpod\tstream\tmessage\n" +
		"qa\tweb-1\tstdout\ttab\\there\\nnewline\n" +
		"qa\tweb-1\tstderr\terror: boom\n" +
		"_node\t\tjournal\tjournal line\n"
	if out != want {
		t.Errorf("tsv:\n%q\nwant\n%q", out, want)
	}
	if !strings.Contains(errOut, "objects raw=1 rows=3 fresh-to=2026-10-11T09:01:00Z") {
		t.Errorf("summary: %s", errOut)
	}
	out, _, err = runSQLTest(t, dir, "--format", "jsonl", "SELECT time, labels, value FROM metrics")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"time":"2026-10-11T09:01:30.000000000Z","labels":{"code":"200"},"value":42}` + "\n"; out != want {
		t.Errorf("jsonl: %s, want %s", out, want)
	}
	out, _, err = runSQLTest(t, dir, "SELECT count(*) AS n FROM hourly_logs")
	if err != nil || out != "n\n3\n" {
		t.Errorf("hourly_logs: %q %v", out, err)
	}
	out, _, err = runSQLTest(t, dir, "SELECT exit_reason FROM pods WHERE exit_code = 137")
	if err != nil || out != "exit_reason\nOOMKilled\n" {
		t.Errorf("pods: %q %v", out, err)
	}
}

func TestSQLExits(t *testing.T) {
	dir := fixture(t)
	if _, e, err := runSQLTest(t, dir, "--limit", "1", "SELECT * FROM logs"); err != nil || !strings.Contains(e, "more rows than --limit 1") {
		t.Errorf("limit: %v %s", err, e)
	}
	if _, _, err := runSQLTest(t, dir, "SELECT * FROM logs WHERE false"); exitOf(err) != exitNoRows {
		t.Errorf("no rows: %v", err)
	}
	if _, _, err := runSQLTest(t, dir, "SELECT * FROM daily_logs"); exitOf(err) != exitNoRows || !strings.Contains(err.Error(), "daily_logs: no objects") {
		t.Errorf("missing tier: %v", err)
	}
	if _, _, err := runSQLTest(t, dir, "--max-objects", "0", "SELECT * FROM logs"); exitOf(err) != exitBudget {
		t.Errorf("budget: %v", err)
	}
	if _, _, err := runSQLTest(t, dir, "SELECT * FROM log"); exitOf(err) != exitNotFound || !strings.Contains(err.Error(), `Did you mean "logs"`) {
		t.Errorf("typo: %v", err)
	}
	if _, _, err := runSQLTest(t, dir, "SELEC 1 FROM logs"); exitOf(err) != exitUsage {
		t.Errorf("a syntax error: %v", err)
	}
	if _, _, err := runSQLTest(t, dir, "SELECT nosuchcolumn FROM logs"); exitOf(err) != exitUsage {
		t.Errorf("an unknown column: %v", err)
	}
	if _, _, err := runSQLTest(t, dir); exitOf(err) != exitUsage {
		t.Errorf("no query: %v", err)
	}
	out, _, err := runSQLTest(t, dir, "--explain", "SELECT * FROM hourly_logs")
	if err != nil || !strings.Contains(out, "hourly_logs\t1 objects") {
		t.Errorf("explain: %q %v", out, err)
	}
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		f    sessionFlags
		want string
	}{
		{sessionFlags{since: "1h"}, "2026-10-11T11:30:00Z..2026-10-11T12:30:00Z"},
		{sessionFlags{since: "2d"}, "2026-10-09T12:30:00Z..2026-10-11T12:30:00Z"},
		{sessionFlags{since: "1h", from: "2026-10-11 09:00"}, "2026-10-11T09:00:00Z..2026-10-11T10:00:00Z"},
		{sessionFlags{since: "1h", from: "2026-10-11T09:00:00Z", to: "2026-10-11T09:30:00Z"}, "2026-10-11T09:00:00Z..2026-10-11T09:30:00Z"},
	} {
		w, err := c.f.window(now)
		if err != nil || w.String() != c.want {
			t.Errorf("%+v: %s %v, want %s", c.f, w, err, c.want)
		}
	}
	if _, err := (&sessionFlags{since: "1h", from: "2026-10-11 10:00", to: "2026-10-11 09:00"}).window(now); err == nil {
		t.Error("an inverted window passed")
	}
}

func TestReferenced(t *testing.T) {
	ref := referenced("SELECT * FROM hourly_logs JOIN pods USING (pod_uid)")
	if !ref["hourly_logs"] || !ref["raw"] || ref["hourly_pods"] {
		t.Errorf("%v", ref)
	}
	if ref := referenced("SELECT * FROM daily_metrics"); ref["raw"] {
		t.Errorf("daily_metrics planned raw: %v", ref)
	}
}

func runSQLTestAt(t *testing.T, dir, from, to string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var o, e bytes.Buffer
	args = append([]string{"--cluster", "c", "--blobs-dir", dir, "--from", from, "--to", to}, args...)
	err = runSQL(context.Background(), args, &o, &e, time.Now())
	return o.String(), e.String(), err
}

func TestEngineThreads(t *testing.T) {
	t.Setenv("Y_OBS_THREADS", "")
	for limit, want := range map[string]int{"2GB": 16, "512MiB": 4, "1.5 GB": 12, "100MB": 1, "bogus": 1} {
		if got := engineThreads(limit); got != min(want, runtime.NumCPU()) {
			t.Errorf("engineThreads(%q) = %d, want %d", limit, got, want)
		}
	}
	t.Setenv("Y_OBS_THREADS", "3")
	if got := engineThreads("2GB"); got != 3 {
		t.Errorf("Y_OBS_THREADS=3 gave %d", got)
	}
}

func TestParseAt(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":                     now,
		"now":                  now,
		"-5m":                  now.Add(-5 * time.Minute),
		"-1d2h":                now.Add(-26 * time.Hour),
		"2026-10-11 09:00":     time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC),
		"2026-10-11T09:00:00Z": time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC),
	} {
		if got, err := parseAt(in, now); err != nil || !got.Equal(want) {
			t.Errorf("parseAt(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"-", "-0m", "-later", "yesterday"} {
		if _, err := parseAt(in, now); err == nil {
			t.Errorf("parseAt(%q) took it", in)
		}
	}
}

func TestAutoSource(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 20, 0, 0, time.UTC)
	for at, want := range map[time.Time]string{
		now:                        "raw",
		now.Add(-50 * time.Minute): "raw",  // 09:30: hour 09 is final at 10:30
		now.Add(-81 * time.Minute): "tier", // 08:59: hour 08 was final at 09:30
	} {
		if got := autoSource(at, now); got != want {
			t.Errorf("autoSource(%v) = %s, want %s", at, got, want)
		}
	}
}
