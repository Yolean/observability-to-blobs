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
		var files []string
		for _, h := range r.hours {
			key := c.l.hourly(t, h)
			ok, err := c.tiers.Stat(ctx, key)
			if err != nil {
				return r, err
			}
			if ok {
				files = append(files, c.tiers.URL(key))
			}
		}
		q := fmt.Sprintf("CREATE TEMP TABLE hourly_%s AS SELECT * EXCLUDE (filename) FROM %s", t, t)
		if len(files) > 0 {
			q += " UNION ALL BY NAME SELECT * FROM read_parquet(" + listLiteral(files) + ", union_by_name = true)"
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
	for _, t := range []string{"logs", "metrics"} {
		objs, err := c.tiers.List(ctx, c.l.dailyPartitionPrefix(t, day))
		if err != nil {
			return r, err
		}
		var files []string
		for _, o := range objs {
			if ns, ok := partitionNamespace(o.Key); ok && inChanged[ns] && o.Key == c.l.dailyPartition(t, day, ns) {
				files = append(files, c.tiers.URL(o.Key))
			}
		}
		if err := c.unionDistinct(ctx, conn, t, files, "hive_partitioning = false, "); err != nil {
			return r, err
		}
	}
	var files []string
	if ok, err := c.tiers.Stat(ctx, c.l.daily("podindex", day)); err != nil {
		return r, err
	} else if ok {
		files = append(files, c.tiers.URL(c.l.daily("podindex", day)))
	}
	if err := c.unionDistinct(ctx, conn, "podindex", files, ""); err != nil {
		return r, err
	}

	r.rows = map[string]int64{}
	for _, t := range []string{"logs", "metrics"} {
		namespaces, err := queryStrings(ctx, conn, "SELECT DISTINCT namespace FROM "+t+" WHERE namespace IS NOT NULL ORDER BY 1")
		if err != nil {
			return r, err
		}
		for _, ns := range namespaces {
			if err := c.write(ctx, conn, t, "SELECT * FROM "+t+" WHERE namespace = "+sqlString(ns), c.l.dailyPartition(t, day, ns)); err != nil {
				return r, err
			}
		}
		if r.rows[t], err = c.count(ctx, conn, t); err != nil {
			return r, err
		}
	}
	if err := c.write(ctx, conn, "podindex", "SELECT * FROM podindex", c.l.daily("podindex", day)); err != nil {
		return r, err
	}
	if r.rows["podindex"], err = c.count(ctx, conn, "podindex"); err != nil {
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
	podsKey := c.l.daily("pods", day)
	if ok, err := c.tiers.Stat(ctx, podsKey); err != nil {
		return r, err
	} else if ok {
		pods += " UNION ALL BY NAME SELECT * FROM read_parquet(" + sqlString(c.tiers.URL(podsKey)) + ") o " +
			"WHERE NOT EXISTS (SELECT 1 FROM changed_namespaces n WHERE n.namespace IS NOT DISTINCT FROM o.namespace) " +
			"AND NOT EXISTS (SELECT 1 FROM pods_changed p WHERE p.pod_uid = o.pod_uid)"
	}
	if err := c.exec(ctx, conn, "CREATE TEMP TABLE pods_day AS "+pods); err != nil {
		return r, err
	}
	if err := c.write(ctx, conn, "pods", "SELECT * FROM pods_day", podsKey); err != nil {
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
	if err := c.tiers.Prepare(dayKeys); err != nil {
		return r, err
	}
	return r, c.exec(ctx, conn, "COPY (SELECT DISTINCT key FROM (SELECT key FROM hour_keys UNION ALL SELECT key FROM day_keys) ORDER BY key) TO "+
		sqlString(c.tiers.URL(dayKeys))+" (FORMAT csv, HEADER true, COMPRESSION zstd)")
}

// unionDistinct creates the temp table t from hourly_t and the files.
func (c *compactor) unionDistinct(ctx context.Context, conn *sql.Conn, t string, files []string, opts string) error {
	q := fmt.Sprintf("CREATE TEMP TABLE %s AS SELECT DISTINCT * FROM (SELECT * FROM hourly_%s", t, t)
	if len(files) > 0 {
		q += " UNION ALL BY NAME SELECT * FROM read_parquet(" + listLiteral(files) + ", " + opts + "union_by_name = true)"
	}
	return c.exec(ctx, conn, q+")")
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
