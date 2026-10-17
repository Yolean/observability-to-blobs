// blobs-to-prometheus tails the metrics lines of the raw tier into a
// Prometheus remote-write receiver. Run it next to a Prometheus started
// with --web.enable-remote-write-receiver and an out_of_order_time_window
// that covers LOOKBACK: the samples keep their scrape times, and objects
// arrive per node, not in global time order.
//
// It reads raw1/{cluster}/{date}/{HH}/{producer}/{MMSS}-{uuid}.jsonl.zst
// (README.md, "What the agent ships"), of CLUSTER_NAME, or with no
// CLUSTER_NAME of every cluster that has a prefix under raw1/, which every
// pass lists again. Every POLL it LISTs the hour prefixes from LOOKBACK
// ago to now, and pushes every key it has not
// pushed yet, oldest first per producer: the metrics lines' samples, with
// the line's cluster, node, job, instance, namespace, pod and container
// as labels. So a new Prometheus, or one whose volume was lost, comes back
// with the history the raw tier still holds, which with the compactor's
// RAW_DELETE=after-final is the last one to two hours. Producers whose
// name starts with '_' are cluster-level producers that ship no metrics
// (K8SAGENT.md) and are not read.
//
// With SELECTORS_FILE, the JSON manifest of tools/rules-manifest, only the
// samples that some selector of the rules matches are pushed.
//
// A key counts as done once Prometheus answered 2xx for it, or refused it
// with a 4xx (then its series are sent one by one, so only the refused
// ones are lost), or it vanished between LIST and GET. A failed LIST, GET
// or a remote write that failed otherwise ends the pass, and the next
// pass tries the same keys again, so an outage of the bucket or of
// Prometheus shorter than LOOKBACK loses nothing.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	cluster     string
	remoteWrite string
	lookback    time.Duration
	poll        time.Duration
	timeout     time.Duration
	metricsAddr string
}

func envDefault(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envDuration(k, def string) time.Duration {
	d, err := time.ParseDuration(envDefault(k, def))
	if err != nil || d <= 0 {
		log.Fatalf("%s %q: want a positive duration", k, os.Getenv(k))
	}
	return d
}

func main() {
	log.SetFlags(0)
	var keep *selectorSet
	if f := envDefault("SELECTORS_FILE", ""); f != "" {
		var err error
		keep, err = loadSelectors(f)
		if err != nil {
			log.Fatalf("SELECTORS_FILE: %v", err)
		}
		log.Printf("filtering to %d selectors from %s", keep.len(), f)
	}
	cfg := config{
		cluster:     envDefault("CLUSTER_NAME", ""),
		remoteWrite: envDefault("REMOTE_WRITE_URL", "http://127.0.0.1:9090/api/v1/write"),
		lookback:    envDuration("LOOKBACK", "3h"),
		poll:        envDuration("POLL", "15s"),
		timeout:     envDuration("REMOTE_WRITE_TIMEOUT", "30s"),
		metricsAddr: envDefault("METRICS_ADDR", ":9091"),
	}
	endpoint := envDefault("S3_ENDPOINT", "")
	bucket := envDefault("S3_BUCKET", "")
	if endpoint == "" || bucket == "" {
		log.Fatal("S3_ENDPOINT and S3_BUCKET are required")
	}
	if cfg.cluster != "" && !clusterRE.MatchString(cfg.cluster) {
		log.Fatalf("CLUSTER_NAME %q: want a lowercase name of letters, digits, '-', '_' and '.'", cfg.cluster)
	}
	endpoint = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"), "/")
	st := &s3Store{bucket: bucket, endpoint: endpoint, tls: envDefault("S3_TLS", "off") == "on",
		region: envDefault("S3_REGION", "us-east-1"), keyID: os.Getenv("AWS_ACCESS_KEY_ID"),
		secret: os.Getenv("AWS_SECRET_ACCESS_KEY"), timeout: cfg.timeout, http: &http.Client{}}
	t := newTailer(cfg, st, &remoteWriter{url: cfg.remoteWrite, client: &http.Client{Timeout: cfg.timeout}}, keep)
	if cfg.metricsAddr != "off" {
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("/metrics", t.serveMetrics)
			log.Fatal(http.ListenAndServe(cfg.metricsAddr, mux))
		}()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	what := "raw1/" + cfg.cluster + "/"
	if cfg.cluster == "" {
		what = "every cluster under raw1/"
	}
	log.Printf("tailing %s from %s back, every %s", what, cfg.lookback, cfg.poll)
	t.run(ctx)
}

// store is the bucket as the tailer uses it.
type store interface {
	// list returns every key under prefix
	list(ctx context.Context, prefix string) ([]string, error)
	// dirs returns the prefixes one level under prefix, each ending in /
	dirs(ctx context.Context, prefix string) ([]string, error)
	// get returns an object's body; errNotFound when it is gone
	get(ctx context.Context, key string) ([]byte, error)
}

var errNotFound = errors.New("not found")

type tailer struct {
	cfg  config
	st   store
	rw   *remoteWriter
	keep *selectorSet
	// keys done, by their hour prefix, so that hours past LOOKBACK go at once
	done map[string]map[string]bool

	mu sync.Mutex
	m  tailerMetrics
}

type tailerMetrics struct {
	objects               map[string]int64 // by result
	samples, dropped      int64
	skippedLines          int64
	lastPass, lastFailure float64 // unix seconds
	newestSample          float64
	keysPending           int
}

func newTailer(cfg config, st store, rw *remoteWriter, keep *selectorSet) *tailer {
	return &tailer{cfg: cfg, st: st, rw: rw, keep: keep, done: map[string]map[string]bool{},
		m: tailerMetrics{objects: map[string]int64{}}}
}

func (t *tailer) run(ctx context.Context) {
	for {
		if err := t.pass(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("pass: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.cfg.poll):
		}
	}
}

// clusterRE is the compactor's: a cluster name is one plain key segment.
var clusterRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)

// clusters is CLUSTER_NAME, or every cluster with a prefix under raw1/.
func (t *tailer) clusters(ctx context.Context) ([]string, error) {
	if t.cfg.cluster != "" {
		return []string{t.cfg.cluster}, nil
	}
	dirs, err := t.st.dirs(ctx, "raw1/")
	if err != nil {
		return nil, fmt.Errorf("list raw1/: %w", err)
	}
	var out []string
	for _, d := range dirs {
		if c := strings.TrimSuffix(strings.TrimPrefix(d, "raw1/"), "/"); clusterRE.MatchString(c) {
			out = append(out, c)
		}
	}
	return out, nil
}

// hourPrefixes are the clusters' raw1 hour prefixes from now-LOOKBACK to
// now, oldest first per cluster.
func (t *tailer) hourPrefixes(clusters []string, now time.Time) []string {
	var out []string
	for _, c := range clusters {
		for h := now.Add(-t.cfg.lookback).UTC().Truncate(time.Hour); !h.After(now); h = h.Add(time.Hour) {
			out = append(out, fmt.Sprintf("raw1/%s/%s/", c, h.Format("2006-01-02/15")))
		}
	}
	return out
}

// pass pushes every new key of the hours in LOOKBACK. It stops at the
// first failure that a later pass may not have, so that per producer the
// keys go in order.
func (t *tailer) pass(ctx context.Context, now time.Time) error {
	clusters, err := t.clusters(ctx)
	if err != nil {
		t.mu.Lock()
		t.m.lastFailure = float64(now.Unix())
		t.mu.Unlock()
		return err
	}
	hours := t.hourPrefixes(clusters, now)
	keepHours := map[string]bool{}
	for _, h := range hours {
		keepHours[h] = true
	}
	for h := range t.done {
		if !keepHours[h] {
			delete(t.done, h)
		}
	}
	err = t.passHours(ctx, hours)
	t.mu.Lock()
	if err != nil {
		t.m.lastFailure = float64(now.Unix())
	} else {
		t.m.lastPass = float64(now.Unix())
	}
	t.mu.Unlock()
	return err
}

func (t *tailer) passHours(ctx context.Context, hours []string) error {
	var pending []string
	for _, h := range hours {
		keys, err := t.st.list(ctx, h)
		if err != nil {
			return fmt.Errorf("list %s: %w", h, err)
		}
		if t.done[h] == nil {
			t.done[h] = map[string]bool{}
		}
		for _, k := range keys {
			if t.done[h][k] {
				continue
			}
			if !strings.HasSuffix(k, ".jsonl.zst") || strings.HasPrefix(producer(h, k), "_") {
				t.done[h][k] = true
				continue
			}
			pending = append(pending, k)
		}
	}
	// key order is cluster, hour, producer, MMSS: time order per producer
	sort.Strings(pending)
	t.mu.Lock()
	t.m.keysPending = len(pending)
	t.mu.Unlock()
	for i, k := range pending {
		result, err := t.pushKey(ctx, k)
		t.mu.Lock()
		t.m.objects[result]++
		if result != "retry" {
			t.m.keysPending = len(pending) - i - 1
		}
		t.mu.Unlock()
		if result == "retry" {
			return fmt.Errorf("%s: %w", k, err)
		}
		if err != nil {
			log.Printf("%s: %v", k, err)
		}
		t.done[hourOf(k)][k] = true
	}
	return nil
}

// pushKey reads and pushes one object. The result is pushed, empty (no
// sample kept), gone (deleted before the GET), undecodable, refused
// (some series refused by the receiver) or retry (try the key again in
// the next pass).
func (t *tailer) pushKey(ctx context.Context, key string) (string, error) {
	body, err := t.st.get(ctx, key)
	if errors.Is(err, errNotFound) {
		return "gone", nil
	}
	if err != nil {
		return "retry", err
	}
	b, err := decodeObject(body, t.keep)
	t.mu.Lock()
	t.m.skippedLines += int64(b.skipped)
	t.mu.Unlock()
	if err != nil {
		return "undecodable", err
	}
	if b.samples == 0 {
		return "empty", nil
	}
	dropped, err := t.rw.push(ctx, b)
	t.mu.Lock()
	t.m.dropped += int64(dropped)
	if err == nil || dropped > 0 {
		t.m.samples += int64(b.samples)
		if s := float64(b.newest) / 1000; s > t.m.newestSample {
			t.m.newestSample = s
		}
	}
	t.mu.Unlock()
	switch {
	case err == nil:
		return "pushed", nil
	case errors.Is(err, errRejected) || dropped > 0:
		return "refused", err
	default:
		return "retry", err
	}
}

// hourOf is the hour prefix raw1/{cluster}/{date}/{HH}/ of a key.
func hourOf(key string) string {
	parts := strings.SplitN(key, "/", 5)
	if len(parts) < 5 {
		return key
	}
	return strings.Join(parts[:4], "/") + "/"
}

// producer is the key's segment after the hour: a node name, or a
// cluster-level producer such as _k8sagent.
func producer(hour, key string) string {
	rest := strings.TrimPrefix(key, hour)
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return ""
}

func (t *tailer) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	t.mu.Lock()
	m := t.m
	objects := map[string]int64{}
	for k, v := range t.m.objects {
		objects[k] = v
	}
	t.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP blobs_to_prometheus_objects_total Raw objects handled, by result.")
	fmt.Fprintln(w, "# TYPE blobs_to_prometheus_objects_total counter")
	results := []string{"pushed", "empty", "gone", "undecodable", "refused", "retry"}
	for _, r := range results {
		fmt.Fprintf(w, "blobs_to_prometheus_objects_total{result=%q} %d\n", r, objects[r])
	}
	gauges := []struct {
		name, help, typ string
		v               float64
	}{
		{"blobs_to_prometheus_samples_total", "Samples sent to the receiver.", "counter", float64(m.samples)},
		{"blobs_to_prometheus_dropped_series_total", "Series the receiver refused.", "counter", float64(m.dropped)},
		{"blobs_to_prometheus_skipped_lines_total", "Metrics lines or samples that could not be decoded.", "counter", float64(m.skippedLines)},
		{"blobs_to_prometheus_last_pass_success_timestamp_seconds", "End of the last pass that completed.", "gauge", m.lastPass},
		{"blobs_to_prometheus_last_pass_failure_timestamp_seconds", "End of the last pass that failed.", "gauge", m.lastFailure},
		{"blobs_to_prometheus_newest_sample_timestamp_seconds", "Time of the newest sample sent.", "gauge", m.newestSample},
		{"blobs_to_prometheus_keys_pending", "Keys listed and not yet done in the current pass.", "gauge", float64(m.keysPending)},
	}
	for _, g := range gauges {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %g\n", g.name, g.help, g.name, g.typ, g.name, g.v)
	}
}
