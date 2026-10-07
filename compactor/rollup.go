package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// rollupResult is what one rollup of a day did.
type rollupResult struct {
	day        time.Time
	hours      []time.Time // the changed hours
	namespaces int         // namespaces whose daily rows were rewritten
	rows       map[string]int64
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
// per namespace; pods is recomputed for those namespaces from the day's
// tables, the other namespaces' pods rows are kept; the daily sidecar, the
// union of the day's keys, is written last. It reads no raw objects, and no
// hourly objects but the changed hours'.
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

	// Typed views over a raw object with no lines: every table below starts
	// from its schema, so a table with no input is empty, not an error.
	if err := setRawFiles(ctx, conn, []string{c.empty}); err != nil {
		return r, err
	}
	if err := loadSQL(ctx, conn); err != nil {
		return r, err
	}
	for _, t := range foldTables {
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
		q := fmt.Sprintf("CREATE TEMP TABLE hourly_%s AS SELECT * EXCLUDE (filename) FROM %s", t, t)
		if len(objs) > 0 {
			q += " UNION ALL BY NAME " + c.readTier(objs, "")
		}
		if err := c.exec(ctx, conn, q); err != nil {
			return r, err
		}
	}
	if err := c.exec(ctx, conn, "CREATE TEMP TABLE changed_namespaces AS SELECT DISTINCT namespace FROM ("+
		"SELECT namespace FROM hourly_logs UNION ALL SELECT namespace FROM hourly_metrics "+
		"UNION ALL SELECT namespace FROM hourly_podindex)"); err != nil {
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

	// The day's rows of the changed namespaces: the changed hours' rows and
	// the existing partitions, DISTINCT. Rows of a re-folded hour that the
	// day already has fold away; late rows are new.
	partitions := map[string]map[string]tierObject{} // table, namespace
	for _, t := range []string{"logs", "metrics"} {
		if partitions[t], err = c.partitions(ctx, t, day); err != nil {
			return r, err
		}
		var objs []tierObject
		for ns, o := range partitions[t] {
			if inChanged[ns] {
				objs = append(objs, o)
			}
		}
		if err := c.unionDistinct(ctx, conn, t, objs, "hive_partitioning = false, "); err != nil {
			return r, err
		}
	}
	podindex, ok, err := c.locate(ctx, c.l.dailyStem("podindex", day))
	if err != nil {
		return r, err
	}
	var objs []tierObject
	if ok {
		objs = append(objs, podindex)
	}
	if err := c.unionDistinct(ctx, conn, "podindex", objs, ""); err != nil {
		return r, err
	}
	// objects, like podindex, is one object for the day
	objectsObj, ok, err := c.locate(ctx, c.l.dailyStem("objects", day))
	if err != nil {
		return r, err
	}
	objs = nil
	if ok {
		objs = append(objs, objectsObj)
	}
	if err := c.unionDistinct(ctx, conn, "objects", objs, ""); err != nil {
		return r, err
	}

	r.rows = map[string]int64{}
	for _, t := range []string{"logs", "metrics"} {
		namespaces, err := queryStrings(ctx, conn, "SELECT DISTINCT namespace FROM "+t+" WHERE namespace IS NOT NULL ORDER BY 1")
		if err != nil {
			return r, err
		}
		for _, ns := range namespaces {
			// a namespace's partition keeps the key it was first written with
			o, ok := partitions[t][ns]
			if !ok {
				o = tierName(c.l.dailyPartitionStem(t, day, ns), c.keyID)
			}
			if err := c.write(ctx, conn, t, "SELECT * FROM "+t+" WHERE namespace = "+sqlString(ns), o.key); err != nil {
				return r, err
			}
		}
		if r.rows[t], err = c.count(ctx, conn, t); err != nil {
			return r, err
		}
	}
	if err := c.write(ctx, conn, "podindex", "SELECT * FROM podindex", podindex.key); err != nil {
		return r, err
	}
	if r.rows["podindex"], err = c.count(ctx, conn, "podindex"); err != nil {
		return r, err
	}
	if err := c.write(ctx, conn, "objects", "SELECT * FROM objects", objectsObj.key); err != nil {
		return r, err
	}
	if r.rows["objects"], err = c.count(ctx, conn, "objects"); err != nil {
		return r, err
	}

	// pods, recomputed from the day's tables for the changed namespaces;
	// podindex holds the whole day, logs and metrics those namespaces. The
	// other namespaces' rows are kept, and so is no row of a pod_uid that
	// was recomputed.
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
	if err := c.exec(ctx, conn, "CREATE TEMP TABLE pods_day AS "+pods); err != nil {
		return r, err
	}
	if err := c.write(ctx, conn, "pods", "SELECT * FROM pods_day", podsObj.key); err != nil {
		return r, err
	}
	if r.rows["pods"], err = c.count(ctx, conn, "pods_day"); err != nil {
		return r, err
	}
	r.namespaces = len(changed)

	if c.beforeCommit != nil {
		if err := c.beforeCommit(dayKeys); err != nil {
			return r, err
		}
	}
	return r, c.put(ctx, conn, dayKeys, func(to string) string {
		return "COPY (SELECT DISTINCT key FROM (SELECT key FROM hour_keys UNION ALL SELECT key FROM day_keys) ORDER BY key) TO " +
			to + " (FORMAT csv, HEADER true, COMPRESSION zstd)"
	}, func(path string) string {
		return "read_csv(" + path + ", header = true, compression = 'zstd', columns = {'key': 'VARCHAR'})"
	})
}

// unionDistinct creates the temp table t from hourly_t and the objects.
func (c *compactor) unionDistinct(ctx context.Context, conn *sql.Conn, t string, objs []tierObject, opts string) error {
	q := fmt.Sprintf("CREATE TEMP TABLE %s AS SELECT DISTINCT * FROM (SELECT * FROM hourly_%s", t, t)
	if len(objs) > 0 {
		q += " UNION ALL BY NAME " + c.readTier(objs, opts)
	}
	return c.exec(ctx, conn, q+")")
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
