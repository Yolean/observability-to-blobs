package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// rollupResult is what one rollup of a day did.
type rollupResult struct {
	day        time.Time
	hours      []time.Time // the changed hours
	namespaces int         // namespaces whose daily rows were rewritten
	rows       map[string]int64
	runs       int // writes sorted in runs (writeSorted)
}

func listLiteral(urls []string) string {
	q := make([]string, len(urls))
	for i, u := range urls {
		q[i] = sqlString(u)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// rollup folds a day's changed hours into the daily tier (compactor/README.md,
// "The rollup of one day"). The changed hours are those whose sidecar has
// keys the daily sidecar lacks. Their logs, metrics and podindex, united
// with the day's existing rows of the namespaces they hold, are rewritten
// one namespace at a time, large writes in sorted runs (writeSorted); pods
// is recomputed for those namespaces from the objects written, the other
// namespaces' pods rows are kept; the daily sidecar, the union of the day's
// keys, is written last. It reads no raw objects, and no hourly objects
// but the changed hours'.
func (c *compactor) rollup(ctx context.Context, day time.Time) (rollupResult, error) {
	day = day.UTC().Truncate(24 * time.Hour)
	r := rollupResult{day: day}
	sidecars, err := c.tiers.List(ctx, c.l.hourlySidecarDayPrefix(day))
	if err != nil {
		return r, err
	}
	var urls []string
	for _, o := range sidecars {
		if _, ok := sidecarHour(o.Key); ok {
			urls = append(urls, c.tiers.URL(o.Key))
		}
	}
	if len(urls) == 0 {
		return r, nil
	}
	conn, err := c.conn(ctx)
	if err != nil {
		return r, err
	}
	defer conn.Close()

	// The day's keys per hour, read before any hourly object, so the rows
	// read below hold at least the keys the daily sidecar will list.
	if err := c.exec(ctx, conn, "CREATE TEMP TABLE hour_keys AS SELECT regexp_extract(filename, '(\\d{2})\\.csv$', 1) AS hh, key "+
		"FROM read_csv("+listLiteral(urls)+", header = true, filename = true, columns = {'key': 'VARCHAR', 'final': 'BOOLEAN'})"); err != nil {
		return r, err
	}
	dayKeys := c.l.dailySidecar(day)
	ok, err := c.tiers.Stat(ctx, dayKeys)
	if err != nil {
		return r, err
	}
	q := "CREATE TEMP TABLE day_keys (key VARCHAR)"
	if ok {
		q = "CREATE TEMP TABLE day_keys AS SELECT key FROM read_csv(" + sqlString(c.tiers.URL(dayKeys)) +
			", header = true, columns = {'key': 'VARCHAR'})"
	}
	if err := c.exec(ctx, conn, q); err != nil {
		return r, err
	}
	hhs, err := queryStrings(ctx, conn, "SELECT DISTINCT hh FROM hour_keys ANTI JOIN day_keys USING (key) ORDER BY hh")
	if err != nil || len(hhs) == 0 {
		return r, err
	}
	for _, hh := range hhs {
		h, err := time.Parse(dateLayout+"/15", day.Format(dateLayout)+"/"+hh)
		if err != nil {
			return r, err
		}
		r.hours = append(r.hours, h)
	}

	c.log.Info("rollup start", "day", day.Format(dateLayout), "hours", hourList(r.hours))

	// Typed views over a raw object with no lines, kept as empty tables:
	// every read below starts from a table's schema, so a table with no
	// input is empty, not an error, and objects written before a column
	// was added align by name.
	if err := setRawFiles(ctx, conn, []string{c.empty}); err != nil {
		return r, err
	}
	if err := loadSQL(ctx, conn); err != nil {
		return r, err
	}
	hourly := map[string]string{} // table: a FROM clause over the changed hours' objects
	for _, t := range foldTables {
		if err := c.exec(ctx, conn, fmt.Sprintf("CREATE TEMP TABLE empty_%s AS SELECT * EXCLUDE (filename) FROM %s LIMIT 0", t, t)); err != nil {
			return r, err
		}
		var objs []tierObject
		for _, h := range r.hours {
			o, ok, err := c.locate(ctx, c.l.hourlyStem(t, h))
			if err != nil {
				return r, err
			}
			if ok {
				objs = append(objs, o)
			}
		}
		hourly[t] = "(SELECT * FROM empty_" + t
		if len(objs) > 0 {
			hourly[t] += " UNION ALL BY NAME " + c.readTier(objs, "")
		}
		hourly[t] += ")"
	}
	if err := c.exec(ctx, conn, "CREATE TEMP TABLE changed_namespaces AS SELECT DISTINCT namespace FROM ("+
		"SELECT namespace FROM "+hourly["logs"]+" UNION ALL SELECT namespace FROM "+hourly["metrics"]+
		" UNION ALL SELECT namespace FROM "+hourly["podindex"]+")"); err != nil {
		return r, err
	}
	changed, err := queryStrings(ctx, conn, "SELECT namespace FROM changed_namespaces WHERE namespace IS NOT NULL")
	if err != nil {
		return r, err
	}
	inChanged := map[string]bool{}
	for _, ns := range changed {
		inChanged[ns] = true
	}

	// The day's rows of each changed namespace, one namespace at a time:
	// the changed hours' rows of the namespace and its existing partition,
	// DISTINCT, straight to the partition. Rows of a re-folded hour that
	// the day already has fold away; late rows are new. The objects are
	// sorted by namespace, so a namespace's read skips the others' row
	// groups, and the engine holds one namespace's rows at a time.
	r.rows = map[string]int64{}
	written := map[string][]tierObject{} // table: the partitions this rollup wrote
	for _, t := range []string{"logs", "metrics"} {
		partitions, err := c.partitions(ctx, t, day)
		if err != nil {
			return r, err
		}
		namespaces, err := queryStrings(ctx, conn, "SELECT DISTINCT namespace FROM "+hourly[t]+" WHERE namespace IS NOT NULL")
		if err != nil {
			return r, err
		}
		for ns := range partitions {
			if inChanged[ns] {
				namespaces = append(namespaces, ns)
			}
		}
		slices.Sort(namespaces)
		for _, ns := range slices.Compact(namespaces) {
			src := "SELECT * FROM " + hourly[t] + " WHERE namespace = " + sqlString(ns)
			// a namespace's partition keeps the key it was first written with
			o, ok := partitions[ns]
			if ok {
				src += " UNION ALL BY NAME " + c.readTier([]tierObject{o}, "hive_partitioning = false, ")
			} else {
				o = tierName(c.l.dailyPartitionStem(t, day, ns), c.keyID)
			}
			n, runs, err := c.writeSorted(ctx, conn, t, src, o.key)
			if err != nil {
				return r, err
			}
			r.rows[t] += n
			r.runs += runs
			written[t] = append(written[t], o)
		}
	}
	// podindex and apiserver are one object each for the day
	for _, t := range []string{"podindex", "apiserver"} {
		o, ok, err := c.locate(ctx, c.l.dailyStem(t, day))
		if err != nil {
			return r, err
		}
		src := "SELECT * FROM " + hourly[t]
		if ok {
			src += " UNION ALL BY NAME " + c.readTier([]tierObject{o}, "")
		}
		var runs int
		if r.rows[t], runs, err = c.writeSorted(ctx, conn, t, src, o.key); err != nil {
			return r, err
		}
		r.runs += runs
		written[t] = []tierObject{o}
	}

	// pods, recomputed for the changed namespaces from the objects just
	// written, under the names pods.sql reads (a temp view takes precedence
	// over the view of its name): logs and metrics hold those namespaces'
	// day, podindex the whole day. The other namespaces' rows are kept,
	// and so is no row of a pod_uid that was recomputed.
	for _, t := range []string{"logs", "metrics", "podindex"} {
		q := "SELECT * FROM empty_" + t
		if len(written[t]) > 0 {
			q += " UNION ALL BY NAME " + c.readTier(written[t], "hive_partitioning = false, ")
		}
		if err := c.exec(ctx, conn, "CREATE TEMP VIEW "+t+" AS "+q); err != nil {
			return r, err
		}
	}
	if err := c.exec(ctx, conn, "CREATE TEMP TABLE pods_changed AS SELECT * FROM pods p "+
		"WHERE EXISTS (SELECT 1 FROM changed_namespaces n WHERE n.namespace IS NOT DISTINCT FROM p.namespace)"); err != nil {
		return r, err
	}
	pods := "SELECT * FROM pods_changed"
	podsObj, ok, err := c.locate(ctx, c.l.dailyStem("pods", day))
	if err != nil {
		return r, err
	} else if ok {
		pods += " UNION ALL BY NAME SELECT * FROM (" + c.readTier([]tierObject{podsObj}, "") + ") o " +
			"WHERE NOT EXISTS (SELECT 1 FROM changed_namespaces n WHERE n.namespace IS NOT DISTINCT FROM o.namespace) " +
			"AND NOT EXISTS (SELECT 1 FROM pods_changed p WHERE p.pod_uid = o.pod_uid)"
	}
	if r.rows["pods"], err = c.write(ctx, conn, "pods", pods, podsObj.key); err != nil {
		return r, err
	}
	r.namespaces = len(changed)

	if c.beforeCommit != nil {
		if err := c.beforeCommit(dayKeys); err != nil {
			return r, err
		}
	}
	_, err = c.put(ctx, conn, dayKeys, func(to string) string {
		return "COPY (SELECT DISTINCT key FROM (SELECT key FROM hour_keys UNION ALL SELECT key FROM day_keys) ORDER BY key) TO " +
			to + " (FORMAT csv, HEADER true, COMPRESSION zstd)"
	}, func(path string) string {
		return "read_csv(" + path + ", header = true, compression = 'zstd', columns = {'key': 'VARCHAR'})"
	})
	return r, err
}

func hourList(hours []time.Time) string {
	hh := make([]string, len(hours))
	for i, h := range hours {
		hh[i] = h.Format("15")
	}
	return strings.Join(hh, ",")
}

// runColumn is the column of each table's sort order that the rollup
// splits a large write by: the first after namespace, which a daily
// partition holds one of.
var runColumn = map[string]string{"logs": "pod", "metrics": "name", "podindex": "pod_uid", "apiserver": "api_group"}

// sortRows is how many rows the rollup sorts at once for an engine memory
// limit: about a kilobyte per row, which a metrics row's DISTINCT and sort
// take with room to spare (a row is ~190 bytes in the engine). No limit
// sorts every write whole.
func sortRows(memoryLimit int64) int64 {
	return memoryLimit / 1024
}

// writeSorted writes the DISTINCT rows of src, sorted, to key. It reads
// src once, into a Parquet file in scratch, which everything after reads:
// the bucket sees one read of each input per write, however it is sorted.
// When src has more than sortRows rows it sorts them in runs: consecutive
// ranges of runColumn's values, each DISTINCT and sorted on its own into
// another file, then copied to key in order. The engine holds one run at a
// time. Two rows in different runs differ in runColumn, so the runs'
// DISTINCT is the whole DISTINCT, and their concatenation is the whole
// sort. A value with more rows than sortRows is one run of its own. The
// files in scratch are encrypted as the object is.
func (c *compactor) writeSorted(ctx context.Context, conn *sql.Conn, table, src, key string) (rows int64, runs int, err error) {
	col := runColumn[table]
	if c.sortRows <= 0 || col == "" {
		rows, err = c.write(ctx, conn, table, "SELECT DISTINCT * FROM ("+src+")", key)
		return rows, 0, err
	}
	dir, err := os.MkdirTemp(c.scratch, "rollup-")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(dir)
	opts, read := "FORMAT parquet, COMPRESSION zstd", "read_parquet(%s)"
	if id := tierKeyID(key); id != "" {
		opts += ", ENCRYPTION_CONFIG " + encryptionConfig(id)
		read = "read_parquet(%s, encryption_config = " + encryptionConfig(id) + ")"
	}
	input := filepath.Join(dir, "input.parquet")
	q := fmt.Sprintf("COPY (%s) TO %s (%s, ROW_GROUP_SIZE 20000)", src, sqlString(input), opts)
	res, err := conn.ExecContext(ctx, q)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", firstLine(q), err)
	}
	total, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	from := fmt.Sprintf(read, sqlString(input))
	if total <= c.sortRows {
		rows, err = c.write(ctx, conn, table, "SELECT DISTINCT * FROM "+from, key)
		return rows, 0, err
	}

	// the ranges, in the sort order's: NULL sorts last, in a run of its own
	type value struct {
		v sql.NullString
		n int64
	}
	var values []value
	vq, err := conn.QueryContext(ctx, "SELECT "+col+", count(*) FROM "+from+" GROUP BY ALL ORDER BY 1")
	if err != nil {
		return 0, 0, fmt.Errorf("runs of %s: %w", key, err)
	}
	for vq.Next() {
		var v value
		if err := vq.Scan(&v.v, &v.n); err != nil {
			vq.Close()
			return 0, 0, err
		}
		values = append(values, v)
	}
	if err := errors.Join(vq.Err(), vq.Close()); err != nil {
		return 0, 0, err
	}
	var preds []string
	for i := 0; i < len(values); {
		if !values[i].v.Valid {
			preds = append(preds, col+" IS NULL")
			i++
			continue
		}
		j, n := i, values[i].n
		for j+1 < len(values) && values[j+1].v.Valid && n+values[j+1].n <= c.sortRows {
			j++
			n += values[j].n
		}
		preds = append(preds, col+" BETWEEN "+sqlString(values[i].v.String)+" AND "+sqlString(values[j].v.String))
		i = j + 1
	}
	files := make([]string, len(preds))
	for i, p := range preds {
		files[i] = filepath.Join(dir, fmt.Sprintf("run-%05d.parquet", i))
		if err := c.exec(ctx, conn, fmt.Sprintf("COPY (SELECT * FROM (SELECT DISTINCT * FROM %s WHERE %s) ORDER BY %s) TO %s (%s)",
			from, p, sortOrder[table], sqlString(files[i]), opts)); err != nil {
			return 0, 0, err
		}
	}
	// the runs in order: one read of the files, in list order, row by row
	if err := c.exec(ctx, conn, "SET preserve_insertion_order = true"); err != nil {
		return 0, 0, err
	}
	defer c.exec(context.WithoutCancel(ctx), conn, "SET preserve_insertion_order = false")
	rows, err = c.writeOrdered(ctx, conn, table, "SELECT * FROM "+fmt.Sprintf(read, listLiteral(files)), key)
	return rows, len(files), err
}

// partitions returns a table's namespace partitions of a day by namespace,
// each under the key it was first written with.
func (c *compactor) partitions(ctx context.Context, table string, day time.Time) (map[string]tierObject, error) {
	objs, err := c.tiers.List(ctx, c.l.dailyPartitionPrefix(table, day))
	if err != nil {
		return nil, err
	}
	out := map[string]tierObject{}
	for _, o := range objs {
		ns, ok := partitionNamespace(o.Key)
		if !ok {
			continue
		}
		t, ok := stemObject(c.l.dailyPartitionStem(table, day, ns), o.Key)
		if !ok {
			continue
		}
		if prev, dup := out[ns]; dup {
			return nil, fmt.Errorf("namespace %q of %s has two objects, %s and %s: the compactor keeps one, under its first key",
				ns, c.l.dailyPartitionPrefix(table, day), prev.key, t.key)
		}
		out[ns] = t
	}
	return out, nil
}

func queryStrings(ctx context.Context, conn *sql.Conn, q string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstLine(q), err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
