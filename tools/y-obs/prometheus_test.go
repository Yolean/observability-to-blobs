package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/golang/snappy"
)

// readRequest builds a prompb.ReadRequest as Prometheus sends it: one
// query with hints, and the accepted response types packed.
func readRequest(start, end int64, ms ...matcher) []byte {
	var q protoBuf
	q.tagVarint(1, uint64(start))
	q.tagVarint(2, uint64(end))
	for _, m := range ms {
		var mb protoBuf
		mb.tagVarint(1, uint64(m.typ))
		mb.tagBytes(2, []byte(m.name))
		mb.tagBytes(3, []byte(m.value))
		q.tagBytes(3, mb.Bytes())
	}
	var hints protoBuf
	hints.tagVarint(1, 15000)
	hints.tagBytes(2, []byte("rate"))
	q.tagBytes(4, hints.Bytes())
	var rr protoBuf
	rr.tagBytes(1, q.Bytes())
	rr.tagBytes(2, []byte{0, 1})
	return rr.Bytes()
}

// decodeReadResponse reads a ReadResponse back into label sets and samples.
func decodeReadResponse(t *testing.T, b []byte) [][]series {
	t.Helper()
	var out [][]series
	r := &protoReader{b: b}
	for !r.done() {
		_, _, _, qb, err := r.next()
		if err != nil {
			t.Fatal(err)
		}
		var ss []series
		qr := &protoReader{b: qb}
		for !qr.done() {
			_, _, _, tb, err := qr.next()
			if err != nil {
				t.Fatal(err)
			}
			var sr series
			tr := &protoReader{b: tb}
			for !tr.done() {
				f, _, _, fb, err := tr.next()
				if err != nil {
					t.Fatal(err)
				}
				fr := &protoReader{b: fb}
				var l label
				var s sample
				for !fr.done() {
					g, w, v, gb, err := fr.next()
					if err != nil {
						t.Fatal(err)
					}
					switch {
					case f == 1 && g == 1:
						l.name = string(gb)
					case f == 1 && g == 2:
						l.value = string(gb)
					case f == 2 && g == 2 && w == 0:
						s.ts = int64(v)
					}
				}
				if f == 1 {
					sr.labels = append(sr.labels, l)
				} else {
					sr.samples = append(sr.samples, s)
				}
			}
			ss = append(ss, sr)
		}
		out = append(out, ss)
	}
	return out
}

func TestReadRequestDecode(t *testing.T) {
	qs, err := decodeReadRequest(readRequest(1000, 2000, matcher{0, "__name__", "up"}, matcher{2, "pod", "web-.*"}))
	if err != nil || len(qs) != 1 {
		t.Fatalf("%v %v", qs, err)
	}
	q := qs[0]
	if q.startMs != 1000 || q.endMs != 2000 || len(q.matchers) != 2 || q.matchers[1] != (matcher{2, "pod", "web-.*"}) {
		t.Errorf("%+v", q)
	}
	if _, err := decodeReadRequest([]byte{0x0a, 0x05, 0x08}); err == nil {
		t.Error("a truncated request decoded")
	}
}

func TestSeriesLabels(t *testing.T) {
	ls := seriesLabels("http_requests_total", map[string]string{"scrape": "pod", "pod": "web-1", "namespace": "", "cluster": "c"},
		map[string]string{"code": "200", "pod": "inner", "empty": ""})
	want := []label{{"__name__", "http_requests_total"}, {"cluster", "c"}, {"code", "200"}, {"exported_pod", "inner"}, {"pod", "web-1"}, {"scrape", "pod"}}
	if len(ls) != len(want) {
		t.Fatalf("%v", ls)
	}
	for i := range want {
		if ls[i] != want[i] {
			t.Errorf("%d: %v, want %v", i, ls[i], want[i])
		}
	}
}

func TestMatcherSQL(t *testing.T) {
	for _, c := range []struct {
		m    matcher
		want string
	}{
		{matcher{0, "__name__", "up"}, "coalesce(name, '') = 'up'"},
		{matcher{0, "namespace", "qa"}, "coalesce(nullif(namespace, '_node'), '') = 'qa'"},
		{matcher{1, "pod", ""}, "coalesce(pod, '') <> ''"},
		{matcher{2, "code", "5.."}, "regexp_full_match(coalesce(labels['code'], ''), '5..')"},
		{matcher{3, "exported_pod", "x|y"}, "NOT regexp_full_match(coalesce(labels['pod'], ''), 'x|y')"},
	} {
		got, err := matcherSQL(c.m)
		if err != nil || got != c.want {
			t.Errorf("%s: %s %v, want %s", c.m, got, err, c.want)
		}
	}
	if _, err := matcherSQL(matcher{2, "pod", "("}); err == nil {
		t.Error("a bad regex passed")
	}
}

// A remote read through the handler: the matchers select over the
// window's tiers, the labels are blobs-to-prometheus's, and a range that
// reaches into the pushed window is cut before it.
func TestRemoteRead(t *testing.T) {
	dir := t.TempDir()
	var lines []map[string]interface{}
	for i := 0; i < 6; i++ {
		at := time.Date(2026, 10, 11, 9, i*10, 0, 0, time.UTC).Format(time.RFC3339)
		lines = append(lines, scrape(at, "web-1", metricSample("http_requests_total", "counter", float64(i), map[string]string{"code": "200"}),
			metricSample("up", "gauge", 1, map[string]string{})))
		lines = append(lines, scrape(at, "web-2", metricSample("http_requests_total", "counter", float64(10*i), map[string]string{"code": "500"})))
	}
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/09/n1/a.jsonl.zst"), lines)
	p := &promServer{sf: sessionFlags{cluster: "c", blobsDir: dir, maxObjects: 10}}
	srv := httptest.NewServer(http.HandlerFunc(p.read))
	defer srv.Close()
	call := func(start, end time.Time, ms ...matcher) [][]series {
		res, err := http.Post(srv.URL, "application/x-protobuf", bytes.NewReader(snappy.Encode(nil, readRequest(start.UnixMilli(), end.UnixMilli(), ms...))))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		buf.ReadFrom(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("HTTP %d %s", res.StatusCode, buf.String())
		}
		raw, err := snappy.Decode(nil, buf.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return decodeReadResponse(t, raw)
	}
	from, to := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	got := call(from, to, matcher{0, "__name__", "http_requests_total"}, matcher{2, "code", "2.."})
	if len(got) != 1 || len(got[0]) != 1 || len(got[0][0].samples) != 6 {
		t.Fatalf("%+v", got)
	}
	want := map[string]string{"__name__": "http_requests_total", "cluster": "c", "code": "200", "container": "app", "instance": "10.0.0.1:9090", "namespace": "qa", "node": "n1", "pod": "web-1", "scrape": "pod"}
	for _, l := range got[0][0].labels {
		if want[l.name] != l.value {
			t.Errorf("label %s=%s", l.name, l.value)
		}
	}
	if got := call(from, to, matcher{0, "__name__", "http_requests_total"}); len(got[0]) != 2 {
		t.Errorf("both series: %+v", got)
	}
	// pushed from 09:25: the read ends before it
	p.pushedFrom.Store(time.Date(2026, 10, 11, 9, 25, 0, 0, time.UTC).Unix())
	got = call(from, to, matcher{0, "__name__", "up"})
	if len(got[0]) != 1 || len(got[0][0].samples) != 3 {
		t.Errorf("trimmed to before the push: %+v", got)
	}
	if got := call(time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC), to, matcher{0, "__name__", "up"}); len(got[0]) != 0 {
		t.Errorf("inside the push: %+v", got)
	}
}

// Following loads only the raw objects no load has read: a second poll
// with nothing new pushes nothing, a new object is all a third pushes.
func TestFollowNewKeysOnly(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	key := func(at time.Time, id string) string {
		return filepath.Join(dir, "raw1/c", at.Format("2006-01-02/15"), "n1", at.Format("0405")+"-"+id+".jsonl.zst")
	}
	line := func(at time.Time, v float64) map[string]interface{} {
		return scrape(at.Format(time.RFC3339Nano), "web-1", metricSample("up", "gauge", v, map[string]string{}))
	}
	t1 := now.Add(-3 * time.Minute)
	writeRaw(t, key(t1, "a"), []map[string]interface{}{line(t1, 1)})
	var writes atomic.Int32
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writes.Add(1) }))
	defer prom.Close()
	p := &promServer{sf: sessionFlags{cluster: "c", blobsDir: dir, maxObjects: 100}}
	if err := p.follow(t.Context(), prom.URL, now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if p.pushed.Load() != 1 || writes.Load() != 1 {
		t.Fatalf("first poll: %d samples in %d writes", p.pushed.Load(), writes.Load())
	}
	if err := p.follow(t.Context(), prom.URL, now); err != nil || writes.Load() != 1 {
		t.Fatalf("nothing new: %d writes, %v", writes.Load(), err)
	}
	t2 := now.Add(-time.Minute)
	writeRaw(t, key(t2, "b"), []map[string]interface{}{line(t2, 0)})
	if err := p.follow(t.Context(), prom.URL, now); err != nil || p.pushed.Load() != 2 || writes.Load() != 2 {
		t.Fatalf("a new object: %d samples in %d writes, %v", p.pushed.Load(), writes.Load(), err)
	}
}

// A wrapper that runs the server as its child, as ystack's y-prometheus
// does: stopping the wrapper's group ends the child too, and stopGroup
// returns only when the group is empty.
func TestStopGroupEndsAWrappersChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60; true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	stopGroup(cmd, 5*time.Second)
	if err := syscall.Kill(-cmd.Process.Pid, 0); err != syscall.ESRCH {
		t.Fatalf("the group is still there: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s: SIGINT did not end the group, SIGKILL did", d)
	}
}
