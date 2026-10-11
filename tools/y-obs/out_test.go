package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLOut(t *testing.T) {
	dir := fixture(t)
	db := filepath.Join(t.TempDir(), "inv.duckdb")
	out, _, err := runSQLTest(t, dir, "--out", db+":lines", "SELECT time, pod, message FROM logs WHERE pod IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"wrote 2 rows to " + db + " table lines (created), 2 rows in the table",
		"columns: time TIMESTAMP_NS, pod VARCHAR, message VARCHAR", `next: y-obs sql --db ` + db + ` "SELECT * FROM lines LIMIT 20"`} {
		if !strings.Contains(out, want) {
			t.Errorf("out: %q lacks %q", out, want)
		}
	}
	if fi, err := os.Stat(db); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the file is not 0600: %v %v", fi.Mode(), err)
	}
	if out, _, err = runSQLTest(t, dir, "--out", db+":lines", "--append", "SELECT time, pod, message FROM logs WHERE pod IS NOT NULL"); err != nil ||
		!strings.Contains(out, "wrote 2 rows to "+db+" table lines (appended), 4 rows in the table") {
		t.Errorf("append: %q %v", out, err)
	}
	if out, _, err = runSQLTest(t, dir, "--out", db, "SELECT 1 AS one"); err != nil || !strings.Contains(out, "table result (created)") {
		t.Errorf("default table: %q %v", out, err)
	}

	// the file alone: no bucket, its tables unqualified
	var o, e strings.Builder
	err = runSQL(t.Context(), []string{"--db", db, `SELECT "table", mode, cluster, "rows" FROM y_obs_runs ORDER BY written`}, &o, &e, testWindow.to)
	if err != nil || o.String() != "table\tmode\tcluster\trows\nlines\tcreated\tc\t2\nlines\tappended\tc\t2\nresult\tcreated\tc\t1\n" {
		t.Errorf("y_obs_runs: %q %v %s", o.String(), err, e.String())
	}
	// beside the bucket's views
	if out, _, err = runSQLTest(t, dir, "--attach", db, "SELECT count(*) AS n FROM local.lines JOIN pods USING (pod)"); err != nil || out != "n\n4\n" {
		t.Errorf("attach: %q %v", out, err)
	}
	pq := filepath.Join(t.TempDir(), "m.parquet")
	if out, _, err = runSQLTest(t, dir, "--out", pq, "SELECT * FROM metrics"); err != nil || !strings.Contains(out, "wrote 1 rows to "+pq) {
		t.Errorf("parquet: %q %v", out, err)
	}
	if _, _, err = runSQLTest(t, dir, "--out", "x.txt", "SELECT 1"); exitOf(err) != exitUsage {
		t.Errorf("bad --out: %v", err)
	}
	err = runSQL(t.Context(), []string{"--db", filepath.Join(t.TempDir(), "none.duckdb"), "SELECT 1"}, &o, &e, testWindow.to)
	if exitOf(err) != exitNotFound {
		t.Errorf("no file: %v", err)
	}
}

func TestParseOut(t *testing.T) {
	for spec, want := range map[string]outTarget{
		"a.duckdb":     {"a.duckdb", "result", "duckdb"},
		"a.duckdb:t1":  {"a.duckdb", "t1", "duckdb"},
		"/x/b.db:t":    {"/x/b.db", "t", "duckdb"},
		"m.parquet":    {"m.parquet", "result", "parquet"},
		"rows.jsonl":   {"rows.jsonl", "result", "json"},
		"C:/x/out.csv": {"C:/x/out.csv", "result", "csv"},
	} {
		got, err := parseOut(spec, "result")
		if err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", spec, got, err, want)
		}
	}
	if _, err := parseOut("a.duckdb:bad-name", "result"); exitOf(err) != exitUsage {
		t.Errorf("bad table name: %v", err)
	}
}
