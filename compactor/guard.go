package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The guard (compactor/README.md, "Raw deletion"): with RAW_DELETE=
// after-final a raw line that no table holds is gone a few hours after it
// was written, and so is a field the raw view does not read. So a fold
// refuses, before it writes anything, when a line's table is missing or
// one it does not fold (an agent newer than the compactor, a renamed
// table, an input an overlay adds without one), or when a line has a
// field that neither the raw view nor raw_retired names (compactor/sql/
// raw.sql); and the rollup refuses a logs or metrics row without a
// namespace, which no daily partition would hold. A refusal is an error
// naming the hour or the day, the count and a key, and the fields, and
// adds the count to blobs_compactor_unaccounted_total.

// unaccountedError is a fold or a rollup that refused.
type unaccountedError struct {
	job    string // fold or rollup
	period string // the hour (2006-01-02T15) or the day
	what   string // what the rows are
	n      int64
	sample string // a raw key (fold) or an hourly object (rollup) that has one
	fields string // unknown fields with their line counts: job:12,zzz:3
}

func (e *unaccountedError) Error() string {
	what := e.what
	if e.fields != "" {
		what += " (" + e.fields + ")"
	}
	return fmt.Sprintf("%s %s refused: %d %s, for example in %s; nothing written", e.job, e.period, e.n, what, e.sample)
}

// guardTables refuses a fold whose raw temp table, the lines of keys
// (read from urls, in the same order), has a line whose table the fold
// does not write.
func guardTables(ctx context.Context, conn *sql.Conn, h time.Time, keys, urls []string) error {
	var n int64
	var file sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT count(*), min(filename) FROM raw
	  WHERE "table" IS NULL OR "table" NOT IN (`+quotedList(foldTables)+`)`).Scan(&n, &file); err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	if n == 0 {
		return nil
	}
	e := &unaccountedError{job: "fold", period: h.Format(dateLayout + "T15"), n: n,
		what: "raw lines without a table the compactor folds (" + quotedList(foldTables) + "), no raw deleted"}
	for i, u := range urls {
		if u == file.String {
			e.sample = keys[i]
		}
	}
	return e
}

// guardFields refuses a fold whose raw objects (keys, read from urls, in
// the same order) have a line with a field that neither the raw view nor
// raw_retired names. It reads the objects a second time, as JSON
// objects, since the raw view drops the fields it does not name: a fold
// makes two GETs of each raw key.
func guardFields(ctx context.Context, conn *sql.Conn, h time.Time, keys, urls []string) error {
	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE unknown_fields AS
	  WITH known AS (SELECT list(column_name) AS k FROM (SELECT column_name FROM (DESCRIBE raw) UNION ALL SELECT field FROM raw_retired))
	  SELECT filename, list_filter(json_keys(json), lambda f: NOT list_contains(known.k, f)) AS fields
	  FROM read_ndjson_objects(getvariable('raw_files'), compression = 'zstd', filename = true), known
	  WHERE len(list_filter(json_keys(json), lambda f: NOT list_contains(known.k, f))) > 0`); err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	var n int64
	var file, fields sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM unknown_fields), (SELECT min(filename) FROM unknown_fields),
	  (SELECT string_agg(f || ':' || n, ',' ORDER BY f) FROM (SELECT f, count(*) AS n FROM (SELECT unnest(fields) AS f FROM unknown_fields) GROUP BY f))`).
		Scan(&n, &file, &fields); err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	if n == 0 {
		return nil
	}
	e := &unaccountedError{job: "fold", period: h.Format(dateLayout + "T15"), n: n, fields: fields.String,
		what: "raw lines with a field the compactor neither reads nor retires (compactor/sql/raw.sql), no raw deleted"}
	for i, u := range urls {
		if u == file.String {
			e.sample = keys[i]
		}
	}
	return e
}

// guardNamespaces refuses a rollup whose changed hours (objs, the logs and
// metrics objects) have a row without a namespace. The filter reads the
// row groups' statistics; on a refusal it reads the objects one by one to
// name one that has such a row.
func (c *compactor) guardNamespaces(ctx context.Context, conn *sql.Conn, day time.Time, objs map[string][]tierObject) error {
	e := &unaccountedError{job: "rollup", period: day.Format(dateLayout), what: "logs or metrics rows without a namespace"}
	for _, t := range []string{"logs", "metrics"} {
		if len(objs[t]) == 0 {
			continue
		}
		var n int64
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM ("+c.readTier(objs[t], "")+") WHERE namespace IS NULL").Scan(&n); err != nil {
			return fmt.Errorf("guard: %w", err)
		}
		e.n += n
		for _, o := range objs[t] {
			if n == 0 || e.sample != "" {
				break
			}
			var m int64
			if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM ("+c.readTier([]tierObject{o}, "")+") WHERE namespace IS NULL").Scan(&m); err == nil && m > 0 {
				e.sample = o.key
			}
		}
	}
	if e.n > 0 {
		return e
	}
	return nil
}

func quotedList(names []string) string {
	q := ""
	for i, n := range names {
		if i > 0 {
			q += ", "
		}
		q += sqlString(n)
	}
	return q
}
