package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/klauspost/compress/zstd"
)

func zstdLines(t *testing.T, lines ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		fmt.Fprintln(zw, l)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// metricsLine is a metrics line as the agent writes it (cri_scrape's enqueueScrape).
func metricsLine(at time.Time, node, scrape, ns, samples string) string {
	return fmt.Sprintf(`{"table":"metrics","time":"%s","ts":"%s","cluster":"c1","node":"%s","scrape":"%s","instance":"10.0.0.1:9100","namespace":"%s","pod":"p","pod_uid":"u","container":"c","k8s_deployment_name":"web","k8s_replicaset_name":"web-5d8f7c9b4f","samples":[%s]}`,
		at.UTC().Format("2006-01-02T15:04:05.000000000Z"), at.UTC().Format("2006-01-02T15:04:05.000000Z"), node, scrape, ns, samples)
}

func TestDecodeLines(t *testing.T) {
	at := time.Date(2026, 10, 8, 14, 0, 5, 123456789, time.UTC)
	body := zstdLines(t,
		`{"table":"logs","time":"2026-10-08T14:00:01.000000000Z","message":"metrics are fine"}`,
		metricsLine(at, "n1", "pod", "ns1", `{"name":"up","type":"gauge","labels":{},"value":1.0},`+
			`{"name":"x_total","type":"counter","labels":{"job":"inner","code":"200"},"value":3},`+
			`{"name":"g","type":"gauge","labels":{"le":"+Inf"},"value":nan},`+
			`{"name":"h","type":"gauge","labels":{},"value":-inf}`),
		`not json at all, "metrics"`,
		`{"table":"podindex","kind":"pod-observed"}`,
	)
	b, err := decodeObject(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.lines != 1 || b.samples != 4 || b.skipped != 1 {
		t.Fatalf("lines %d samples %d skipped %d", b.lines, b.samples, b.skipped)
	}
	if b.newest != at.UnixMilli() {
		t.Fatalf("newest %d, want %d", b.newest, at.UnixMilli())
	}
	byName := map[string]*series{}
	for _, k := range b.order {
		byName[labelValue(b.series[k].labels, "__name__")] = b.series[k]
	}
	x := byName["x_total"]
	if x == nil {
		t.Fatalf("no x_total in %v", b.order)
	}
	// the target's own job stays job; the scrape path is scrape
	want := "__name__=x_total cluster=c1 code=200 container=c instance=10.0.0.1:9100 job=inner k8s_deployment_name=web k8s_replicaset_name=web-5d8f7c9b4f namespace=ns1 node=n1 pod=p scrape=pod"
	if got := fmtLabels(x.labels); got != want {
		t.Fatalf("labels\n got %s\nwant %s", got, want)
	}
	if v := byName["g"].samples[0].value; !math.IsNaN(v) {
		t.Fatalf("nan: %v", v)
	}
	if v := byName["h"].samples[0].value; !math.IsInf(v, -1) {
		t.Fatalf("-inf: %v", v)
	}
}

func fmtLabels(ls []label) string {
	var s []string
	for _, l := range ls {
		s = append(s, l.name+"="+l.value)
	}
	return strings.Join(s, " ")
}

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	listErr error
	gets    []string
}

func (f *fakeStore) list(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (f *fakeStore) dirs(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	seen := map[string]bool{}
	var dirs []string
	for k := range f.objects {
		rest, ok := strings.CutPrefix(k, prefix)
		if i := strings.Index(rest, "/"); ok && i >= 0 && !seen[rest[:i]] {
			seen[rest[:i]] = true
			dirs = append(dirs, prefix+rest[:i+1])
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

func (f *fakeStore) get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, key)
	b, ok := f.objects[key]
	if !ok {
		return nil, errNotFound
	}
	return b, nil
}

// receiver is a remote-write endpoint that records series names and can
// fail with a status.
type receiver struct {
	mu     sync.Mutex
	status int
	// refuse makes a request containing this series name a 400
	refuse string
	names  []string
	reqs   int
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	raw, err := snappy.Decode(nil, body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	names := decodeNames(raw)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs++
	if r.status != 0 {
		w.WriteHeader(r.status)
		return
	}
	for _, n := range names {
		if n == r.refuse {
			http.Error(w, "out of order sample", 400)
			return
		}
	}
	r.names = append(r.names, names...)
}

// decodeNames reads the __name__ of every TimeSeries of a WriteRequest.
func decodeNames(b []byte) []string {
	var names []string
	for _, ts := range fields(b, 1) {
		for _, l := range fields(ts, 1) {
			kv := fields(l, 1)
			v := fields(l, 2)
			if len(kv) == 1 && string(kv[0]) == "__name__" && len(v) == 1 {
				names = append(names, string(v[0]))
			}
		}
	}
	return names
}

// fields returns the length-delimited fields numbered n of a message.
func fields(b []byte, n int) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		key, k := binary.Uvarint(b)
		b = b[k:]
		switch key & 7 {
		case 0:
			_, k := binary.Uvarint(b)
			b = b[k:]
		case 1:
			b = b[8:]
		case 2:
			l, k := binary.Uvarint(b)
			b = b[k:]
			if int(key>>3) == n {
				out = append(out, b[:l])
			}
			b = b[l:]
		default:
			return out
		}
	}
	return out
}

func testTailer(t *testing.T, st store, rcv *receiver, keep *selectorSet) *tailer {
	srv := httptest.NewServer(rcv)
	t.Cleanup(srv.Close)
	return newTailer(config{cluster: "c1", lookback: 3 * time.Hour},
		st, &remoteWriter{url: srv.URL, client: &http.Client{Timeout: 5 * time.Second}}, keep)
}

func TestTailerPasses(t *testing.T) {
	now := time.Date(2026, 10, 8, 14, 20, 0, 0, time.UTC)
	key := func(at time.Time, producer string, n int) string {
		return fmt.Sprintf("raw1/c1/%s/%s/%s-%04d.jsonl.zst", at.Format("2006-01-02/15"), producer, at.Format("0405"), n)
	}
	old := now.Add(-4 * time.Hour)
	inHour := now.Add(-90 * time.Minute)
	st := &fakeStore{objects: map[string][]byte{
		key(old, "n1", 1):            zstdLines(t, metricsLine(old, "n1", "node", "", `{"name":"too_old","labels":{},"value":1}`)),
		key(inHour, "n1", 2):         zstdLines(t, metricsLine(inHour, "n1", "node", "", `{"name":"a","labels":{},"value":1}`)),
		key(now, "n1", 3):            zstdLines(t, metricsLine(now, "n1", "node", "", `{"name":"b","labels":{},"value":1}`)),
		key(now, "_k8sagent", 4):     []byte("never read"),
		key(now, "n2", 5):            zstdLines(t, `{"table":"logs","message":"x"}`),
		"raw1/c1/2026-10-08/14/n1/x": []byte("not a raw object"),
	}}
	rcv := &receiver{}
	tl := testTailer(t, st, rcv, nil)
	ctx := context.Background()

	if err := tl.pass(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rcv.names, ","); got != "a,b" {
		t.Fatalf("pushed %q, want a,b (oldest first, nothing past LOOKBACK)", got)
	}
	for _, g := range st.gets {
		if strings.Contains(g, "_k8sagent") || strings.HasSuffix(g, "/x") {
			t.Fatalf("read %s", g)
		}
	}
	// a second pass pushes nothing again
	if err := tl.pass(ctx, now); err != nil || len(rcv.names) != 2 {
		t.Fatalf("second pass: %v, %v", err, rcv.names)
	}

	// Prometheus down: the pass fails and the key is tried again later
	st.objects[key(now.Add(time.Minute), "n1", 6)] = zstdLines(t, metricsLine(now, "n1", "node", "", `{"name":"c","labels":{},"value":1}`))
	rcv.status = 503
	if err := tl.pass(ctx, now.Add(time.Minute)); err == nil {
		t.Fatal("pass with the receiver down: no error")
	}
	rcv.status = 0
	if err := tl.pass(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rcv.names, ","); got != "a,b,c" {
		t.Fatalf("after the outage %q, want a,b,c", got)
	}

	// a LIST failure fails the pass and loses nothing
	st.listErr = errors.New("dns")
	st.objects[key(now.Add(3*time.Minute), "n1", 7)] = zstdLines(t, metricsLine(now, "n1", "node", "", `{"name":"d","labels":{},"value":1}`))
	if err := tl.pass(ctx, now.Add(3*time.Minute)); err == nil {
		t.Fatal("pass with LIST failing: no error")
	}
	st.listErr = nil
	if err := tl.pass(ctx, now.Add(4*time.Minute)); err != nil || strings.Join(rcv.names, ",") != "a,b,c,d" {
		t.Fatalf("after the LIST failure: %v %v", err, rcv.names)
	}
}

// One refused series loses only itself: the object goes again series by series.
func TestTailerRefusedSeries(t *testing.T) {
	now := time.Date(2026, 10, 8, 14, 20, 0, 0, time.UTC)
	st := &fakeStore{objects: map[string][]byte{
		"raw1/c1/2026-10-08/14/n1/2000-1.jsonl.zst": zstdLines(t, metricsLine(now, "n1", "node", "",
			`{"name":"good1","labels":{},"value":1},{"name":"bad","labels":{},"value":1},{"name":"good2","labels":{},"value":1}`)),
	}}
	rcv := &receiver{refuse: "bad"}
	tl := testTailer(t, st, rcv, nil)
	if err := tl.pass(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rcv.names, ","); got != "good1,good2" {
		t.Fatalf("pushed %q", got)
	}
	if tl.m.dropped != 1 || tl.m.objects["refused"] != 1 {
		t.Fatalf("dropped %d refused %d", tl.m.dropped, tl.m.objects["refused"])
	}
	// done: not sent again
	reqs := rcv.reqs
	if err := tl.pass(context.Background(), now); err != nil || rcv.reqs != reqs {
		t.Fatalf("second pass: %v, %d requests after %d", err, rcv.reqs, reqs)
	}
}

func TestTailerSelectors(t *testing.T) {
	now := time.Date(2026, 10, 8, 14, 20, 0, 0, time.UTC)
	st := &fakeStore{objects: map[string][]byte{
		"raw1/c1/2026-10-08/14/n1/2000-1.jsonl.zst": zstdLines(t, metricsLine(now, "n1", "host", "",
			`{"name":"yolean_hostfw_loaded","labels":{},"value":1},{"name":"node_load1","labels":{},"value":1}`)),
	}}
	rcv := &receiver{}
	tl := testTailer(t, st, rcv, loadFrom(t, `{"selectors":[{"name":"yolean_hostfw_loaded"}]}`))
	if err := tl.pass(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rcv.names, ","); got != "yolean_hostfw_loaded" {
		t.Fatalf("pushed %q", got)
	}
}

func TestHourOf(t *testing.T) {
	if got := hourOf("raw1/c1/2026-10-08/14/n1/2000-1.jsonl.zst"); got != "raw1/c1/2026-10-08/14/" {
		t.Fatal(got)
	}
	if got := producer("raw1/c1/2026-10-08/14/", "raw1/c1/2026-10-08/14/_k8sagent/0000-x.jsonl.zst"); got != "_k8sagent" {
		t.Fatal(got)
	}
}

// With no CLUSTER_NAME every cluster under raw1/ is tailed, each line's
// series with its own cluster label; one is found when it appears.
func TestTailerEveryCluster(t *testing.T) {
	now := time.Date(2026, 10, 8, 14, 20, 0, 0, time.UTC)
	line := func(cluster, name string) []byte {
		return zstdLines(t, strings.Replace(metricsLine(now, "n1", "node", "", `{"name":"`+name+`","labels":{},"value":1}`),
			`"cluster":"c1"`, `"cluster":"`+cluster+`"`, 1))
	}
	st := &fakeStore{objects: map[string][]byte{
		"raw1/c1/2026-10-08/14/n1/2000-1.jsonl.zst":            line("c1", "one"),
		"raw1/c2/2026-10-08/14/n1/2000-1.jsonl.zst":            line("c2", "two"),
		"raw1/Not_A_Cluster/2026-10-08/14/n1/2000-1.jsonl.zst": line("x", "never"),
	}}
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	t.Cleanup(srv.Close)
	tl := newTailer(config{lookback: 3 * time.Hour}, st, &remoteWriter{url: srv.URL, client: &http.Client{Timeout: 5 * time.Second}}, nil)
	if err := tl.pass(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rcv.names, ","); got != "one,two" {
		t.Fatalf("pushed %q, want one,two", got)
	}
	st.mu.Lock()
	st.objects["raw1/c3/2026-10-08/14/n1/2010-1.jsonl.zst"] = line("c3", "three")
	st.mu.Unlock()
	if err := tl.pass(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rcv.names, ","); got != "one,two,three" {
		t.Fatalf("pushed %q after c3 appeared", got)
	}
}
