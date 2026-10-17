package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	"github.com/klauspost/compress/zstd"
)

// The tables of the hourly and daily tiers (compactor/README.md,
// "Compaction"); logs and metrics are partitioned by namespace in the
// daily tier, the others are one object per day.
var tierTables = []string{"logs", "metrics", "podindex", "pods", "apiserver"}

func dailyPartitioned(t string) bool { return t == "logs" || t == "metrics" }

// The views over the raw tier, all of which read raw (tables.sql,
// pods.sql). apiserver_at is the macro of apiserver_at.sql.
var rawViews = []string{"raw", "logs", "metrics", "podindex", "apiserver", "pods", "apiserver_at"}

// window is the time range a command reads, UTC, from inclusive.
type window struct{ from, to time.Time }

// hours are the UTC hours the window touches, oldest first.
func (w window) hours() []time.Time {
	var hs []time.Time
	for h := w.from.Truncate(time.Hour); h.Before(w.to); h = h.Add(time.Hour) {
		hs = append(hs, h)
	}
	return hs
}

// days are the UTC dates the window touches, oldest first.
func (w window) days() []string {
	var ds []string
	for d := w.from.Truncate(24 * time.Hour); d.Before(w.to); d = d.Add(24 * time.Hour) {
		ds = append(ds, d.Format("2006-01-02"))
	}
	return ds
}

func (w window) String() string {
	return w.from.Format("2006-01-02T15:04:05Z") + ".." + w.to.Format("2006-01-02T15:04:05Z")
}

// sessionFlags are the flags every command that reads the bucket shares.
type sessionFlags struct {
	context, bucket, cluster, node, since, from, to, blobsDir, keysDir string
	maxObjects                                                         int
	explain                                                            bool
}

func (f *sessionFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.context, "context", "", "the bucket, cluster and AWS profile to read (y-obs contexts); default Y_OBS_CONTEXT, else the config's current")
	fs.StringVar(&f.bucket, "bucket", "", "the bucket (env S3_BUCKET); default the context's")
	fs.StringVar(&f.cluster, "cluster", os.Getenv("CLUSTER_NAME"), "the cluster, the first key segment of every tier (env CLUSTER_NAME); default the context's, else the bucket's only one")
	fs.StringVar(&f.node, "node", "", "read only this node's raw objects")
	fs.StringVar(&f.since, "since", "1h", "the window: this long back from now, or from --from (30m, 6h, 2d)")
	fs.StringVar(&f.from, "from", "", "window start, UTC: 2026-10-11T09:00:00Z or '2026-10-11 09:00'")
	fs.StringVar(&f.to, "to", "", "window end, UTC; default now, or --from plus --since")
	fs.StringVar(&f.blobsDir, "blobs-dir", os.Getenv("BLOBS_DIR"), "read a local directory laid out like the bucket instead of S3 (env BLOBS_DIR)")
	fs.StringVar(&f.keysDir, "parquet-keys-dir", os.Getenv("PARQUET_KEYS_DIR"), "key files for encrypted tiers, as the compactor mounts them (env PARQUET_KEYS_DIR)")
	fs.IntVar(&f.maxObjects, "max-objects", 200, "refuse a plan costing more: a tier object counts 1, a raw object a tenth")
	fs.BoolVar(&f.explain, "explain", false, "print the plan, the objects each view would read, and read nothing")
}

// parseTime takes RFC 3339 or "2006-01-02 15:04[:05]", always UTC.
func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time: use 2026-10-11T09:00:00Z or '2026-10-11 09:00' (UTC)", s)
}

// parseAt reads --at: empty or "now" is now, "-30m" or "-2d" that long
// before now, else a time as parseTime takes it.
func parseAt(s string, now time.Time) (time.Time, error) {
	switch {
	case s == "" || s == "now":
		return now.UTC(), nil
	case strings.HasPrefix(s, "-"):
		d, err := parseSince(s[1:])
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("%q is not a time: use now, -30m, -2d, 2026-10-11T09:00:00Z or '2026-10-11 09:00' (UTC)", s)
		}
		return now.UTC().Add(-d), nil
	}
	t, err := parseTime(s)
	if err != nil {
		return t, fmt.Errorf("%q is not a time: use now, -30m, -2d, 2026-10-11T09:00:00Z or '2026-10-11 09:00' (UTC)", s)
	}
	return t, nil
}

// parseSince is time.ParseDuration plus days: 2d, 1d12h.
func parseSince(s string) (time.Duration, error) {
	if m := regexp.MustCompile(`^(\d+)d(.*)$`).FindStringSubmatch(s); m != nil {
		days, _ := strconv.Atoi(m[1])
		rest := time.Duration(0)
		if m[2] != "" {
			var err error
			if rest, err = time.ParseDuration(m[2]); err != nil {
				return 0, fmt.Errorf("--since %q: %v", s, err)
			}
		}
		return time.Duration(days)*24*time.Hour + rest, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--since %q: use 30m, 6h or 2d", s)
	}
	return d, nil
}

func (f *sessionFlags) window(now time.Time) (window, error) {
	since, err := parseSince(f.since)
	if err != nil {
		return window{}, err
	}
	var w window
	switch {
	case f.from != "" && f.to != "":
		if w.from, err = parseTime(f.from); err != nil {
			return w, err
		}
		if w.to, err = parseTime(f.to); err != nil {
			return w, err
		}
	case f.from != "":
		if w.from, err = parseTime(f.from); err != nil {
			return w, err
		}
		w.to = w.from.Add(since)
	case f.to != "":
		if w.to, err = parseTime(f.to); err != nil {
			return w, err
		}
		w.from = w.to.Add(-since)
	default:
		w.to = now.UTC()
		w.from = w.to.Add(-since)
	}
	if !w.from.Before(w.to) {
		return w, fmt.Errorf("the window %s is empty: --from must be before --to", w)
	}
	return w, nil
}

// exitError carries an exit code (README.md, "Design rules for LLM
// callers") with a one-line message.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

const (
	exitOK       = 0
	exitNoRows   = 1
	exitUsage    = 2
	exitAuth     = 3
	exitNotFound = 4
	exitBudget   = 5
	exitStore    = 6
)

func fail(code int, format string, a ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, a...)}
}

// session is one engine connection with the bucket's secret, the views
// and the variables set: views and secrets are the database's, variables
// the connection's, and raw.sql reads its file list from a variable.
type session struct {
	db           *sql.DB
	conn         *sql.Conn
	root         string // s3://bucket or a local directory
	store        storeConfig
	local        string // the .duckdb file attached as local, if any
	cluster      string
	w            window
	node         string
	keyIDs       map[string]bool
	redact       []string
	tmp          string
	plan         map[string][]string // view -> the objects it reads
	wplan        *windowPlan         // the window views' plan (planner.go)
	promql       bool                // hints speak of matchers, for remote reads
	narrow       string              // what narrows the command's rows, for the --limit note
	listRequests int                 // the bucket listings y-obs made itself
	warnings     []string
	// secretStmt is the current CREATE SECRET, read by every new
	// connection's init; a renewal replaces it
	secretMu   sync.Mutex
	secretStmt string
	stopRenew  func()
}

// storeConfig is where the bucket is and the credential that reads it
// (contexts.go, storeFor).
type storeConfig struct {
	bucket, endpoint, region, keyID, secret, token string
	tls                                            bool
	// source is where the credential came from, for messages; profile is
	// the one it was resolved for, its key in the process's cache
	source, profile string
	// expires is the credential's expiry, zero for static keys; it is
	// renewed margin before
	expires time.Time
	margin  time.Duration
}

// clusterFor is the cluster a session reads: the flag (or CLUSTER_NAME),
// else the context's, else the bucket's only cluster, which list finds.
func clusterFor(ctx context.Context, flagValue, contextCluster string, list func(context.Context) ([]string, error)) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if contextCluster != "" {
		return contextCluster, nil
	}
	if list == nil {
		return "", fail(exitUsage, "no cluster: pass --cluster or set CLUSTER_NAME")
	}
	clusters, err := list(ctx)
	if err != nil {
		return "", err
	}
	if len(clusters) == 1 {
		return clusters[0], nil
	}
	hint := ": the bucket has none"
	if len(clusters) > 1 {
		hint = ": the bucket has " + strings.Join(clusters, ", ")
	}
	return "", fail(exitUsage, "no cluster: pass --cluster, set CLUSTER_NAME, or name one in the context%s", hint)
}

// defaultMemoryLimit bounds the engine's memory (Y_OBS_MEMORY_LIMIT).
const defaultMemoryLimit = "2GB"

// threadsPerGB scales the engine's threads with its memory limit: each
// thread reserves its own buffers, so the limit, not the CPU count, bounds
// them. Under 2 GB, pods on a 30-minute window ran out of memory at 32
// threads and not at 16; exits on a day was 3.5x slower at 8 than at 32.
const threadsPerGB = 8

// engineThreads is the engine's thread count (Y_OBS_THREADS), by default
// threadsPerGB per GB of the memory limit and at most the CPU count.
func engineThreads(limit string) int {
	if n, err := strconv.Atoi(os.Getenv("Y_OBS_THREADS")); err == nil && n > 0 {
		return n
	}
	n := 1
	if b, ok := parseBytes(limit); ok {
		n = max(1, int(b*threadsPerGB/1e9))
	}
	return min(runtime.NumCPU(), n)
}

// parseBytes reads a DuckDB memory size such as 2GB, 512MiB or 1.5 GB.
func parseBytes(v string) (float64, bool) {
	v = strings.ToUpper(strings.TrimSpace(v))
	i := strings.IndexFunc(v, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i <= 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(v[:i], 64)
	if err != nil {
		return 0, false
	}
	unit := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40}[strings.TrimSpace(v[i:])]
	return n * unit, unit > 0
}

// openSession opens the engine. With blobsDir the tiers are read from a
// local directory and no extension is loaded; otherwise httpfs is loaded,
// from HTTPFS_EXTENSION when set, else installed from DuckDB's signed
// repository, and the secret is scoped to the bucket.
func openSession(ctx context.Context, f *sessionFlags, w window) (*session, error) {
	var st storeConfig
	var contextCluster string
	if f.blobsDir == "" {
		var err error
		if st, contextCluster, err = storeFor(ctx, f.context, f.bucket); err != nil {
			return nil, err
		}
	}
	s := &session{w: w, node: f.node, keyIDs: map[string]bool{}, plan: map[string][]string{}, store: st}
	var init []string
	if f.blobsDir != "" {
		abs, err := filepath.Abs(f.blobsDir)
		if err != nil {
			return nil, err
		}
		s.root = abs
	} else {
		s.root = "s3://" + st.bucket
		switch p := os.Getenv("HTTPFS_EXTENSION"); {
		case p != "":
			init = append(init, "LOAD "+sqlString(p))
		case len(httpfsExtension) > 0:
			p, err := embeddedHTTPFS()
			if err != nil {
				return nil, fail(exitStore, "the embedded httpfs extension: %v", err)
			}
			init = append(init, "LOAD "+sqlString(p))
		default:
			init = append(init, "INSTALL httpfs", "LOAD httpfs")
		}
		s.setSecret(st)
	}
	var list func(context.Context) ([]string, error)
	if f.blobsDir == "" {
		list = s.clusters
	}
	var err error
	if s.cluster, err = clusterFor(ctx, f.cluster, contextCluster, list); err != nil {
		return nil, err
	}
	init = append(init, "SET TimeZone = 'UTC'", "SET enable_object_cache = true", "SET enable_http_metadata_cache = true")
	if f.keysDir != "" {
		stmts, err := s.parquetKeys(f.keysDir)
		if err != nil {
			return nil, err
		}
		init = append(init, stmts...)
	}
	if err := s.openEngine(ctx, init); err != nil {
		return nil, err
	}
	if !st.expires.IsZero() {
		s.renewEvery(func(ctx context.Context) (storeConfig, error) {
			st, _, err := storeFor(ctx, f.context, f.bucket)
			return st, err
		}, st.expires, st.margin)
	}
	if s.tmp, err = os.MkdirTemp("", "y-obs-"); err != nil {
		s.close()
		return nil, err
	}
	// a bounded engine that spills to the session's directory: a two-day
	// window ran to 4.4 GB of memory without a limit
	limit := os.Getenv("Y_OBS_MEMORY_LIMIT")
	if limit == "" {
		limit = defaultMemoryLimit
	}
	for _, q := range []string{"SET memory_limit = " + sqlString(limit), "SET temp_directory = " + sqlString(s.tmp), "SET preserve_insertion_order = false", fmt.Sprintf("SET threads = %d", engineThreads(limit))} {
		if err := s.exec(ctx, q); err != nil {
			s.close()
			return nil, err
		}
	}
	return s, nil
}

// embeddedHTTPFS writes the embedded extension once into the user's cache,
// under its checksum, and returns its path: DuckDB loads extensions from
// files, and verifies the signature when it does.
func embeddedHTTPFS() (string, error) {
	sum := sha256.Sum256(httpfsExtension)
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "y-obs", "httpfs-"+hex.EncodeToString(sum[:8]))
	p := filepath.Join(dir, "httpfs.duckdb_extension")
	if fi, err := os.Stat(p); err == nil && fi.Size() == int64(len(httpfsExtension)) {
		return p, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "httpfs-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(httpfsExtension); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return p, os.Rename(tmp.Name(), p)
}

// openEngine opens an in-memory engine with autoinstall and autoload
// off, runs init on its connection, and pins that one connection.
func (s *session) openEngine(ctx context.Context, init []string) error {
	c, err := duckdb.NewConnector("", func(e driver.ExecerContext) error {
		stmts := append([]string{"SET autoinstall_known_extensions = false", "SET autoload_known_extensions = false"}, init...)
		s.secretMu.Lock()
		if s.secretStmt != "" {
			stmts = append(stmts, s.secretStmt)
		}
		s.secretMu.Unlock()
		for _, q := range stmts {
			if _, err := e.ExecContext(ctx, q, nil); err != nil {
				return s.wrap(err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.db = sql.OpenDB(c)
	if s.conn, err = s.db.Conn(ctx); err != nil {
		s.db.Close()
		return s.wrap(err)
	}
	return nil
}

// setSecret makes st's credential the one new connections create, and
// keeps its secret out of error messages.
func (s *session) setSecret(st storeConfig) string {
	q := fmt.Sprintf("CREATE OR REPLACE SECRET blobs (TYPE s3, PROVIDER config, KEY_ID %s, SECRET %s, ENDPOINT %s, REGION %s, URL_STYLE 'path', USE_SSL %t, SCOPE %s",
		sqlString(st.keyID), sqlString(st.secret), sqlString(st.endpoint), sqlString(st.region), st.tls, sqlString(s.root))
	if st.token != "" {
		q += ", SESSION_TOKEN " + sqlString(st.token)
	}
	q += ")"
	s.secretMu.Lock()
	s.secretStmt = q
	s.redact = append(s.redact, st.secret, st.token)
	s.secretMu.Unlock()
	return q
}

// renewEvery replaces the secret margin before its expiry, for a session
// that outlives its credential: secrets are the database's, so a statement
// on a second connection reaches the pinned one, and httpfs re-reads the
// secret when a request is refused (httpfs_enable_credential_refresh).
func (s *session) renewEvery(get func(context.Context) (storeConfig, error), expires time.Time, margin time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	s.stopRenew = cancel
	go func() {
		for {
			wait := time.Until(expires.Add(-margin))
			select {
			case <-ctx.Done():
				return
			case <-time.After(max(wait, time.Second)):
			}
			st, err := get(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "y-obs: renewing the credential: %v\n", err)
				return
			}
			q := s.setSecret(st)
			c, err := s.db.Conn(ctx)
			if err == nil {
				_, err = c.ExecContext(ctx, q)
				c.Close()
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "y-obs: replacing the secret: %v\n", s.wrap(err))
				return
			}
			expires, margin = st.expires, st.margin
			fmt.Fprintf(os.Stderr, "y-obs: credential renewed, expires %s\n", expires.UTC().Format(time.RFC3339))
		}
	}()
}

func (s *session) close() {
	if s.stopRenew != nil {
		s.stopRenew()
	}
	if s.conn != nil {
		s.conn.Close()
	}
	if s.db != nil {
		s.db.Close()
	}
	if s.tmp != "" {
		os.RemoveAll(s.tmp)
	}
}

// wrap blanks secrets out of an engine error and classifies it.
func (s *session) wrap(err error) error {
	if err == nil {
		return nil
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return err
	}
	msg := err.Error()
	for _, r := range s.redact {
		if r != "" {
			msg = strings.ReplaceAll(msg, r, "<redacted>")
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	// DuckDB echoes the statement and appends advice; the cause comes first
	for _, cut := range []string{" LINE 1:", " Authentication Failure - "} {
		if i := strings.Index(msg, cut); i > 0 {
			msg = msg[:i]
		}
	}
	switch {
	case strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "SignatureDoesNotMatch") || strings.Contains(msg, "InvalidAccessKeyId") || strings.Contains(msg, "ExpiredToken"):
		source := s.store.source
		if source == "" {
			source = "the AWS default chain"
		}
		forgetStore(s.store.profile)
		return fail(exitAuth, "the store refused the credentials of %s: %s", source, msg)
	case strings.Contains(msg, "Out of Memory Error"):
		if i := strings.Index(msg, " Possible solutions:"); i > 0 {
			msg = msg[:i]
		}
		return fail(exitBudget, "%s; narrow the window or the query, or raise Y_OBS_MEMORY_LIMIT (threads follow it, or set Y_OBS_THREADS lower)", msg)
	case strings.Contains(msg, "Parser Error"), strings.Contains(msg, "Binder Error"):
		return fail(exitUsage, "%s; y-obs schema lists the views and their columns", msg)
	case strings.Contains(msg, "Catalog Error"):
		return fail(exitNotFound, "%s; y-obs schema lists the views", msg)
	case strings.Contains(msg, "HTTP"), strings.Contains(msg, "IO Error"):
		return fail(exitStore, "%s", msg)
	}
	return fail(exitStore, "%s", msg)
}

var keyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// parquetKeys reads key files as the compactor mounts them
// (compactor/README.md, "Encryption").
func (s *session) parquetKeys(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fail(exitUsage, "--parquet-keys-dir: %v", err)
	}
	var stmts []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		id := e.Name()
		if !keyIDPattern.MatchString(id) {
			return nil, fail(exitUsage, "--parquet-keys-dir: %s is not a key id", id)
		}
		b, err := os.ReadFile(filepath.Join(dir, id))
		if err != nil {
			return nil, err
		}
		key := strings.Join(strings.Fields(string(b)), "")
		stmts = append(stmts, fmt.Sprintf("PRAGMA add_parquet_key(%s, %s)", sqlString(id), sqlString(key)))
		s.keyIDs[id] = true
		s.redact = append(s.redact, key)
	}
	return stmts, nil
}

// glob lists the objects matching a pattern under the root.
func (s *session) glob(ctx context.Context, pattern string) ([]string, error) {
	rows, err := s.conn.QueryContext(ctx, "SELECT file FROM glob(?) ORDER BY file", pattern)
	if err != nil {
		return nil, s.wrap(err)
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, s.wrap(rows.Err())
}

// referenced returns the views a query names, and the views they read.
func referenced(query string) map[string]bool {
	ref := map[string]bool{}
	word := func(name string) bool {
		return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`).MatchString(query)
	}
	for _, v := range rawViews {
		if word(v) {
			ref["raw"] = true
		}
	}
	for _, t := range tierTables {
		for _, tier := range []string{"hourly_", "daily_"} {
			if word(tier + t) {
				ref[tier+t] = true
			}
		}
	}
	return ref
}

// planRaw lists the window's raw objects, hour by hour: an hour-first key
// layout lists one hour per request.
func (s *session) planRaw(ctx context.Context) error {
	node := "*"
	if s.node != "" {
		node = s.node
	}
	var files []string
	// from the hour of the margin before the window: the previous hour's
	// last object holds the window's first rows when it starts on the hour
	for _, h := range (window{from: s.w.from.Add(-rawBefore), to: s.w.to}).hours() {
		fs, err := s.glob(ctx, fmt.Sprintf("%s/raw1/%s/%s/%s/*.jsonl.zst", s.root, s.cluster, h.Format("2006-01-02/15"), node))
		if err != nil {
			return err
		}
		files = append(files, fs...)
	}
	s.plan["raw"] = s.rawInWindow(files)
	return nil
}

// A raw key's MMSS is when the agent opened the object, which it uploads
// within UPLOAD_TIMEOUT (30 s by default). Objects opened from rawBefore
// before the window to rawAfter past it hold its rows: the margin after
// takes lines that arrived late, after an outage of up to rawAfter.
const (
	rawBefore = 2 * time.Minute
	rawAfter  = 5 * time.Minute
)

var rawKeyTime = regexp.MustCompile(`/(\d{4}-\d{2}-\d{2})/(\d{2})/[^/]+/(\d{2})(\d{2})-[^/]*$`)

// rawInWindow keeps the raw objects opened in the window, with its
// margins; a key without the time is kept.
func (s *session) rawInWindow(files []string) []string {
	from, to := s.w.from.Add(-rawBefore), s.w.to.Add(rawAfter)
	var keep []string
	for _, f := range files {
		m := rawKeyTime.FindStringSubmatch(f)
		if m == nil {
			keep = append(keep, f)
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05", m[1]+" "+m[2]+":"+m[3]+":"+m[4])
		if err != nil || !t.Before(from) && t.Before(to) {
			keep = append(keep, f)
		}
	}
	return keep
}

// rawWeight is what a raw object costs against --max-objects: raw objects
// are small and come one per node per upload, so they count a tenth of a
// tier object.
const rawWeight = 10

// cost is the plan against --max-objects.
func (s *session) cost() int {
	n := 0
	for v, fs := range s.plan {
		if v == "raw" {
			n += (len(fs) + rawWeight - 1) / rawWeight
		} else {
			n += len(fs)
		}
	}
	return n
}

// overBudget is exit 5 with the plan and what would shrink it: raw objects
// are per node, the daily tier's logs and metrics per namespace.
func (s *session) overBudget(maxObjects int, what string) error {
	raw := len(s.plan["raw"])
	tier := s.planned() - raw
	var hints []string
	node, ns := "--node NODE", "-n NAMESPACE"
	if s.promql {
		node, ns = `a node="..." matcher`, `a namespace="..." matcher`
	}
	if raw > 0 {
		hints = append(hints, node+" (raw objects are per node)")
	}
	if len(s.plan["daily_logs"])+len(s.plan["daily_metrics"]) > 0 {
		hints = append(hints, ns+" (the daily tier is per namespace)")
	}
	hints = append(hints, "a shorter window", "or a higher --max-objects")
	src := s.sourcesSummary()
	if src != "" {
		src = "; " + src
	}
	return fail(exitBudget, "%s costs %d, over --max-objects %d (%d raw objects at a tenth, %d tier objects: %s%s): %s",
		what, s.cost(), maxObjects, raw, tier, s.planSummary(), src, strings.Join(hints, ", "))
}

var hourlyName = regexp.MustCompile(`/(\d\d)(?:\.k-([a-z0-9-]+))?\.parquet$`)

// planTier lists one table's objects of the hourly or daily tier in the
// window.
func (s *session) planTier(ctx context.Context, tier, table string) error {
	hours := map[string]bool{}
	for _, h := range s.w.hours() {
		hours[h.Format("2006-01-02/15")] = true
	}
	var files []string
	for _, d := range s.w.days() {
		var pattern string
		switch {
		case tier == "hourly":
			pattern = fmt.Sprintf("%s/hourly/%s/%s/%s/*.parquet", s.root, s.cluster, table, d)
		case dailyPartitioned(table):
			pattern = fmt.Sprintf("%s/daily/%s/%s/%s/*/*.parquet", s.root, s.cluster, table, d)
		default:
			pattern = fmt.Sprintf("%s/daily/%s/%s/%s*.parquet", s.root, s.cluster, table, d)
		}
		fs, err := s.glob(ctx, pattern)
		if err != nil {
			return err
		}
		for _, f := range fs {
			if tier == "hourly" {
				m := hourlyName.FindStringSubmatch(f)
				if m == nil || !hours[d+"/"+m[1]] {
					continue
				}
			}
			files = append(files, f)
		}
	}
	s.plan[tier+"_"+table] = files
	return nil
}

// keyOf is the parquet key id an object's name carries, "" when plain.
var keyOfName = regexp.MustCompile(`\.k-([a-z0-9-]+)\.parquet$`)

func keyOf(f string) string {
	if m := keyOfName.FindStringSubmatch(f); m != nil {
		return m[1]
	}
	return ""
}

// prepare plans the views a query references, refuses a plan over budget,
// and creates them. The raw views always exist: with no raw objects in the
// plan they read one empty file, so they have their columns and no rows.
func (s *session) prepare(ctx context.Context, ref map[string]bool, maxObjects int) error {
	if err := s.planRefs(ctx, ref); err != nil {
		return err
	}
	if s.cost() > maxObjects {
		return s.overBudget(maxObjects, "the plan")
	}
	return s.createViews(ctx)
}

// planRefs plans the views a query references, without creating them.
func (s *session) planRefs(ctx context.Context, ref map[string]bool) error {
	if ref["raw"] {
		if err := s.planRaw(ctx); err != nil {
			return err
		}
	}
	for _, t := range tierTables {
		for _, tier := range []string{"hourly", "daily"} {
			if ref[tier+"_"+t] {
				if err := s.planTier(ctx, tier, t); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *session) planned() int {
	n := 0
	for _, fs := range s.plan {
		n += len(fs)
	}
	return n
}

func (s *session) planSummary() string {
	var parts []string
	for _, v := range s.planViews() {
		if len(s.plan[v]) > 0 || v != "raw" || s.wplan == nil {
			parts = append(parts, fmt.Sprintf("%s=%d", v, len(s.plan[v])))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func (s *session) planViews() []string {
	var vs []string
	for v := range s.plan {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return vs
}

func (s *session) exec(ctx context.Context, q string) error {
	_, err := s.conn.ExecContext(ctx, q)
	return s.wrap(err)
}

func (s *session) createViews(ctx context.Context) error {
	raw := s.plan["raw"]
	if len(raw) == 0 {
		empty, err := s.emptyRaw()
		if err != nil {
			return err
		}
		raw = []string{empty}
	}
	if err := s.exec(ctx, "SET VARIABLE raw_files = "+sqlList(raw)); err != nil {
		return err
	}
	for _, f := range []string{"raw.sql", "tables.sql", "pods.sql", "apiserver_at.sql"} {
		b, err := sqlFS.ReadFile("sql/" + f)
		if err != nil {
			return err
		}
		if err := s.exec(ctx, string(b)); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
	}
	for _, v := range s.planViews() {
		if v == "raw" {
			continue
		}
		q, err := s.tierView(v, s.plan[v])
		if err != nil {
			return err
		}
		if q == "" {
			continue
		}
		if err := s.exec(ctx, "CREATE OR REPLACE VIEW "+v+" AS "+q); err != nil {
			return err
		}
	}
	return nil
}

// tierView unites by name one read of the plain objects and one per key
// id: one read takes one key (compactor/README.md, "Encryption").
func (s *session) tierView(view string, files []string) (string, error) {
	byKey := map[string][]string{}
	for _, f := range files {
		byKey[keyOf(f)] = append(byKey[keyOf(f)], f)
	}
	var ids []string
	for id := range byKey {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var reads []string
	for _, id := range ids {
		hive := ""
		if strings.HasPrefix(view, "daily_") && dailyPartitioned(strings.TrimPrefix(view, "daily_")) {
			hive = ", hive_partitioning = false"
		}
		if id == "" {
			reads = append(reads, fmt.Sprintf("SELECT * FROM read_parquet(%s, filename = true, union_by_name = true%s)", sqlList(byKey[id]), hive))
			continue
		}
		if !s.keyIDs[id] {
			s.warnings = append(s.warnings, fmt.Sprintf("%s: %d objects under parquet key %s, which --parquet-keys-dir lacks, are left out", view, len(byKey[id]), id))
			continue
		}
		reads = append(reads, fmt.Sprintf("SELECT * FROM read_parquet(%s, filename = true, encryption_config = {footer_key: %s}%s)", sqlList(byKey[id]), sqlString(id), hive))
	}
	return strings.Join(reads, " UNION ALL BY NAME "), nil
}

// emptyRaw writes a zstd file with no lines: raw.sql then binds its
// pinned columns and reads no rows.
func (s *session) emptyRaw() (string, error) {
	p := filepath.Join(s.tmp, "empty.jsonl.zst")
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		f.Close()
		return "", err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return "", err
	}
	return p, f.Close()
}

// missingViews are the referenced tier views without objects in the
// window, which therefore do not exist.
func (s *session) missingViews(ref map[string]bool) []string {
	var miss []string
	for v := range ref {
		if v != "raw" && len(s.plan[v]) == 0 {
			miss = append(miss, v)
		}
	}
	sort.Strings(miss)
	return miss
}

// sqlString quotes s as a SQL string literal.
func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func sqlList(items []string) string {
	q := make([]string, len(items))
	for i, it := range items {
		q[i] = sqlString(it)
	}
	return "[" + strings.Join(q, ", ") + "]"
}
