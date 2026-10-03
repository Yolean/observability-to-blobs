package main

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metrics is the /metrics endpoint, in the Prometheus text exposition
// format written with the standard library. The node agent scrapes it into
// the blobs, where alerting reads it; the rule it exists for is
// time() - blobs_compactor_last_success_timestamp_seconds{job="rolling"} > 600.
type metrics struct {
	mu           sync.Mutex
	now          func() time.Time
	lastSuccess  map[string]float64    // job
	lastDuration map[string]float64    // job
	runs         map[[2]string]float64 // job, result
	rows         map[[2]string]float64 // tier, table
	keysRead     float64
	rawDeleted   float64
	rawObjects   float64
	rawOldest    time.Time // upload time of the oldest raw object in the last full listing
}

func newMetrics() *metrics {
	return &metrics{now: time.Now, lastSuccess: map[string]float64{}, lastDuration: map[string]float64{},
		runs: map[[2]string]float64{}, rows: map[[2]string]float64{}}
}

// job records one run of a job: rolling, past or rollup.
func (m *metrics) job(job string, start time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	end := m.now()
	m.lastDuration[job] = end.Sub(start).Seconds()
	result := "success"
	if err != nil {
		result = "failure"
	} else {
		m.lastSuccess[job] = float64(end.UnixMilli()) / 1000
	}
	m.runs[[2]string{job, result}]++
}

func (m *metrics) fold(r foldResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keysRead += float64(r.keys)
	m.rawDeleted += float64(len(r.deleted))
	for t, n := range r.rows {
		m.rows[[2]string{"hourly", t}] = float64(n)
	}
}

func (m *metrics) rollup(r rollupResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for t, n := range r.rows {
		m.rows[[2]string{"daily", t}] = float64(n)
	}
}

func (m *metrics) listing(objects int, oldest time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rawObjects = float64(objects)
	m.rawOldest = oldest
}

func (m *metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.write(w)
}

func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	family := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	byJob := func(name string, v map[string]float64) {
		for _, k := range sortedKeys(v) {
			fmt.Fprintf(&b, "%s{job=%q} %s\n", name, k, num(v[k]))
		}
	}
	pairs := func(name, l1, l2 string, v map[[2]string]float64) {
		keys := make([][2]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i][0]+"\x00"+keys[i][1] < keys[j][0]+"\x00"+keys[j][1] })
		for _, k := range keys {
			fmt.Fprintf(&b, "%s{%s=%q,%s=%q} %s\n", name, l1, k[0], l2, k[1], num(v[k]))
		}
	}
	family("blobs_compactor_last_success_timestamp_seconds", "gauge", "When a job last completed without error (jobs: rolling, past, rollup).")
	byJob("blobs_compactor_last_success_timestamp_seconds", m.lastSuccess)
	family("blobs_compactor_last_duration_seconds", "gauge", "How long the job's last run took, successful or not.")
	byJob("blobs_compactor_last_duration_seconds", m.lastDuration)
	family("blobs_compactor_runs_total", "counter", "Runs of each job by result.")
	pairs("blobs_compactor_runs_total", "job", "result", m.runs)
	family("blobs_compactor_rows", "gauge", "Rows in the last object written, per tier and table (daily: the day's rows of the namespaces rewritten).")
	pairs("blobs_compactor_rows", "tier", "table", m.rows)
	family("blobs_compactor_raw_keys_read_total", "counter", "Raw objects read by folds; a full read counts every key of its hour.")
	fmt.Fprintf(&b, "blobs_compactor_raw_keys_read_total %s\n", num(m.keysRead))
	family("blobs_compactor_raw_deleted_total", "counter", "Raw objects deleted after their hour's final fold (RAW_DELETE=after-final).")
	fmt.Fprintf(&b, "blobs_compactor_raw_deleted_total %s\n", num(m.rawDeleted))
	family("blobs_compactor_raw_objects", "gauge", "Raw objects in the last listing of the cluster's raw prefix, less those deleted since.")
	fmt.Fprintf(&b, "blobs_compactor_raw_objects %s\n", num(m.rawObjects))
	family("blobs_compactor_raw_oldest_age_seconds", "gauge", "Age now of the oldest raw object (by upload time) in the last listing, 0 when it had none.")
	age := 0.0
	if !m.rawOldest.IsZero() {
		age = m.now().Sub(m.rawOldest).Seconds()
	}
	fmt.Fprintf(&b, "blobs_compactor_raw_oldest_age_seconds %s\n", num(age))
	io.WriteString(w, b.String())
}

// num renders a sample value without an exponent, so timestamps keep
// their precision.
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func sortedKeys(v map[string]float64) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
