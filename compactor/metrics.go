package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
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
	// cluster is the label on every series
	cluster      string
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
	// rows that refused folds and rollups could not account for (guard.go)
	unaccounted float64
	// spool mode: the put endpoint's requests by result, the bytes it
	// stored, and the spool's files now, a walk of it that a scrape makes
	// without holding mu, so that a put's count never waits for a walk.
	// spool is set before the metrics are served.
	puts     map[string]float64
	putBytes float64
	spool    func() (spoolStats, error)
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

func newMetrics(cluster string) *metrics {
	m := &metrics{cluster: cluster, now: time.Now, lastSuccess: map[string]float64{}, lastDuration: map[string]float64{},
		runs: map[[2]string]float64{}, rows: map[[2]string]float64{}, rawHours: map[time.Time]rawSummary{},
		puts: map[string]float64{}}
	// every job's counters from the start, so that the first failure after
	// a start is an increase from 0, not a series that appears
	for _, job := range []string{"rolling", "past", "rollup"} {
		for _, result := range []string{"success", "failure"} {
			m.runs[[2]string{job, result}] = 0
		}
	}
	return m
}

// processStart is when this process started, near enough: a restart, an
// OOM kill's included, shows as a new value (process_start_time_seconds).
var processStart = time.Now()

// residentBytes is the process's resident memory, DuckDB's included, from
// /proc/self/statm; false where there is none.
func residentBytes() (float64, bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseFloat(f[1], 64)
	if err != nil {
		return 0, false
	}
	return pages * float64(os.Getpagesize()), true
}

// put records one request to the put endpoint: stored, duplicate,
// conflict or already_folded with the bytes of a stored body, or the S3
// error code it got.
func (m *metrics) put(result string, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts[result]++
	if result == "stored" {
		m.putBytes += float64(bytes)
	}
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

// refused counts a fold or a rollup that the guard refused.
func (m *metrics) refused(e *unaccountedError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unaccounted += float64(e.n)
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

// write writes m's families, as a metricsSet of m alone does.
func (m *metrics) write(w io.Writer) { writeMetrics(w, []*metrics{m}) }

// metricsSet is the /metrics endpoint of a process that compacts several
// clusters: each family once, with a series per cluster.
type metricsSet struct {
	mu sync.Mutex
	ms []*metrics // by cluster
}

func (s *metricsSet) add(m *metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ms = append(s.ms, m)
	sort.Slice(s.ms, func(i, j int) bool { return s.ms[i].cluster < s.ms[j].cluster })
}

func (s *metricsSet) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.mu.Lock()
	ms := append([]*metrics(nil), s.ms...)
	s.mu.Unlock()
	writeMetrics(w, ms)
}

// writeMetrics writes each family once, and in it each cluster's series,
// labelled cluster. A spool's gauges are a walk of it now, made before
// the cluster's mu is held; a failed walk leaves them out.
func writeMetrics(w io.Writer, ms []*metrics) {
	sts := make([]spoolStats, len(ms))
	stErrs := make([]error, len(ms))
	spooled := false
	for i, m := range ms {
		if m.spool != nil {
			spooled = true
			sts[i], stErrs[i] = m.spool()
		}
	}
	var b strings.Builder
	family := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	// each runs f for every cluster with its mu held, l its label pair
	each := func(f func(i int, m *metrics, l string)) {
		for i, m := range ms {
			m.mu.Lock()
			f(i, m, fmt.Sprintf("cluster=%q", m.cluster))
			m.mu.Unlock()
		}
	}
	byJob := func(name string, v func(m *metrics) map[string]float64) {
		each(func(_ int, m *metrics, l string) {
			for _, k := range sortedKeys(v(m)) {
				fmt.Fprintf(&b, "%s{%s,job=%q} %s\n", name, l, k, num(v(m)[k]))
			}
		})
	}
	pairs := func(name, l1, l2 string, v func(m *metrics) map[[2]string]float64) {
		each(func(_ int, m *metrics, l string) {
			keys := make([][2]string, 0, len(v(m)))
			for k := range v(m) {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i][0]+"\x00"+keys[i][1] < keys[j][0]+"\x00"+keys[j][1] })
			for _, k := range keys {
				fmt.Fprintf(&b, "%s{%s,%s=%q,%s=%q} %s\n", name, l, l1, k[0], l2, k[1], num(v(m)[k]))
			}
		})
	}
	single := func(name string, v func(m *metrics) float64) {
		each(func(_ int, m *metrics, l string) { fmt.Fprintf(&b, "%s{%s} %s\n", name, l, num(v(m))) })
	}
	family("blobs_compactor_last_success_timestamp_seconds", "gauge", "When a job last completed without error (jobs: rolling, past, rollup).")
	byJob("blobs_compactor_last_success_timestamp_seconds", func(m *metrics) map[string]float64 { return m.lastSuccess })
	family("blobs_compactor_last_duration_seconds", "gauge", "How long the job's last run took, successful or not.")
	byJob("blobs_compactor_last_duration_seconds", func(m *metrics) map[string]float64 { return m.lastDuration })
	family("blobs_compactor_runs_total", "counter", "Runs of each job by result.")
	pairs("blobs_compactor_runs_total", "job", "result", func(m *metrics) map[[2]string]float64 { return m.runs })
	family("blobs_compactor_rows", "gauge", "Rows in the last object written, per tier and table (daily: the day's rows of the namespaces rewritten).")
	pairs("blobs_compactor_rows", "tier", "table", func(m *metrics) map[[2]string]float64 { return m.rows })
	family("blobs_compactor_raw_keys_read_total", "counter", "Raw objects read by folds; a full read counts every key of its hour.")
	single("blobs_compactor_raw_keys_read_total", func(m *metrics) float64 { return m.keysRead })
	family("blobs_compactor_raw_deleted_total", "counter", "Raw objects deleted after their hour's final fold (RAW_DELETE=after-final).")
	single("blobs_compactor_raw_deleted_total", func(m *metrics) float64 { return m.rawDeleted })
	family("blobs_compactor_raw_objects", "gauge", "Raw objects as the latest listing of each hour saw them (a fold's of its hour, the past pass's of every hour), less those deleted since.")
	single("blobs_compactor_raw_objects", func(m *metrics) float64 {
		n := 0
		for _, s := range m.rawHours {
			n += s.objects
		}
		return float64(n)
	})
	family("blobs_compactor_raw_oldest_age_seconds", "gauge", "Age now of the oldest of those raw objects, by upload time; 0 when there are none.")
	single("blobs_compactor_raw_oldest_age_seconds", func(m *metrics) float64 {
		var oldest time.Time
		for _, s := range m.rawHours {
			if oldest.IsZero() || s.oldest.Before(oldest) {
				oldest = s.oldest
			}
		}
		if oldest.IsZero() {
			return 0
		}
		return m.now().Sub(oldest).Seconds()
	})
	family("blobs_compactor_unaccounted_total", "counter", "Raw lines without a table the compactor folds, and hourly logs or metrics rows without a namespace, counted at every fold or rollup that refused for them: it wrote nothing and deleted no raw. Alert on any increase.")
	single("blobs_compactor_unaccounted_total", func(m *metrics) float64 { return m.unaccounted })
	family("blobs_compactor_parquet_keys_missing", "gauge", "Parquet key ids that tier object names (or PARQUET_KEY_ID) carry and PARQUET_KEYS_DIR lacks; folds and rollups refuse while it is above 0.")
	single("blobs_compactor_parquet_keys_missing", func(m *metrics) float64 { return m.keysMissed })
	// the process's own, once, as client_golang's process collector names
	// them
	family("process_start_time_seconds", "gauge", "Start time of the process since unix epoch in seconds.")
	fmt.Fprintf(&b, "process_start_time_seconds %s\n", num(float64(processStart.UnixMilli())/1000))
	if rss, ok := residentBytes(); ok {
		family("process_resident_memory_bytes", "gauge", "Resident memory size in bytes.")
		fmt.Fprintf(&b, "process_resident_memory_bytes %s\n", num(rss))
	}
	if !spooled {
		io.WriteString(w, b.String())
		return
	}
	family("blobs_compactor_put_requests_total", "counter", "Requests to the put endpoint by result: stored, duplicate (the key held the same content), conflict (the key held other content, which it keeps), already_folded (the key's hour was folded with it and its file deleted: nothing stored), or the S3 error code answered.")
	each(func(_ int, m *metrics, l string) {
		if m.spool == nil {
			return
		}
		for _, k := range sortedKeys(m.puts) {
			fmt.Fprintf(&b, "blobs_compactor_put_requests_total{%s,result=%q} %s\n", l, k, num(m.puts[k]))
		}
	})
	family("blobs_compactor_put_bytes_total", "counter", "Bytes of the objects the put endpoint stored.")
	each(func(_ int, m *metrics, l string) {
		if m.spool != nil {
			fmt.Fprintf(&b, "blobs_compactor_put_bytes_total{%s} %s\n", l, num(m.putBytes))
		}
	})
	for _, g := range []struct {
		name, help string
		v          func(st spoolStats, m *metrics) float64
	}{
		{"blobs_compactor_spool_files", "Raw files in the spool, uploaded and not yet deleted after their hour's final fold.", func(st spoolStats, _ *metrics) float64 { return float64(st.files) }},
		{"blobs_compactor_spool_bytes", "Bytes of those files.", func(st spoolStats, _ *metrics) float64 { return float64(st.bytes) }},
		{"blobs_compactor_spool_oldest_age_seconds", "Age now of the oldest of those files, by write time; 0 when there are none.", func(st spoolStats, m *metrics) float64 {
			if st.oldest.IsZero() {
				return 0
			}
			return m.now().Sub(st.oldest).Seconds()
		}},
	} {
		family(g.name, "gauge", g.help)
		each(func(i int, m *metrics, l string) {
			if m.spool != nil && stErrs[i] == nil {
				fmt.Fprintf(&b, "%s{%s} %s\n", g.name, l, num(g.v(sts[i], m)))
			}
		})
	}
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
