package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// The engine behaviour the fold is built on (compactor/README.md,
// "Compaction"). A DuckDB bump that changes any of it fails here first.

func fixture(t *testing.T) (context.Context, *sql.Conn, string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl.zst")
	b := filepath.Join(dir, "b.jsonl.zst")
	// a: one container log line, written twice as a retried upload repeats
	// it, and the pod's observation; b: a journal line and one scrape
	for _, s := range []string{
		`COPY (
		   SELECT 'logs' AS "table", TIMESTAMP_NS '2026-09-25 14:00:01' AS time, 'itest' AS cluster,
		          'n1' AS node, 'default' AS namespace, 'p' AS pod, 'u1' AS pod_uid, 'app' AS container,
		          0 AS restart, 'stdout' AS stream, 'hello' AS message
		   FROM range(2)
		   UNION ALL BY NAME
		   SELECT 'podindex' AS "table", TIMESTAMP_NS '2026-09-25 14:00:00' AS time, 'itest' AS cluster,
		          'n1' AS node, 'pod-observed' AS kind, 'default' AS namespace, 'p' AS pod, 'u1' AS pod_uid
		 ) TO ` + sqlString(a) + ` (FORMAT json, COMPRESSION zstd)`,
		`COPY (
		   SELECT 'logs' AS "table", TIMESTAMP_NS '2026-09-25 14:00:02' AS ts, 'itest' AS cluster,
		          'n1' AS node, 'journal' AS stream, 'k3s.service' AS unit, 'up' AS message
		   UNION ALL BY NAME
		   SELECT 'metrics' AS "table", TIMESTAMP_NS '2026-09-25 14:00:03' AS time, 'itest' AS cluster,
		          'n1' AS node, 'pod' AS job, 'default' AS namespace, 'p' AS pod, 'u1' AS pod_uid, 'app' AS container, 0 AS restart,
		          [{'name': 'up', 'type': 'gauge', 'labels': MAP {}, 'value': 1.0}] AS samples
		 ) TO ` + sqlString(b) + ` (FORMAT json, COMPRESSION zstd)`,
	} {
		if _, err := c.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, c, a, b
}

func count(t *testing.T, ctx context.Context, c *sql.Conn, q string) int {
	t.Helper()
	var n int
	if err := c.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestRawViewReadsTheVariableWhenQueried(t *testing.T) {
	ctx, c, a, b := fixture(t)
	if err := setRawFiles(ctx, c, []string{a}); err != nil {
		t.Fatal(err)
	}
	if err := loadSQL(ctx, c); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, c, "SELECT count(*) FROM raw"); n != 3 {
		t.Fatalf("raw over a: %d lines, want 3", n)
	}
	if err := setRawFiles(ctx, c, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, c, "SELECT count(*) FROM raw"); n != 5 {
		t.Fatalf("raw after setRawFiles(a, b): %d lines, want 5", n)
	}
}

func TestFoldShapeTempTablesTakePrecedence(t *testing.T) {
	ctx, c, a, b := fixture(t)
	if err := setRawFiles(ctx, c, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := loadSQL(ctx, c); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, c, "SELECT count(*) FROM logs WHERE namespace = '_node' AND time IS NOT NULL"); n != 1 {
		t.Fatalf("journal rows in _node with time from ts: %d, want 1", n)
	}
	// step 1: the keys read once; the views now read the temp table
	if _, err := c.ExecContext(ctx, "CREATE TEMP TABLE raw AS SELECT * FROM raw WHERE filename = "+sqlString(a)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, c, "SELECT count(*) FROM logs"); n != 2 {
		t.Fatalf("logs view over temp raw (a only): %d rows, want the 2 copies", n)
	}
	// step 2: the tier's rows deduplicated under the views' names; each
	// statement reads the view, the temp table exists only once it completes
	for _, s := range []string{
		"CREATE TEMP TABLE logs AS SELECT DISTINCT * EXCLUDE (filename) FROM logs",
		"CREATE TEMP TABLE metrics AS SELECT DISTINCT * EXCLUDE (filename) FROM metrics",
		"CREATE TEMP TABLE podindex AS SELECT DISTINCT * EXCLUDE (filename) FROM podindex",
	} {
		if _, err := c.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if n := count(t, ctx, c, "SELECT count(*) FROM logs"); n != 1 {
		t.Fatalf("temp logs: %d rows, want 1 after DISTINCT", n)
	}
	// the trap: main.<name> finds the temp table too, not the view
	if n := count(t, ctx, c, "SELECT count(*) FROM main.logs"); n != 1 {
		t.Fatalf("main.logs: %d rows, want the temp table's 1", n)
	}
	var lines int
	var phase string
	if err := c.QueryRowContext(ctx,
		"SELECT lines, phase FROM pods WHERE pod_uid = 'u1' AND container = 'app'").Scan(&lines, &phase); err != nil {
		t.Fatal(err)
	}
	if lines != 1 || phase != "running" {
		t.Fatalf("pods over the temp tables: lines %d phase %s, want 1 running", lines, phase)
	}
}

func TestPodsJoinsMetricsOnTheFullKey(t *testing.T) {
	ctx, c, a, b := fixture(t)
	if err := setRawFiles(ctx, c, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := loadSQL(ctx, c); err != nil {
		t.Fatal(err)
	}
	// the log lines (a) and the scrape (b) of one run are one pods row
	if n := count(t, ctx, c, "SELECT count(*) FROM pods WHERE pod_uid = 'u1' AND container = 'app'"); n != 1 {
		t.Fatalf("pods rows for u1/app: %d, want 1", n)
	}
	if n := count(t, ctx, c, "SELECT count(*) FROM pods WHERE pod_uid = 'u1' AND container = 'app' AND restart = 0 AND metric_names = ['up'] AND lines > 0"); n != 1 {
		t.Fatal("the run's pods row lacks its metric names or its log lines")
	}
}
