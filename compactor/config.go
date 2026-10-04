package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The cadence's bounds. With RAW_DELETE=after-final an hour's raw objects
// go once its final fold commits, at the first rolling tick after
// H+1h+GRACE, so a raw object lives at most 1h + GRACE + ROLLING_INTERVAL
// plus one fold from its upload: at most 1h35m plus the fold. A late key,
// one that arrives after its hour is final, goes at the next past pass:
// PAST_INTERVAL plus that pass. Both stay under 2h, the bound the raw-age
// alert reads, while a fold takes under 25m. ROLLING_INTERVAL at most 5m
// also keeps the rolling alert (10m without a success) from firing between
// two healthy passes.
const (
	maxGrace   = 30 * time.Minute
	maxRolling = 5 * time.Minute
	maxPast    = time.Hour
)

// config is the compactor's environment (compactor/README.md, "Manifests").
type config struct {
	cluster string

	// the bucket, or a local directory instead of one
	bucket, endpoint, region string
	tls                      bool
	keyID, secret            string
	dir                      string

	rolling, grace, past, jobTimeout time.Duration
	rawDelete                        bool

	// job settings: DuckDB's request rate is a function of its threads
	threads, httpRetries int
	memoryLimit          int64 // bytes DuckDB may use, 75% of DUCKDB_MEMORY_LIMIT
	tempDir              string

	metricsAddr string
	httpfs      string

	// parquet encryption: the keys directory, and the key of new objects
	parquetKeysDir, parquetKeyID string

	// the raw source: "bucket" (raw1/ in the bucket, or BLOBS_DIR), or
	// "spool", the directory spoolDir that the put endpoint fills
	rawSource string
	spoolDir  string
	// the put endpoint (put.go): its address, "" for none, the key pair
	// the agents sign with (in S3_REGION, for S3_BUCKET), and the largest
	// body it takes
	putListen        string
	putKeyID, putKey string
	putMaxBytes      int64
}

// defaultPutMaxBytes bounds a PutObject's body (PUT_MAX_BYTES). out_s3
// uploads once its buffer reaches total_file_size, 64M in the agent's
// config, and compresses the buffer, so its objects stay below it with a
// margin of as much again. The body is streamed to a file in the spool's
// _tmp/ as it is hashed, never held in memory, so the bound is on disk.
const defaultPutMaxBytes = 128 << 20

func configFromEnv() (config, error) { return parseConfig(os.Getenv) }

func parseConfig(env func(string) string) (config, error) {
	get := func(k, def string) string {
		if v := env(k); v != "" {
			return v
		}
		return def
	}
	c := config{
		cluster:        env("CLUSTER_NAME"),
		bucket:         env("S3_BUCKET"),
		region:         get("S3_REGION", "us-east-1"),
		keyID:          env("AWS_ACCESS_KEY_ID"),
		secret:         env("AWS_SECRET_ACCESS_KEY"),
		dir:            env("BLOBS_DIR"),
		tempDir:        env("DUCKDB_TEMP_DIRECTORY"),
		metricsAddr:    get("METRICS_ADDR", ":9090"),
		httpfs:         httpfsPath(),
		parquetKeysDir: env("PARQUET_KEYS_DIR"),
		parquetKeyID:   env("PARQUET_KEY_ID"),
		rawSource:      get("RAW_SOURCE", "bucket"),
		spoolDir:       env("SPOOL_DIR"),
		putListen:      env("PUT_LISTEN"),
		putKeyID:       env("PUT_ACCESS_KEY_ID"),
		putKey:         env("PUT_SECRET_ACCESS_KEY"),
	}
	if !clusterRE.MatchString(c.cluster) {
		return c, fmt.Errorf("CLUSTER_NAME %q: want a lowercase name of letters, digits, '-', '_' and '.'", c.cluster)
	}
	// the endpoint is host[:port]; a scheme, as the agent's env carries it, is dropped
	ep := env("S3_ENDPOINT")
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	c.endpoint = strings.TrimSuffix(ep, "/")
	switch get("S3_TLS", "on") {
	case "on", "true":
		c.tls = true
	case "off", "false":
	default:
		return c, fmt.Errorf("S3_TLS %q: want on or off", env("S3_TLS"))
	}
	if c.dir == "" {
		for k, v := range map[string]string{"S3_BUCKET": c.bucket, "S3_ENDPOINT": c.endpoint,
			"AWS_ACCESS_KEY_ID": c.keyID, "AWS_SECRET_ACCESS_KEY": c.secret} {
			if v == "" {
				return c, fmt.Errorf("%s is required (or BLOBS_DIR for a local directory)", k)
			}
		}
	}
	var err error
	durations := []struct {
		name string
		def  string
		dst  *time.Duration
	}{
		{"ROLLING_INTERVAL", "2m", &c.rolling},
		{"GRACE", "10m", &c.grace},
		{"PAST_INTERVAL", "1h", &c.past},
		{"JOB_TIMEOUT", "30m", &c.jobTimeout},
	}
	for _, d := range durations {
		if *d.dst, err = time.ParseDuration(get(d.name, d.def)); err != nil {
			return c, fmt.Errorf("%s: %w", d.name, err)
		}
		if *d.dst < 0 || (*d.dst == 0 && d.name != "GRACE") {
			return c, fmt.Errorf("%s %s: must be positive", d.name, *d.dst)
		}
	}
	for _, b := range []struct {
		name     string
		v, limit time.Duration
	}{{"GRACE", c.grace, maxGrace}, {"ROLLING_INTERVAL", c.rolling, maxRolling}, {"PAST_INTERVAL", c.past, maxPast}} {
		if b.v > b.limit {
			return c, fmt.Errorf("%s %s: at most %s, so that with RAW_DELETE=after-final a raw object is deleted within 2h of its upload", b.name, b.v, b.limit)
		}
	}
	// The spool is a buffer, not storage: its files go once their hour's
	// final fold has committed, so spool mode deletes by default and
	// refuses RAW_DELETE=off.
	rawDelete := "off"
	switch c.rawSource {
	case "bucket":
		if c.spoolDir != "" {
			return c, fmt.Errorf("SPOOL_DIR %s: only with RAW_SOURCE=spool", c.spoolDir)
		}
	case "spool":
		rawDelete = "after-final"
		if !filepath.IsAbs(c.spoolDir) {
			return c, fmt.Errorf("RAW_SOURCE=spool: SPOOL_DIR %q must be an absolute path", c.spoolDir)
		}
	default:
		return c, fmt.Errorf("RAW_SOURCE %q: want bucket or spool", c.rawSource)
	}
	switch get("RAW_DELETE", rawDelete) {
	case "off":
		if c.rawSource == "spool" {
			return c, fmt.Errorf("RAW_DELETE off: RAW_SOURCE=spool deletes its files after their hour's final fold, want after-final")
		}
	case "after-final":
		c.rawDelete = true
	default:
		return c, fmt.Errorf("RAW_DELETE %q: want off or after-final", env("RAW_DELETE"))
	}
	if c.putListen != "" {
		if c.rawSource != "spool" {
			return c, fmt.Errorf("PUT_LISTEN %s: only with RAW_SOURCE=spool, which the fold reads what it takes from", c.putListen)
		}
		for k, v := range map[string]string{"PUT_ACCESS_KEY_ID": c.putKeyID, "PUT_SECRET_ACCESS_KEY": c.putKey,
			"S3_BUCKET": c.bucket} {
			if v == "" {
				return c, fmt.Errorf("PUT_LISTEN %s: %s is required", c.putListen, k)
			}
		}
		c.putMaxBytes = defaultPutMaxBytes
		if v := env("PUT_MAX_BYTES"); v != "" {
			if c.putMaxBytes, err = strconv.ParseInt(v, 10, 64); err != nil || c.putMaxBytes < 1 {
				return c, fmt.Errorf("PUT_MAX_BYTES %q: want bytes", v)
			}
		}
	}
	ints := []struct {
		name string
		def  string
		dst  *int
	}{
		{"DUCKDB_THREADS", "4", &c.threads},
		{"DUCKDB_HTTP_RETRIES", "5", &c.httpRetries},
	}
	for _, i := range ints {
		if *i.dst, err = strconv.Atoi(get(i.name, i.def)); err != nil || *i.dst < 1 {
			return c, fmt.Errorf("%s %q: want a positive integer", i.name, env(i.name))
		}
	}
	if c.parquetKeyID != "" {
		if !keyIDRE.MatchString(c.parquetKeyID) {
			return c, fmt.Errorf("PARQUET_KEY_ID %q: want a key id, %s", c.parquetKeyID, keyIDRE)
		}
		if c.parquetKeysDir == "" {
			return c, fmt.Errorf("PARQUET_KEY_ID %s: PARQUET_KEYS_DIR is required", c.parquetKeyID)
		}
	}
	// the container's memory limit in bytes, from resourceFieldRef; the rest
	// is the Go heap, the HTTP client and DuckDB's own overhead
	if v := env("DUCKDB_MEMORY_LIMIT"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("DUCKDB_MEMORY_LIMIT %q: want bytes", v)
		}
		c.memoryLimit = n / 4 * 3
	}
	return c, nil
}

// stores returns the raw source and the tier store the config names: one
// bucket for both, or one local directory; with RAW_SOURCE=spool the raw
// source is the spool instead.
func (c config) stores() (RawSource, TierStore, error) {
	var raw RawSource
	var tiers TierStore
	if c.dir != "" {
		d := dirBucket{root: c.dir}
		raw, tiers = d, d
	} else {
		b := &s3Bucket{bucket: c.bucket, endpoint: c.endpoint, tls: c.tls, region: c.region,
			keyID: c.keyID, secret: c.secret, http: &http.Client{Timeout: 5 * time.Minute}, now: time.Now}
		raw, tiers = b, b
	}
	if c.rawSource == "spool" {
		s, err := openSpool(c.spoolDir)
		if err != nil {
			return nil, nil, err
		}
		raw = s
	}
	return raw, tiers, nil
}

// engineInit is what every connection of the engine runs first: the
// extension, the bucket's secret, and the settings that hold for every job.
func (c config) engineInit() (httpfs string, init []initStmt) {
	init = append(init, initStmt{sql: "SET enable_external_file_cache = false"})
	if c.memoryLimit > 0 {
		init = append(init, initStmt{sql: fmt.Sprintf("SET memory_limit = '%dB'", c.memoryLimit)})
	}
	if c.tempDir != "" {
		init = append(init, initStmt{sql: "SET temp_directory = " + sqlString(c.tempDir)})
	}
	if c.dir != "" {
		// httpfs is for the bucket, and for encrypted writes: without it
		// the engine's crypto is read-only (sqlfiles_test.go). A keys
		// directory without an active key still rewrites encrypted objects
		if c.parquetKeysDir == "" {
			return "", init
		}
		return c.httpfs, init
	}
	init = append(init, initStmt{label: "CREATE SECRET (the bucket's)", redact: []string{c.secret}, sql: fmt.Sprintf(
		"CREATE OR REPLACE SECRET blobs (TYPE s3, PROVIDER config, KEY_ID %s, SECRET %s, REGION %s, ENDPOINT %s, URL_STYLE 'path', USE_SSL %t, SCOPE %s)",
		sqlString(c.keyID), sqlString(c.secret), sqlString(c.region), sqlString(c.endpoint), c.tls, sqlString("s3://"+c.bucket))})
	return c.httpfs, init
}
