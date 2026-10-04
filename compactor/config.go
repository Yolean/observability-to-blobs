package main

import (
	"fmt"
	"net/http"
	"os"
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
}

func configFromEnv() (config, error) { return parseConfig(os.Getenv) }

func parseConfig(env func(string) string) (config, error) {
	get := func(k, def string) string {
		if v := env(k); v != "" {
			return v
		}
		return def
	}
	c := config{
		cluster:     env("CLUSTER_NAME"),
		bucket:      env("S3_BUCKET"),
		region:      get("S3_REGION", "us-east-1"),
		keyID:       env("AWS_ACCESS_KEY_ID"),
		secret:      env("AWS_SECRET_ACCESS_KEY"),
		dir:         env("BLOBS_DIR"),
		tempDir:     env("DUCKDB_TEMP_DIRECTORY"),
		metricsAddr: get("METRICS_ADDR", ":9090"),
		httpfs:      httpfsPath(),
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
	switch get("RAW_DELETE", "off") {
	case "off":
	case "after-final":
		c.rawDelete = true
	default:
		return c, fmt.Errorf("RAW_DELETE %q: want off or after-final", env("RAW_DELETE"))
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
// bucket for both, or one local directory.
func (c config) stores() (RawSource, TierStore) {
	if c.dir != "" {
		d := dirBucket{root: c.dir}
		return d, d
	}
	b := &s3Bucket{bucket: c.bucket, endpoint: c.endpoint, tls: c.tls, region: c.region,
		keyID: c.keyID, secret: c.secret, http: &http.Client{Timeout: 5 * time.Minute}, now: time.Now}
	return b, b
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
		return "", init
	}
	init = append(init, initStmt{label: "CREATE SECRET (the bucket's)", redact: []string{c.secret}, sql: fmt.Sprintf(
		"CREATE OR REPLACE SECRET blobs (TYPE s3, PROVIDER config, KEY_ID %s, SECRET %s, REGION %s, ENDPOINT %s, URL_STYLE 'path', USE_SSL %t, SCOPE %s)",
		sqlString(c.keyID), sqlString(c.secret), sqlString(c.region), sqlString(c.endpoint), c.tls, sqlString("s3://"+c.bucket))})
	return c.httpfs, init
}
