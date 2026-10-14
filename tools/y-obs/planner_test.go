package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func logLine(at, msg string) map[string]interface{} {
	return map[string]interface{}{"table": "logs", "time": at, "ts": at, "cluster": "c", "node": "n1",
		"namespace": "qa", "pod": "web-1", "pod_uid": "u1", "container": "app", "restart": 0, "stream": "stdout", "message": msg}
}

// tierObject writes lines as a raw object in a scratch directory, and
// their logs view, as the compactor would, to dest.
func tierObject(t *testing.T, dest string, lines []map[string]interface{}) {
	t.Helper()
	scratch := t.TempDir()
	writeRaw(t, filepath.Join(scratch, "raw1/c/2026-10-11/00/n1/x.jsonl.zst"), lines)
	ctx := context.Background()
	s, err := openSession(ctx, &sessionFlags{cluster: "c", blobsDir: scratch}, window{from: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC), to: time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.prepare(ctx, map[string]bool{"raw": true}, 10); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(dest), 0o755)
	if err := s.exec(ctx, "COPY (SELECT * EXCLUDE (filename) FROM logs) TO "+sqlString(dest)); err != nil {
		t.Fatal(err)
	}
}

// One window, every source: the day before from the daily tier only, 08
// final in both raw and hourly (hourly wins), 09 raw only, 10 hourly only
// and not final yet.
func TestPlanWindow(t *testing.T) {
	dir := t.TempDir()
	tierObject(t, filepath.Join(dir, "daily/c/logs/2026-10-10/namespace=qa/data_0.parquet"), []map[string]interface{}{
		logLine("2026-10-10T22:30:00Z", "daily 22"), logLine("2026-10-10T12:00:00Z", "daily outside the window")})
	tierObject(t, filepath.Join(dir, "hourly/c/logs/2026-10-11/08.parquet"), []map[string]interface{}{logLine("2026-10-11T08:30:00Z", "hourly 08")})
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/08/n1/a.jsonl.zst"), []map[string]interface{}{logLine("2026-10-11T08:30:00Z", "raw 08, also in hourly")})
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/09/n1/a.jsonl.zst"), []map[string]interface{}{logLine("2026-10-11T09:30:00Z", "raw 09")})
	tierObject(t, filepath.Join(dir, "hourly/c/logs/2026-10-11/10.parquet"), []map[string]interface{}{logLine("2026-10-11T10:30:00Z", "hourly 10")})

	ctx := context.Background()
	now := time.Date(2026, 10, 11, 11, 0, 0, 0, time.UTC)
	w := window{from: time.Date(2026, 10, 10, 22, 0, 0, 0, time.UTC), to: now}
	s, err := openSession(ctx, &sessionFlags{cluster: "c", blobsDir: dir}, w)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.planWindow(ctx, []string{"logs"}, "qa", now); err != nil {
		t.Fatal(err)
	}
	want := map[string]hourSource{"2026-10-10/22": fromDaily, "2026-10-10/23": fromDaily, "2026-10-11/08": fromHourly, "2026-10-11/09": fromRaw, "2026-10-11/10": fromHourly, "2026-10-11/00": fromNone}
	for k, v := range want {
		if s.wplan.sources[k] != v {
			t.Errorf("%s: %s, want %s", k, s.wplan.sources[k], v)
		}
	}
	if got := s.sourcesSummary(); got != "hours raw=1 hourly=2 daily=2 none=8" {
		t.Errorf("summary: %s", got)
	}
	if err := s.createWindowViews(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.conn.QueryContext(ctx, "SELECT message FROM w_logs ORDER BY time")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var m string
		rows.Scan(&m)
		got = append(got, m)
	}
	if strings.Join(got, "|") != "daily 22|hourly 08|raw 09|hourly 10" {
		t.Errorf("w_logs: %v", got)
	}
}

// The current day's daily object is a partial rollup: hours without
// another tier stay "none" rather than turn the day to daily, and the raw
// hours keep raw. An hourly object under a key the session lacks is not
// planned: the hour reads raw, with a warning.
func TestPlanWindowPartialDayAndKeys(t *testing.T) {
	dir := t.TempDir()
	tierObject(t, filepath.Join(dir, "daily/c/logs/2026-10-11/namespace=qa/data_0.parquet"), []map[string]interface{}{logLine("2026-10-11T08:30:00Z", "partial daily")})
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/09/n1/a.jsonl.zst"), []map[string]interface{}{logLine("2026-10-11T09:30:00Z", "raw 09")})
	tierObject(t, filepath.Join(dir, "hourly/c/logs/2026-10-11/09.k-k1.parquet"), []map[string]interface{}{logLine("2026-10-11T09:30:00Z", "hourly 09 under k1")})
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, err := openSession(ctx, &sessionFlags{cluster: "c", blobsDir: dir}, window{from: time.Date(2026, 10, 11, 7, 0, 0, 0, time.UTC), to: now})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.planWindow(ctx, []string{"logs"}, "qa", now); err != nil {
		t.Fatal(err)
	}
	if got := s.sourcesSummary(); got != "hours raw=1 none=4" {
		t.Errorf("summary: %s", got)
	}
	if len(s.warnings) != 1 || !strings.Contains(s.warnings[0], "parquet key k1") {
		t.Errorf("warnings: %v", s.warnings)
	}
	if err := s.createWindowViews(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.strings(ctx, "SELECT message FROM w_logs ORDER BY time")
	if err != nil || strings.Join(got, "|") != "raw 09" {
		t.Errorf("w_logs: %v %v", got, err)
	}
}

// Raw keys are cut to the window by their open time, with its margins;
// a raw object costs a tenth.
func TestRawInWindow(t *testing.T) {
	s := &session{w: window{from: time.Date(2026, 10, 11, 9, 10, 0, 0, time.UTC), to: time.Date(2026, 10, 11, 9, 20, 0, 0, time.UTC)}, plan: map[string][]string{}}
	files := []string{
		"s3://b/raw1/c/2026-10-11/09/n1/0700-a.jsonl.zst", // before the margin
		"s3://b/raw1/c/2026-10-11/09/n1/0815-a.jsonl.zst", // in the margin before
		"s3://b/raw1/c/2026-10-11/09/n1/1500-a.jsonl.zst",
		"s3://b/raw1/c/2026-10-11/09/n1/2430-a.jsonl.zst", // in the margin after
		"s3://b/raw1/c/2026-10-11/09/n1/2500-a.jsonl.zst", // past it
		"s3://b/raw1/c/2026-10-11/09/n1/x.jsonl.zst",      // no time: kept
	}
	got := s.rawInWindow(files)
	if strings.Join(got, " ") != strings.Join([]string{files[1], files[2], files[3], files[5]}, " ") {
		t.Errorf("%v", got)
	}
	s.plan["raw"] = make([]string, 21)
	s.plan["hourly_logs"] = make([]string, 3)
	if c := s.cost(); c != 6 {
		t.Errorf("cost %d, want 3 + ceil(21/10) = 6", c)
	}
	err := s.overBudget(5, "the plan")
	if exitOf(err) != exitBudget || !strings.Contains(err.Error(), "--node NODE") || strings.Contains(err.Error(), "-n NAMESPACE") {
		t.Errorf("%v", err)
	}
}

func TestExplainOverBudget(t *testing.T) {
	dir := fixture(t)
	out, _, err := runSQLTest(t, dir, "--explain", "--max-objects", "0", "SELECT * FROM logs")
	if exitOf(err) != exitBudget || !strings.Contains(out, "raw\t1 objects") {
		t.Errorf("explain over budget: %q %v", out, err)
	}
}

// A window starting on the hour reads the previous hour's last object,
// which holds its first rows: through the window planner and sql's raw view.
func TestWindowOnTheHour(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/09/n1/5945-a.jsonl.zst"), []map[string]interface{}{
		logLine("2026-10-11T09:59:50Z", "before the window"), logLine("2026-10-11T10:00:10Z", "first in the window")})
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/10/n1/0015-a.jsonl.zst"), []map[string]interface{}{logLine("2026-10-11T10:00:20Z", "second")})
	now := time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC)
	var o, e bytes.Buffer
	if err := runLogs(context.Background(), []string{"--cluster", "c", "--blobs-dir", dir, "--from", "2026-10-11 10:00", "--to", "2026-10-11 10:30", "qa", "web-1"}, &o, &e, now); err != nil {
		t.Fatal(err, e.String())
	}
	if o.String() != "first in the window\nsecond\n" {
		t.Errorf("logs: %q", o.String())
	}
	out, _, err := runSQLTestAt(t, dir, "2026-10-11 10:00", "2026-10-11 10:30", "SELECT count(*) AS n FROM logs WHERE time >= '2026-10-11 10:00:00'")
	if err != nil || out != "n\n2\n" {
		t.Errorf("sql: %q %v", out, err)
	}
}
