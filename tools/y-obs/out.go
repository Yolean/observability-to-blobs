package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// outFlags keep a result in a local file instead of printing it, and
// query such files (README.md, "Output to a DuckDB file").
type outFlags struct {
	out, db, attach string
	append          bool
}

func (o *outFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.out, "out", "", "write the rows to FILE.duckdb[:TABLE] (a table, replaced unless --append), or to .parquet, .csv or .jsonl")
	fs.BoolVar(&o.append, "append", false, "with --out to a .duckdb file: add the rows to the table instead of replacing it")
	fs.StringVar(&o.db, "db", "", "query this .duckdb file alone: no bucket, no credentials")
	fs.StringVar(&o.attach, "attach", "", "attach this .duckdb file as local beside the bucket's views, to join saved tables (local.<table>) with them")
}

type outTarget struct {
	path, table, format string // format: duckdb, parquet, csv, json
}

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseOut reads FILE.duckdb[:TABLE] or FILE.{parquet,csv,jsonl}.
func parseOut(spec, defaultTable string) (outTarget, error) {
	t := outTarget{path: spec, table: defaultTable}
	if i := strings.LastIndex(spec, ":"); i > 0 {
		base, table := spec[:i], spec[i+1:]
		if ext := strings.ToLower(filepath.Ext(base)); ext == ".duckdb" || ext == ".db" {
			if !identPattern.MatchString(table) {
				return t, fail(exitUsage, "--out %s: %q is not a table name (letters, digits, _)", spec, table)
			}
			t.path, t.table = base, table
		}
	}
	switch strings.ToLower(filepath.Ext(t.path)) {
	case ".duckdb", ".db":
		t.format = "duckdb"
	case ".parquet":
		t.format = "parquet"
	case ".csv":
		t.format = "csv"
	case ".jsonl", ".ndjson":
		t.format = "json"
	default:
		return t, fail(exitUsage, "--out %s: name a .duckdb, .parquet, .csv or .jsonl file", spec)
	}
	return t, nil
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	pa, err1 := filepath.Abs(a)
	pb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && pa == pb
}

// attachLocal attaches a .duckdb file as local, read-only unless the
// result is written into it.
func (s *session) attachLocal(ctx context.Context, path string, writable bool) error {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return fail(exitNotFound, "%s: no such file; --out FILE.duckdb creates one", path)
	}
	mode := " (READ_ONLY)"
	if writable {
		mode = ""
	}
	return s.exec(ctx, "ATTACH "+sqlString(path)+" AS local"+mode)
}

// openLocal is a session over one .duckdb file and nothing else, its
// tables unqualified.
func openLocal(ctx context.Context, path string, writable bool) (*session, error) {
	s := &session{plan: map[string][]string{}, keyIDs: map[string]bool{}}
	if err := s.openEngine(ctx, []string{"SET TimeZone = 'UTC'"}); err != nil {
		return nil, err
	}
	if err := s.attachLocal(ctx, path, writable); err != nil {
		s.close()
		return nil, err
	}
	if err := s.exec(ctx, "USE local"); err != nil {
		s.close()
		return nil, err
	}
	s.local = path
	return s, nil
}

// trimQuery drops a trailing semicolon, so the query nests as a subquery.
func trimQuery(q string) string {
	return strings.TrimRight(strings.TrimSpace(q), "; \n\t")
}

// writeOut runs query into the target and prints, instead of rows, what
// the next step needs: the table, its row count, its columns and a query.
func (s *session) writeOut(ctx context.Context, t outTarget, query string, appendRows bool, cmdline string, stdout io.Writer) (int64, error) {
	old := syscall.Umask(0o077) // the file holds log lines
	defer syscall.Umask(old)
	q := trimQuery(query)
	if t.format != "duckdb" {
		opts := map[string]string{"parquet": "FORMAT parquet", "csv": "FORMAT csv, HEADER", "json": "FORMAT json"}[t.format]
		res, err := s.conn.ExecContext(ctx, "COPY ("+q+") TO "+sqlString(t.path)+" ("+opts+")")
		if err != nil {
			return 0, s.wrap(err)
		}
		n, _ := res.RowsAffected()
		fmt.Fprintf(stdout, "wrote %d rows to %s\n", n, t.path)
		return n, nil
	}
	alias := "y_obs_out"
	if samePath(t.path, s.local) {
		alias = "local"
	} else if err := s.exec(ctx, "ATTACH "+sqlString(t.path)+" AS "+alias); err != nil {
		return 0, err
	} else {
		defer s.exec(context.Background(), "DETACH "+alias)
	}
	table := alias + `."` + t.table + `"`
	var exists bool
	if err := s.conn.QueryRowContext(ctx, "SELECT count(*) > 0 FROM duckdb_tables() WHERE database_name = ? AND table_name = ?", alias, t.table).Scan(&exists); err != nil {
		return 0, s.wrap(err)
	}
	mode, stmt := "created", "CREATE OR REPLACE TABLE "+table+" AS SELECT * FROM ("+q+")"
	if exists && appendRows {
		mode, stmt = "appended", "INSERT INTO "+table+" BY NAME SELECT * FROM ("+q+")"
	} else if exists {
		mode = "replaced"
	}
	var before int64
	if mode == "appended" {
		if err := s.conn.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&before); err != nil {
			return 0, s.wrap(err)
		}
	}
	if err := s.exec(ctx, stmt); err != nil {
		return 0, err
	}
	var total int64
	if err := s.conn.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&total); err != nil {
		return 0, s.wrap(err)
	}
	written := total - before
	if err := s.recordRun(ctx, alias, t.table, mode, cmdline, written); err != nil {
		return 0, err
	}
	cols, err := s.columns(ctx, alias, t.table)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(stdout, "wrote %d rows to %s table %s (%s), %d rows in the table\n", written, t.path, t.table, mode, total)
	fmt.Fprintf(stdout, "columns: %s\n", strings.Join(cols, ", "))
	fmt.Fprintf(stdout, "next: y-obs sql --db %s \"SELECT * FROM %s LIMIT 20\"\n", shellQuote(t.path), t.table)
	return written, nil
}

// recordRun keeps provenance beside the table: what wrote it, over which
// window and objects, and the rows that run wrote. Credentials are never
// written.
func (s *session) recordRun(ctx context.Context, alias, table, mode, cmdline string, rows int64) error {
	if err := s.exec(ctx, "CREATE TABLE IF NOT EXISTS "+alias+`.y_obs_runs (written TIMESTAMP, "table" VARCHAR, mode VARCHAR, command VARCHAR, cluster VARCHAR, window_from TIMESTAMP, window_to TIMESTAMP, objects VARCHAR, "rows" BIGINT)`); err != nil {
		return err
	}
	var from, to any
	if !s.w.from.IsZero() {
		from, to = s.w.from, s.w.to
	}
	objects := s.planSummary()
	if s.local != "" && s.cluster == "" {
		objects = "local " + s.local
	}
	_, err := s.conn.ExecContext(ctx, "INSERT INTO "+alias+".y_obs_runs VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		time.Now().UTC(), table, mode, cmdline, nullIfEmpty(s.cluster), from, to, objects, rows)
	return s.wrap(err)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *session) columns(ctx context.Context, db, table string) ([]string, error) {
	rows, err := s.conn.QueryContext(ctx, "SELECT column_name, data_type FROM duckdb_columns() WHERE database_name = ? AND table_name = ? ORDER BY column_index", db, table)
	if err != nil {
		return nil, s.wrap(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		cols = append(cols, name+" "+typ)
	}
	return cols, rows.Err()
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func commandLine(name string, args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.TrimSpace("y-obs " + name + " " + strings.Join(q, " "))
}

// limited applies --limit to a query written with --out, where output is
// not cut unless --limit is given.
func limited(q string, limit int) string {
	if limit <= 0 {
		return q
	}
	return fmt.Sprintf("SELECT * FROM (%s) LIMIT %d", trimQuery(q), limit)
}
