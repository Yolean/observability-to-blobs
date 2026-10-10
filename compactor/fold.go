package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// compactor folds one cluster's raw tier into its hourly and daily tiers.
// Its state is the bucket's: the sidecars say what is folded, so every
// start is an ordinary tick.
type compactor struct {
	l         layout
	raw       RawSource
	tiers     TierStore
	db        *sql.DB
	grace     time.Duration
	rawDelete bool
	threads   int
	retries   int // http_retries; 0 when httpfs is not loaded
	log       *slog.Logger
	m         *metrics
	// empty is a raw object with no lines: the tables' schema for a
	// rollup that finds no input for one of them
	empty string
	// beforeCommit, when set, runs after a fold's or a rollup's table
	// writes and before its sidecar; tests fail there as a crash would
	beforeCommit func(sidecar string) error
	// copied, when set, runs after a COPY to a temporary key and before
	// the object there is read back; tests damage it there
	copied func(tmp string)
	// finalSeen is the latest previous hour seen final: a cache that spares
	// the rolling pass a sidecar read, never state
	finalSeen time.Time
	// foldedKeys, with the put endpoint on, has every sidecar the folds
	// read or commit, for the endpoint's PUTs of keys already folded
	foldedKeys *foldedKeys

	// Parquet encryption (encryption.go): the keys directory, "" without
	// one; the active key id, "" to write plain objects; the keys the
	// engine has; and the refusal to fold that checkKeys sets while a key
	// is missing.
	keysDir string
	keyID   string
	keys    parquetKeys
	refusal error

	// scratch is the local directory for the engine's spill and the
	// rollup's sorted runs; sortRows is how many rows the rollup sorts at
	// once (rollup.go, writeSorted), 0 for no limit
	scratch  string
	sortRows int64
}

func newCompactor(c config, log *slog.Logger) (*compactor, error) {
	raw, tiers, err := c.stores()
	if err != nil {
		return nil, err
	}
	httpfs, init := c.engineInit()
	db, err := open(httpfs, c.tempDir, init...)
	if err != nil {
		return nil, err
	}
	retries := 0
	if httpfs != "" {
		retries = c.httpRetries
	}
	cp := &compactor{l: layout{c.cluster}, raw: raw, tiers: tiers, db: db, grace: c.grace,
		rawDelete: c.rawDelete, threads: c.threads, retries: retries, log: log, m: newMetrics(),
		keysDir: c.parquetKeysDir, keyID: c.parquetKeyID, sortRows: sortRows(c.memoryLimit)}
	if sp, ok := raw.(*spool); ok {
		cp.m.spool = sp.stats
	}
	scratch := c.tempDir
	if scratch == "" {
		scratch = os.TempDir()
	}
	if err := cp.init(context.Background(), scratch); err != nil {
		db.Close()
		return nil, err
	}
	return cp, nil
}

// init makes the engine's connections single-use, so a job's temp tables
// and variables go with its connection, and writes the empty raw object.
// It gives the engine the parquet keys, and with an active key, writes and
// reads an encrypted object in scratch.
func (c *compactor) init(ctx context.Context, scratch string) error {
	c.db.SetMaxIdleConns(0)
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return err
	}
	c.scratch = scratch
	c.empty = filepath.Join(scratch, "blobs-compactor-empty.jsonl.zst")
	if _, err := c.db.ExecContext(ctx, `COPY (SELECT 'logs' AS "table" WHERE false) TO `+sqlString(c.empty)+
		` (FORMAT json, COMPRESSION zstd)`); err != nil {
		return err
	}
	if err := c.loadKeys(ctx); err != nil {
		return err
	}
	if _, ok := c.keys[c.keyID]; c.keyID != "" && !ok {
		return fmt.Errorf("PARQUET_KEY_ID %s: PARQUET_KEYS_DIR %s has no key file %s", c.keyID, c.keysDir, c.keyID)
	}
	return c.keyCheck(ctx, scratch)
}

func (c *compactor) Close() error { return c.db.Close() }

// conn is one job's connection, with the job settings: DuckDB's threads
// set its request rate against the bucket, http_retries how it rides out
// throttling (compactor/README.md, "Request rate is a job setting").
func (c *compactor) conn(ctx context.Context) (*sql.Conn, error) {
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	stmts := []string{
		"SET TimeZone = 'UTC'",
		"SET preserve_insertion_order = false",
		fmt.Sprintf("SET threads = %d", c.threads),
	}
	if c.retries > 0 {
		stmts = append(stmts, fmt.Sprintf("SET http_retries = %d", c.retries))
	}
	for _, s := range stmts {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%s: %w", s, err)
		}
	}
	return conn, nil
}

func (c *compactor) exec(ctx context.Context, conn *sql.Conn, q string) error {
	if _, err := conn.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("%s: %w", firstLine(q), err)
	}
	return nil
}

func firstLine(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexByte(q, '\n'); i > 0 {
		q = q[:i] + " ..."
	}
	if len(q) > 300 {
		q = q[:300] + " ..."
	}
	return q
}

// write COPYs query, sorted the table's way, to key, encrypted with the
// parquet key its name carries, if any, and returns the rows written.
func (c *compactor) write(ctx context.Context, conn *sql.Conn, table, query, key string) (int64, error) {
	return c.writeOrdered(ctx, conn, table, "SELECT * FROM ("+query+") ORDER BY "+sortOrder[table], key)
}

// writeOrdered is write of a query whose rows are in the table's order.
func (c *compactor) writeOrdered(ctx context.Context, conn *sql.Conn, table, query, key string) (int64, error) {
	opts := "FORMAT parquet, COMPRESSION zstd"
	if n := rowGroups[table]; n > 0 {
		opts += fmt.Sprintf(", ROW_GROUP_SIZE %d", n)
	}
	read := readParquet
	if id := tierKeyID(key); id != "" {
		opts += ", ENCRYPTION_CONFIG " + encryptionConfig(id)
		read = func(path string) string {
			return "read_parquet(" + path + ", encryption_config = " + encryptionConfig(id) + ")"
		}
	}
	return c.put(ctx, conn, key, func(to string) string {
		return fmt.Sprintf("COPY (%s) TO %s (%s)", query, to, opts)
	}, read)
}

func readParquet(path string) string { return "read_parquet(" + path + ")" }

// put writes an object of a tier so that its key only ever holds a whole
// one: copySQL, the COPY to a quoted path, writes to a temporary key; the
// object there must read back, through the FROM clause read returns for
// the path, with as many rows as the COPY reported; only then is it moved
// to key. A COPY that a deadline interrupts can still complete its upload,
// unreadable, which at a tier's key would fail every later read of it;
// here it lands at a temporary key that is deleted.
func (c *compactor) put(ctx context.Context, conn *sql.Conn, key string, copySQL, read func(path string) string) (int64, error) {
	tmp := c.l.tmp(key)
	wrote, err := c.putTmp(ctx, conn, tmp, copySQL, read)
	if err == nil {
		err = c.tiers.Move(ctx, tmp, key)
	}
	if err != nil {
		// best effort, also after a deadline; the past pass deletes what
		// this misses
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if derr := c.tiers.Delete(dctx, []string{tmp}); derr != nil {
			c.log.Warn("delete temporary object", "key", tmp, "err", derr)
		}
		return 0, fmt.Errorf("write %s: %w", key, err)
	}
	return wrote, nil
}

func (c *compactor) putTmp(ctx context.Context, conn *sql.Conn, tmp string, copySQL, read func(path string) string) (int64, error) {
	if err := c.tiers.Prepare(tmp); err != nil {
		return 0, err
	}
	path := sqlString(c.tiers.URL(tmp))
	q := copySQL(path)
	res, err := conn.ExecContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", firstLine(q), err)
	}
	wrote, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if c.copied != nil {
		c.copied(tmp)
	}
	var n int64
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM "+read(path)).Scan(&n); err != nil {
		return 0, fmt.Errorf("read back %s: %w", tmp, err)
	}
	if n != wrote {
		return 0, fmt.Errorf("read back %s: %d rows, the COPY wrote %d", tmp, n, wrote)
	}
	return wrote, nil
}

// sweep deletes the tiers' temporary objects uploaded more than age before
// now: what a job left that failed or was killed between its COPY and the
// move, and whose own delete missed it. A job's own temporary object is
// younger than its deadline, so age is JOB_TIMEOUT.
func (c *compactor) sweep(ctx context.Context, now time.Time, age time.Duration) error {
	var old []string
	for _, p := range c.l.tmpPrefixes() {
		objs, err := c.tiers.List(ctx, p)
		if err != nil {
			return err
		}
		for _, o := range objs {
			if o.Modified.Before(now.Add(-age)) {
				old = append(old, o.Key)
			}
		}
	}
	if len(old) == 0 {
		return nil
	}
	if err := c.tiers.Delete(ctx, old); err != nil {
		return err
	}
	c.log.Info("sweep", "deleted", len(old))
	return nil
}

func (c *compactor) count(ctx context.Context, conn *sql.Conn, table string) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	return n, err
}

// foldResult is what one fold of an hour did.
type foldResult struct {
	hour    time.Time
	keys    int  // raw keys read
	full    bool // the read of every key at H+1h+GRACE
	final   bool // the hour's final fold has committed, now or before
	wrote   bool
	deleted []string
	rows    map[string]int64
	raw     rawSummary // the hour's listing, less deleted
}

// fold folds one hour (compactor/README.md, "A fold of one hour"). listed
// is the hour's raw listing, or nil to list it here.
//
// Until the hour is final a fold takes the keys its sidecar lacks, reads
// them once, and unions their rows with the hour's existing objects. The
// first fold at or after H+1h+GRACE is the full read: every key, no union,
// and the sidecar marks those keys final. After that, folds of the hour are
// incremental again, for late arrivals, and never full reads.
func (c *compactor) fold(ctx context.Context, h time.Time, listed []Object, now time.Time) (foldResult, error) {
	r := foldResult{hour: h}
	if listed == nil {
		all, err := c.raw.List(ctx, c.l.rawHourPrefix(h))
		if err != nil {
			return r, err
		}
		for _, o := range all {
			if oh, ok := c.l.rawHour(o.Key); ok && oh.Equal(h) {
				listed = append(listed, o)
			}
		}
	}
	conn, err := c.conn(ctx)
	if err != nil {
		return r, err
	}
	defer conn.Close()
	sidecar := c.l.hourlySidecar(h)
	folded, err := c.readFolded(ctx, conn, sidecar)
	if err != nil {
		return r, err
	}
	c.foldedKeys.set(h, folded)
	for _, f := range folded {
		r.final = r.final || f
	}
	inListing := make(map[string]bool, len(listed))
	for _, o := range listed {
		inListing[o.Key] = true
	}

	var keys []string
	union := true
	if !r.final && len(listed) > 0 && !now.Before(h.Add(time.Hour+c.grace)) {
		r.full = true
		for _, o := range listed {
			keys = append(keys, o.Key)
		}
		// The full read replaces the hour's objects, which repairs what
		// incremental folds got wrong; but only while raw still holds
		// every key those objects may hold rows of. If a folded key is
		// gone (lifecycle), or there is no sidecar to say which keys the
		// objects hold (it expired, or the first fold crashed before it),
		// replacing could lose rows, so it unions like an incremental fold.
		union = false
		for k := range folded {
			if !inListing[k] {
				union = true
				break
			}
		}
		if len(folded) == 0 {
			if union, err = c.hasHourly(ctx, h); err != nil {
				return r, err
			}
		}
	} else {
		for _, o := range listed {
			if _, ok := folded[o.Key]; !ok {
				keys = append(keys, o.Key)
			}
		}
	}

	if len(keys) > 0 {
		r.keys = len(keys)
		if r.rows, err = c.foldKeys(ctx, conn, h, keys, union); err != nil {
			return r, err
		}
		next := make(map[string]bool, len(folded)+len(keys))
		for k, f := range folded {
			next[k] = f
		}
		for _, k := range keys {
			next[k] = next[k] || r.full
		}
		if c.beforeCommit != nil {
			if err := c.beforeCommit(sidecar); err != nil {
				return r, err
			}
		}
		if err := c.writeFolded(ctx, conn, sidecar, next); err != nil {
			return r, err
		}
		// before any deletion below: a PUT that finds a file gone finds
		// its key there
		c.foldedKeys.set(h, next)
		folded = next
		r.wrote = true
		r.final = r.final || r.full
	}

	// Raw deletion: only keys a committed sidecar lists, only once the
	// hour's final fold has committed. Also here when nothing was folded,
	// for a deletion that a crash interrupted after the commit.
	if c.rawDelete && r.final {
		var del []string
		for _, o := range listed {
			if _, ok := folded[o.Key]; ok {
				del = append(del, o.Key)
			}
		}
		if len(del) > 0 {
			if err := c.raw.Delete(ctx, del); err != nil {
				return r, fmt.Errorf("delete raw of %s: %w", h.Format(dateLayout+"T15"), err)
			}
			r.deleted = del
		}
	}
	r.raw = summarize(listed, r.deleted)
	return r, nil
}

// hasHourly tells whether any of the hour's objects exists.
func (c *compactor) hasHourly(ctx context.Context, h time.Time) (bool, error) {
	for _, t := range append(foldTables, "pods") {
		_, ok, err := c.locate(ctx, c.l.hourlyStem(t, h))
		if ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}

// foldKeys reads keys once, unions their rows with the hour's existing
// objects when union is set, and writes the hour's four objects.
func (c *compactor) foldKeys(ctx context.Context, conn *sql.Conn, h time.Time, keys []string, union bool) (map[string]int64, error) {
	urls := make([]string, len(keys))
	for i, k := range keys {
		urls[i] = c.raw.URL(k)
	}
	if err := setRawFiles(ctx, conn, urls); err != nil {
		return nil, err
	}
	if err := loadSQL(ctx, conn); err != nil {
		return nil, err
	}
	// the new objects, read once; the table views read this table now
	if err := c.exec(ctx, conn, "CREATE TEMP TABLE raw AS SELECT * FROM raw"); err != nil {
		return nil, err
	}
	// The hour's objects, each under the key it was first written with.
	objs := map[string]tierObject{}
	exists := map[string]bool{}
	for _, t := range append(foldTables, "pods") {
		o, ok, err := c.locate(ctx, c.l.hourlyStem(t, h))
		if err != nil {
			return nil, err
		}
		objs[t], exists[t] = o, ok
	}
	// The tier's rows under the views' names, so pods counts deduplicated
	// rows. Each statement reads the view of its name: the temp table
	// exists only once it completes. The existing object is read into
	// memory here, before anything overwrites it.
	for _, t := range foldTables {
		q := fmt.Sprintf("CREATE TEMP TABLE %s AS SELECT DISTINCT * EXCLUDE (filename) FROM %s", t, t)
		if union && exists[t] {
			q = fmt.Sprintf("CREATE TEMP TABLE %s AS SELECT DISTINCT * FROM (SELECT * EXCLUDE (filename) FROM %s "+
				"UNION ALL BY NAME %s)", t, t, c.readTier([]tierObject{objs[t]}, ""))
		}
		if err := c.exec(ctx, conn, q); err != nil {
			return nil, err
		}
	}
	rows := map[string]int64{}
	for _, t := range append(foldTables, "pods") {
		n, err := c.write(ctx, conn, t, "SELECT * FROM "+t, objs[t].key)
		if err != nil {
			return nil, err
		}
		rows[t] = n
	}
	return rows, nil
}

// The hourly sidecar is a CSV with a header, one row per raw key folded
// into the hour: key, and final, true for the keys the hour's final fold
// read. An hour is final when any row is. A missing sidecar is no keys.
func (c *compactor) readFolded(ctx context.Context, conn *sql.Conn, key string) (map[string]bool, error) {
	return readFolded(ctx, conn, c.tiers, key)
}

func readFolded(ctx context.Context, conn *sql.Conn, tiers TierStore, key string) (map[string]bool, error) {
	folded := map[string]bool{}
	ok, err := tiers.Stat(ctx, key)
	if err != nil || !ok {
		return folded, err
	}
	rows, err := conn.QueryContext(ctx, "SELECT key, final FROM read_csv("+sqlString(tiers.URL(key))+
		", header = true, columns = {'key': 'VARCHAR', 'final': 'BOOLEAN'})")
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var f bool
		if err := rows.Scan(&k, &f); err != nil {
			return nil, err
		}
		folded[k] = f
	}
	return folded, rows.Err()
}

func (c *compactor) writeFolded(ctx context.Context, conn *sql.Conn, key string, folded map[string]bool) error {
	keys := make([]string, 0, len(folded))
	for k := range folded {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	quoted := make([]string, len(keys))
	finals := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = sqlString(k)
		finals[i] = fmt.Sprint(folded[k])
	}
	if err := c.exec(ctx, conn, "SET VARIABLE folded_keys = ["+strings.Join(quoted, ", ")+"]::VARCHAR[]"); err != nil {
		return err
	}
	if err := c.exec(ctx, conn, "SET VARIABLE folded_final = ["+strings.Join(finals, ", ")+"]::BOOLEAN[]"); err != nil {
		return err
	}
	_, err := c.put(ctx, conn, key, func(to string) string {
		return "COPY (SELECT * FROM (SELECT unnest(getvariable('folded_keys')) AS key, " +
			"unnest(getvariable('folded_final')) AS final) ORDER BY key) TO " + to + " (FORMAT csv, HEADER true)"
	}, func(path string) string {
		return "read_csv(" + path + ", header = true, columns = {'key': 'VARCHAR', 'final': 'BOOLEAN'})"
	})
	return err
}
