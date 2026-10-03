package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"path/filepath"
	"strings"
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

// The engine's Parquet encryption, as encryption.go and every tier reader
// rely on it (compactor/README.md, "Encryption").
func TestEngineParquetEncryption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := func(name string) string { return sqlString(filepath.Join(dir, name)) }
	k32 := "0123456789abcdef0123456789abcdef" // printable, so it is a plain 32 byte key too
	k16 := "0123456789abcdef"
	keys := []initStmt{
		{sql: "PRAGMA add_parquet_key('raw32', '" + k32 + "')"},
		{sql: "PRAGMA add_parquet_key('b64-32', '" + base64.StdEncoding.EncodeToString([]byte(k32)) + "')"},
		{sql: "PRAGMA add_parquet_key('raw16', '" + k16 + "')"},
		{sql: "PRAGMA add_parquet_key('b64-16', '" + base64.StdEncoding.EncodeToString([]byte(k16)) + "')"},
	}
	engine := func(init ...initStmt) *sql.DB {
		t.Helper()
		db, err := open("", append(init, keys...)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	fails := func(db *sql.DB, q, want string) {
		t.Helper()
		_, err := db.ExecContext(ctx, q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err %v, want %q", q, err, want)
		}
	}
	n := func(db *sql.DB, q string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	// Without httpfs the engine's crypto reads but does not write.
	fails(engine(), "COPY (SELECT 1 AS i) TO "+f("x.parquet")+" (FORMAT parquet, ENCRYPTION_CONFIG {footer_key: 'raw32'})",
		"read-only crypto module")

	db := engine(initStmt{sql: "SET force_mbedtls_unsafe = true"})
	for _, s := range []string{
		"COPY (SELECT range AS i FROM range(10)) TO " + f("a.k-b64-32.parquet") + " (FORMAT parquet, ENCRYPTION_CONFIG {footer_key: 'b64-32'})",
		"COPY (SELECT range AS i, 'x' AS c FROM range(5)) TO " + f("b.k-raw16.parquet") + " (FORMAT parquet, ENCRYPTION_CONFIG {footer_key: 'raw16'})",
		"COPY (SELECT range AS i FROM range(3)) TO " + f("c.k-b64-16.parquet") + " (FORMAT parquet, ENCRYPTION_CONFIG {footer_key: 'b64-16'})",
		"COPY (SELECT range AS i FROM range(2)) TO " + f("d.parquet") + " (FORMAT parquet)",
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	// The base64 of a 32 byte key is decoded; the base64 of a 16 byte key,
	// 24 characters, is taken as a 24 byte key itself. Key files hold 32
	// bytes only (encryption.go).
	if got := n(db, "SELECT count(*) FROM read_parquet("+f("a.k-b64-32.parquet")+", encryption_config = {footer_key: 'raw32'})"); got != 10 {
		t.Fatalf("a 32 byte key's base64: %d rows", got)
	}
	fails(db, "SELECT * FROM read_parquet("+f("c.k-b64-16.parquet")+", encryption_config = {footer_key: 'raw16'})",
		"Computed AES tag differs")
	// A plain read of an encrypted file fails, a keyed read of a plain one
	// too, and one read takes one key, so a glob over two keys fails.
	fails(db, "SELECT * FROM read_parquet("+f("a.k-b64-32.parquet")+")", "is encrypted, but 'encryption_config' was not set")
	fails(db, "SELECT * FROM read_parquet("+f("d.parquet")+", encryption_config = {footer_key: 'raw32'})",
		"is not encrypted, but 'encryption_config' was set")
	fails(db, "SELECT * FROM read_parquet("+f("[ab].k-*.parquet")+", encryption_config = {footer_key: 'raw32'})",
		"Computed AES tag differs")
	// One read per key, united by name with the plain read, which aligns
	// the columns.
	if got := n(db, "SELECT count(c) * 100 + count(*) FROM ("+
		"SELECT * FROM read_parquet("+f("d.parquet")+", union_by_name = true) UNION ALL BY NAME "+
		"SELECT * FROM read_parquet("+f("*.k-b64-32.parquet")+", encryption_config = {footer_key: 'b64-32'}, filename = true) UNION ALL BY NAME "+
		"SELECT * FROM read_parquet("+f("*.k-raw16.parquet")+", encryption_config = {footer_key: 'raw16'}))"); got != 5*100+17 {
		t.Fatalf("the per-key union: %d, want 517", got)
	}
	// union_by_name on an encrypted read is an internal error, which also
	// invalidates the database: readers never pass it with a key.
	fails(engine(initStmt{sql: "SET force_mbedtls_unsafe = true"}),
		"SELECT sum(i) FROM read_parquet("+f("a.k-b64-32.parquet")+", encryption_config = {footer_key: 'b64-32'}, union_by_name = true)",
		"INTERNAL Error")
}
