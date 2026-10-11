package main

import (
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
