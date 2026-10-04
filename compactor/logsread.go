package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// logsReader finds a pod's lines for the logs API (logs.go,
// compactor/README.md, "The logs API"). A request looks back LOGS_LOOKBACK
// from now: the daily tier for the days in that window that are rolled up,
// the hourly tier for the other hours, and the raw objects of the current
// and the previous hour that their sidecars do not list yet. The pod name
// resolves to its latest incarnation in the pods rows of those, and only
// the logs objects of the hours and days that hold rows of it are read,
// filtered to its namespace, pod and pod_uid, which the tiers' sort order
// and row groups prune to a few row groups.
type logsReader struct {
	l     layout
	tiers TierStore
	// raw is where the unfolded objects are: the spool, raw1/ in the
	// bucket, or nil to read the tiers only
	raw      RawSource
	db       *sql.DB
	keysDir  string
	lookback time.Duration
	now      func() time.Time
	log      *slog.Logger

	mu   sync.Mutex
	keys parquetKeys
}

// newLogsReader opens the API's own engine: a database of its own, so
// that its memory limit (LOGS_MEMORY_LIMIT, from DUCKDB_MEMORY_LIMIT) and
// its spill directory are its own, and a request cannot take the memory of
// a fold.
func newLogsReader(cfg config, raw RawSource, tiers TierStore, log *slog.Logger) (*logsReader, error) {
	ecfg := cfg
	ecfg.memoryLimit = cfg.logsMemoryLimit
	if cfg.tempDir != "" {
		ecfg.tempDir = filepath.Join(cfg.tempDir, "logs")
		if err := os.MkdirAll(ecfg.tempDir, 0o755); err != nil {
			return nil, err
		}
	}
	httpfs, init := ecfg.engineInit()
	init = append(init, initStmt{sql: "SET TimeZone = 'UTC'"}, initStmt{sql: fmt.Sprintf("SET threads = %d", cfg.threads)})
	if httpfs != "" {
		init = append(init, initStmt{sql: fmt.Sprintf("SET http_retries = %d", cfg.httpRetries)})
	}
	db, err := open(httpfs, init...)
	if err != nil {
		return nil, err
	}
	r := &logsReader{l: layout{cfg.cluster}, tiers: tiers, raw: raw, db: db, keysDir: cfg.parquetKeysDir,
		lookback: cfg.logsLookback, now: time.Now, log: log}
	r.init()
	if err := r.loadKeys(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

// init makes the engine's connections single-use: a request's temp tables
// and variables go with its connection.
func (r *logsReader) init() { r.db.SetMaxIdleConns(0) }

func (r *logsReader) Close() error { return r.db.Close() }

// loadKeys gives the engine the keys directory's new and changed keys; a
// request reloads it, so a Secret update reaches the API without a restart.
func (r *logsReader) loadKeys(ctx context.Context) error {
	if r.keysDir == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	keys, err := addParquetKeys(ctx, r.db, r.keysDir, r.keys, r.log)
	if err != nil {
		return err
	}
	r.keys = keys
	return nil
}

// errNoPod: the name has no incarnation in the window.
var errNoPod = errors.New("no pod")

// errNoContainer: the incarnation has no container of that name.
type errNoContainer struct{ known []string }

func (e *errNoContainer) Error() string { return "no container, known: " + strings.Join(e.known, ", ") }

// errStorage is a failed listing ("list") or read ("read") of the tiers or
// the raw objects.
type errStorage struct {
	op  string
	err error
}

func (e *errStorage) Error() string { return e.op + ": " + e.err.Error() }
func (e *errStorage) Unwrap() error { return e.err }

func listErr(err error) error { return &errStorage{"list", err} }
func readErr(err error) error { return &errStorage{"read", err} }

// incarnation is the pod_uid a pod name resolves to, and what is known of
// it: its containers in the order they started, its phase, and where its
// rows are.
type incarnation struct {
	uid        string
	containers []string
	phase      PodPhase
	hours      []time.Time // hourly-tier hours with rows of it
	days       []time.Time // daily-tier days with rows of it
	raw        bool        // unfolded raw objects have rows of it
}

// readLine is one line of a container.
type readLine struct {
	time      time.Time
	container string
	message   string
}

// lineQuery is what the reader may filter in the engine. The handler
// applies the reference semantics again to what comes back (formatOutput),
// so these only narrow the rows to a superset of the answer: the lines
// that match grep, the last tail+1 of them when tail > 0.
type lineQuery struct {
	container string
	grep      string
	stripAnsi bool
	tail      int
}

// plan is the objects a request reads, found by listing.
type plan struct {
	start, now time.Time
	pods       []tierObject             // hourly and daily pods objects
	hourlyLogs map[time.Time]tierObject // by hour
	dailyDays  map[time.Time]bool       // days read from the daily tier
	rawURLs    []string                 // unfolded raw objects
}

// read resolves ns/pod and returns its incarnation and lines. A read
// that fails while raw objects are part of it is tried once more: a fold
// can commit, and in spool mode delete files, between the listing and the
// read.
func (r *logsReader) read(ctx context.Context, ns, pod string, q lineQuery) (*incarnation, []readLine, error) {
	inc, lines, raw, err := r.readOnce(ctx, ns, pod, q)
	var se *errStorage
	if err != nil && raw && errors.As(err, &se) && ctx.Err() == nil {
		r.log.Warn("logs: read again", "namespace", ns, "pod", pod, "err", err)
		inc, lines, _, err = r.readOnce(ctx, ns, pod, q)
	}
	return inc, lines, err
}

func (r *logsReader) readOnce(ctx context.Context, ns, pod string, q lineQuery) (*incarnation, []readLine, bool, error) {
	if err := r.loadKeys(ctx); err != nil {
		return nil, nil, false, readErr(err)
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return nil, nil, false, readErr(err)
	}
	defer conn.Close()
	p, err := r.plan(ctx, conn)
	if err != nil {
		return nil, nil, false, err
	}
	raw := len(p.rawURLs) > 0
	inc, err := r.resolve(ctx, conn, p, ns, pod)
	if err != nil {
		return nil, nil, raw, err
	}
	if q.container != "" && !contains(inc.containers, q.container) {
		return inc, nil, raw, &errNoContainer{inc.containers}
	}
	lines, err := r.lines(ctx, conn, p, ns, pod, inc, q)
	return inc, lines, raw, err
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// plan lists what the window holds. Per day of the window: the daily
// tier's pods object if the day is rolled up and before today, else the
// hourly pods and logs objects of the day's hours in the window. Then the
// current and the previous hour's raw keys that their sidecars lack; the
// sidecar is read before the listing, so a key is either in an object
// read below or among these, or both while a fold commits meanwhile (the
// lines query folds that repeat away).
func (r *logsReader) plan(ctx context.Context, conn *sql.Conn) (*plan, error) {
	now := r.now().UTC()
	p := &plan{start: now.Add(-r.lookback), now: now, hourlyLogs: map[time.Time]tierObject{}, dailyDays: map[time.Time]bool{}}
	today := dayOf(now)
	first := p.start.Truncate(time.Hour)
	for d := dayOf(p.start); !d.After(today); d = d.Add(24 * time.Hour) {
		if d.Before(today) {
			o, ok, err := r.find(ctx, r.l.dailyStem("pods", d))
			if err != nil {
				return nil, err
			}
			if ok {
				p.pods = append(p.pods, o)
				p.dailyDays[d] = true
				continue
			}
		}
		for _, t := range []string{"pods", "logs"} {
			objs, err := r.hourly(ctx, t, d)
			if err != nil {
				return nil, err
			}
			for h, o := range objs {
				if h.Before(first) || h.After(now) {
					continue
				}
				if t == "pods" {
					p.pods = append(p.pods, o)
				} else {
					p.hourlyLogs[h] = o
				}
			}
		}
	}
	if r.raw == nil {
		return p, nil
	}
	cur := now.Truncate(time.Hour)
	for _, h := range []time.Time{cur.Add(-time.Hour), cur} {
		if !h.Add(time.Hour).After(p.start) {
			continue
		}
		folded, err := readFolded(ctx, conn, r.tiers, r.l.hourlySidecar(h))
		if err != nil {
			return nil, readErr(err)
		}
		listed, err := r.raw.List(ctx, r.l.rawHourPrefix(h))
		if err != nil {
			return nil, listErr(err)
		}
		for _, o := range listed {
			if oh, ok := r.l.rawHour(o.Key); ok && oh.Equal(h) {
				if _, ok := folded[o.Key]; !ok {
					p.rawURLs = append(p.rawURLs, r.raw.URL(o.Key))
				}
			}
		}
	}
	return p, nil
}

// find returns stem's one object, plain or under any key.
func (r *logsReader) find(ctx context.Context, stem string) (tierObject, bool, error) {
	objs, err := r.tiers.List(ctx, stem+".")
	if err != nil {
		return tierObject{}, false, listErr(err)
	}
	var found []tierObject
	for _, o := range objs {
		if t, ok := stemObject(stem, o.Key); ok {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 0:
		return tierObject{}, false, nil
	case 1:
		return found[0], true, r.check(found[0])
	default:
		return tierObject{}, false, listErr(fmt.Errorf("%s has %d objects, %v: the compactor keeps one per stem", stem, len(found), found))
	}
}

var hourlyObjectRE = regexp.MustCompile(`/(\d{4}-\d{2}-\d{2})/(\d{2})(?:\.k-[a-z0-9][a-z0-9-]{0,31})?\.parquet$`)

// hourly returns a table's hourly objects of a day by hour: one LIST.
func (r *logsReader) hourly(ctx context.Context, table string, day time.Time) (map[time.Time]tierObject, error) {
	prefix := fmt.Sprintf("hourly/%s/%s/%s/", r.l.cluster, table, day.Format(dateLayout))
	objs, err := r.tiers.List(ctx, prefix)
	if err != nil {
		return nil, listErr(err)
	}
	out := map[time.Time]tierObject{}
	for _, o := range objs {
		m := hourlyObjectRE.FindStringSubmatch(o.Key)
		if m == nil {
			continue
		}
		h, err := time.Parse(dateLayout+"/15", m[1]+"/"+m[2])
		if err != nil {
			continue
		}
		t, ok := stemObject(r.l.hourlyStem(table, h), o.Key)
		if !ok {
			continue
		}
		if prev, dup := out[h]; dup {
			return nil, listErr(fmt.Errorf("%s has two objects, %s and %s: the compactor keeps one", r.l.hourlyStem(table, h), prev.key, t.key))
		}
		if err := r.check(t); err != nil {
			return nil, err
		}
		out[h] = t
	}
	return out, nil
}

// check fails for an object whose parquet key the keys directory lacks,
// naming the key, rather than leave it to the engine's message.
func (r *logsReader) check(o tierObject) error {
	if o.keyID == "" {
		return nil
	}
	r.mu.Lock()
	_, ok := r.keys[o.keyID]
	r.mu.Unlock()
	if !ok {
		return readErr(fmt.Errorf("%s is encrypted with parquet key %s, which PARQUET_KEYS_DIR lacks", o.key, o.keyID))
	}
	return nil
}

// podsCols are the pods columns the resolution reads.
const podsCols = "pod_uid, container, phase, pod_started, container_started, first_log, last_log, last_seen, " +
	"pod_deleted, container_exited"

// resolve finds the name's latest incarnation in the window: of the
// pod_uids that the pods rows of the planned objects and of the unfolded
// raw have for namespace and pod, and whose latest activity is in the
// window, the one that started last (pod_started, else its first container
// start or line).
func (r *logsReader) resolve(ctx context.Context, conn *sql.Conn, p *plan, ns, pod string) (*incarnation, error) {
	where := "namespace = " + sqlString(ns) + " AND pod = " + sqlString(pod)
	var parts []string
	if len(p.pods) > 0 {
		// src is the object's date, and /HH for an hourly one
		parts = append(parts, "SELECT "+podsCols+", regexp_extract(filename, '/pods/([0-9]{4}-[0-9]{2}-[0-9]{2}(/[0-9]{2})?)[.]', 1) AS src FROM ("+
			readTier(r.tiers, p.pods, "filename = true, ")+") WHERE "+where)
	}
	if len(p.rawURLs) > 0 {
		// the raw views over the unfolded objects, this pod's rows read
		// once into the temp table raw, which the views then read
		if err := setRawFiles(ctx, conn, p.rawURLs); err != nil {
			return nil, readErr(err)
		}
		if err := loadSQL(ctx, conn); err != nil {
			return nil, readErr(err)
		}
		if _, err := conn.ExecContext(ctx, "CREATE TEMP TABLE raw AS SELECT * FROM raw WHERE "+where); err != nil {
			return nil, readErr(err)
		}
		parts = append(parts, "SELECT "+podsCols+", 'raw' AS src FROM pods")
	}
	if len(parts) == 0 {
		return nil, errNoPod
	}
	if _, err := conn.ExecContext(ctx, "CREATE TEMP TABLE cand AS "+strings.Join(parts, " UNION ALL BY NAME ")); err != nil {
		return nil, readErr(err)
	}
	start := "TIMESTAMP_NS " + sqlString(p.start.Format("2006-01-02 15:04:05.999999999"))
	var uid string
	err := conn.QueryRowContext(ctx, `SELECT pod_uid FROM cand GROUP BY pod_uid
		HAVING coalesce(max(greatest(last_log, last_seen, pod_deleted, container_exited, container_started, pod_started)), `+start+`) >= `+start+`
		ORDER BY min(coalesce(pod_started, container_started, first_log)) DESC NULLS LAST,
		  max(greatest(last_log, last_seen, pod_deleted, container_exited)) DESC NULLS LAST, pod_uid DESC LIMIT 1`).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoPod
	}
	if err != nil {
		return nil, readErr(err)
	}
	inc := &incarnation{uid: uid}
	u := sqlString(uid)
	if inc.containers, err = queryStrings(ctx, conn, "SELECT container FROM cand WHERE pod_uid = "+u+
		" AND container IS NOT NULL GROUP BY container ORDER BY min(coalesce(container_started, first_log)) NULLS LAST, container"); err != nil {
		return nil, readErr(err)
	}
	srcs, err := queryStrings(ctx, conn, "SELECT DISTINCT src FROM cand WHERE pod_uid = "+u+" ORDER BY src")
	if err != nil {
		return nil, readErr(err)
	}
	daily := true
	for _, s := range srcs {
		switch {
		case s == "raw":
			inc.raw, daily = true, false
		case len(s) == len("2006-01-02/15"):
			h, err := time.Parse(dateLayout+"/15", s)
			if err != nil {
				return nil, readErr(fmt.Errorf("pods object of %q", s))
			}
			inc.hours, daily = append(inc.hours, h), false
		default:
			d, err := time.Parse(dateLayout, s)
			if err != nil {
				return nil, readErr(fmt.Errorf("pods object of %q", s))
			}
			inc.days = append(inc.days, d)
		}
	}
	// the phase of the latest pods row that saw the pod's sandbox go,
	// else running if any row has it running
	var phase sql.NullString
	err = conn.QueryRowContext(ctx, "SELECT phase FROM cand WHERE pod_uid = "+u+
		" AND pod_deleted IS NOT NULL ORDER BY pod_deleted DESC LIMIT 1").Scan(&phase)
	if errors.Is(err, sql.ErrNoRows) {
		err = conn.QueryRowContext(ctx, "SELECT CASE WHEN bool_or(phase = 'running') THEN 'running' END FROM cand WHERE pod_uid = "+u).Scan(&phase)
	}
	if err != nil {
		return nil, readErr(err)
	}
	inc.phase = podPhase(phase.String)
	if daily {
		inc.phase = PodPhaseArchived
	}
	return inc, nil
}

// podPhase maps the pods table's derived phase (compactor/sql/pods.sql)
// to the API's.
func podPhase(p string) PodPhase {
	switch p {
	case "running":
		return PodPhaseRunning
	case "succeeded":
		return PodPhaseSucceeded
	case "failed":
		return PodPhaseFailed
	}
	return PodPhaseUnknown
}

// logsCols are the logs table's columns, which a DISTINCT compares.
const logsCols = "time, cluster, node, namespace, pod, pod_uid, container, restart, stream, unit, priority, message"

// ansiSQL is removeAnsi in the engine: ESC [, parameter and intermediate
// bytes, an optional final byte (logs_test.go holds the two to the same
// results).
const ansiSQL = `regexp_replace(message, '\x1b\[[\x20-\x3f]*[\x40-\x7e]?', '', 'g')`

// grepSQL is matchGrep in the engine, on the message as the handler
// greps it.
func grepSQL(grep string, strip bool) string {
	if grep == "" {
		return "true"
	}
	m := "message"
	if strip {
		m = ansiSQL
	}
	if strings.HasPrefix(grep, "^") {
		return "starts_with(" + m + ", " + sqlString(grep[1:]) + ")"
	}
	return "contains(" + m + ", " + sqlString(grep) + ")"
}

// lines reads the incarnation's lines in the window from the objects of
// the hours and days that hold rows of it, and from the unfolded raw.
// The current and the previous hour's objects and the raw rows go
// through one DISTINCT, which folds away the rows of a key that a fold
// committed while this request listed. With tail > 0 only the last
// tail+1 matching lines leave the engine.
func (r *logsReader) lines(ctx context.Context, conn *sql.Conn, p *plan, ns, pod string, inc *incarnation, q lineQuery) ([]readLine, error) {
	where := "namespace = " + sqlString(ns) + " AND pod = " + sqlString(pod) + " AND pod_uid = " + sqlString(inc.uid) +
		" AND time >= TIMESTAMP_NS " + sqlString(p.start.Format("2006-01-02 15:04:05.999999999"))
	if q.container != "" {
		where += " AND container = " + sqlString(q.container)
	}
	recentFrom := p.now.Truncate(time.Hour).Add(-time.Hour)
	var old, recent []tierObject
	for _, h := range inc.hours {
		o, ok := p.hourlyLogs[h]
		if !ok {
			continue // a pods row of podindex or metrics rows only
		}
		if h.Before(recentFrom) {
			old = append(old, o)
		} else {
			recent = append(recent, o)
		}
	}
	for _, d := range inc.days {
		o, ok, err := r.find(ctx, r.l.dailyPartitionStem("logs", d, ns))
		if err != nil {
			return nil, err
		}
		if ok {
			old = append(old, o)
		}
	}
	sel := func(from string) string { return "SELECT " + logsCols + " FROM (" + from + ") WHERE " + where }
	var parts []string
	if len(old) > 0 {
		parts = append(parts, sel(readTier(r.tiers, old, "hive_partitioning = false, ")))
	}
	var rec []string
	if len(recent) > 0 {
		rec = append(rec, sel(readTier(r.tiers, recent, "")))
	}
	if inc.raw {
		rec = append(rec, sel("SELECT * FROM logs"))
	}
	switch {
	case len(rec) == 1 && !inc.raw:
		parts = append(parts, rec[0])
	case len(rec) > 0:
		parts = append(parts, "SELECT DISTINCT * FROM ("+strings.Join(rec, " UNION ALL ")+")")
	}
	if len(parts) == 0 {
		return nil, nil
	}
	query := "SELECT time, container, message FROM (" + strings.Join(parts, " UNION ALL ") + ") WHERE " +
		grepSQL(q.grep, q.stripAnsi) + " ORDER BY time DESC, container DESC, restart DESC, message DESC"
	if q.tail > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.tail+1)
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, readErr(err)
	}
	defer rows.Close()
	var out []readLine
	for rows.Next() {
		var l readLine
		var container, message sql.NullString
		if err := rows.Scan(&l.time, &container, &message); err != nil {
			return nil, readErr(err)
		}
		l.container, l.message = container.String, message.String
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, readErr(err)
	}
	// oldest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
