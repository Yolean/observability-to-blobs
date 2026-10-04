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
	// the raw objects of each hour as its latest listing saw them, less
	// those deleted since: a fold's listing of its hour, or the past
	// pass's of every hour
	rawHours map[time.Time]rawSummary
	// parquet key ids the tiers' object names carry with no key file
	keysMissed float64
}

// rawSummary is one hour's raw objects: how many, and the oldest upload.
type rawSummary struct {
	objects int
	oldest  time.Time
}

func summarize(objs []Object, deleted []string) rawSummary {
	gone := make(map[string]bool, len(deleted))
	for _, k := range deleted {
		gone[k] = true
	}
	var s rawSummary
	for _, o := range objs {
		if gone[o.Key] {
			continue
		}
		s.objects++
		if s.oldest.IsZero() || o.Modified.Before(s.oldest) {
			s.oldest = o.Modified
		}
	}
	return s
}

func newMetrics() *metrics {
	return &metrics{now: time.Now, lastSuccess: map[string]float64{}, lastDuration: map[string]float64{},
		runs: map[[2]string]float64{}, rows: map[[2]string]float64{}, rawHours: map[time.Time]rawSummary{}}
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
	m.setRawHour(r.hour, r.raw)
}

func (m *metrics) rollup(r rollupResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for t, n := range r.rows {
		m.rows[[2]string{"daily", t}] = float64(n)
	}
}

// rawHour sets an hour's raw summary from a listing of it.
func (m *metrics) rawHour(h time.Time, s rawSummary) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setRawHour(h, s)
}

func (m *metrics) setRawHour(h time.Time, s rawSummary) {
	if s.objects == 0 {
		delete(m.rawHours, h)
		return
	}
	m.rawHours[h] = s
}

// rawRetain forgets the hours a listing of every hour did not find.
func (m *metrics) rawRetain(hours map[time.Time]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h := range m.rawHours {
		if !hours[h] {
			delete(m.rawHours, h)
		}
	}
}

func (m *metrics) keysMissing(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keysMissed = float64(n)
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
	var objects int
	var oldest time.Time
	for _, s := range m.rawHours {
		objects += s.objects
		if oldest.IsZero() || s.oldest.Before(oldest) {
			oldest = s.oldest
		}
	}
	family("blobs_compactor_raw_objects", "gauge", "Raw objects as the latest listing of each hour saw them (a fold's of its hour, the past pass's of every hour), less those deleted since.")
	fmt.Fprintf(&b, "blobs_compactor_raw_objects %d\n", objects)
	family("blobs_compactor_raw_oldest_age_seconds", "gauge", "Age now of the oldest of those raw objects, by upload time; 0 when there are none.")
	age := 0.0
	if !oldest.IsZero() {
		age = m.now().Sub(oldest).Seconds()
	}
	fmt.Fprintf(&b, "blobs_compactor_raw_oldest_age_seconds %s\n", num(age))
	family("blobs_compactor_parquet_keys_missing", "gauge", "Parquet key ids that tier object names (or PARQUET_KEY_ID) carry and PARQUET_KEYS_DIR lacks; folds and rollups refuse while it is above 0.")
	fmt.Fprintf(&b, "blobs_compactor_parquet_keys_missing %s\n", num(m.keysMissed))
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
