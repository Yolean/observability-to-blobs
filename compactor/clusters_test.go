package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Without CLUSTER_NAME the fleet finds the clusters under raw1/, leaves
// out what is no cluster name, and gives each cluster what a compactor of
// its own writes, and its own series in the one /metrics.
func TestFleetCompactsEveryCluster(t *testing.T) {
	w := newWorld(t)
	other := func(key string) string { return strings.Replace(key, "raw1/"+testCluster+"/", "raw1/b/", 1) }
	w.put(rawKey(h10, "n1", "1000-a"), logs(h10, 3, "ns1", "p1", "u1")...)
	w.put(other(rawKey(h10, "n1", "1000-a")), logs(h10, 2, "ns1", "p1", "u1")...)
	w.put(strings.Replace(rawKey(h10, "n1", "1000-a"), testCluster, "Not_A_Cluster", 1), logs(h10, 1, "ns1", "p1", "u1")...)

	f := newFleet(w.c, "")
	if err := f.discover(w.ctx, true, time.Minute); err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", testCluster}; !reflect.DeepEqual(f.names, want) {
		t.Fatalf("clusters %v, want %v", f.names, want)
	}
	now := day.Add(26 * time.Hour)
	f.each(w.ctx, func(c *compactor) {
		if err := c.past(w.ctx, now, time.Minute); err != nil {
			t.Fatalf("%s: %v", c.l.cluster, err)
		}
	})
	for cluster, n := range map[string]int64{testCluster: 3, "b": 2} {
		cw := *w
		cw.c = f.byName[cluster]
		if got := cw.hourlyRows("logs", h10); got != n {
			t.Errorf("%s: %d hourly logs rows, want %d", cluster, got, n)
		}
		if !w.exists(cw.c.l.dailySidecar(day)) {
			t.Errorf("%s: no rollup", cluster)
		}
	}
	rec := httptest.NewRecorder()
	f.set.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{
		"# TYPE blobs_compactor_raw_keys_read_total counter\nblobs_compactor_raw_keys_read_total{cluster=\"b\"} 1\nblobs_compactor_raw_keys_read_total{cluster=\"itest\"} 1\n",
		`blobs_compactor_last_success_timestamp_seconds{cluster="b",job="rollup"} `,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, rec.Body.String())
		}
	}
}

// A cluster whose raw objects are all folded and deleted is found under
// hourly/, on a past pass's discovery, so it still gets its rollups; a
// rolling pass's discovery lists raw1/ only.
func TestFleetFindsAFoldedCluster(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(filepath.Join(w.dir, "hourly", "gone", "_folded"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.put(rawKey(h10, "n1", "1000-a"), logs(h10, 1, "ns1", "p1", "u1")...)
	f := newFleet(w.c, "")
	if err := f.discover(w.ctx, false, time.Minute); err != nil || !reflect.DeepEqual(f.names, []string{testCluster}) {
		t.Fatalf("raw1/ only: %v %v", f.names, err)
	}
	if err := f.discover(w.ctx, true, time.Minute); err != nil || !reflect.DeepEqual(f.names, []string{"gone", testCluster}) {
		t.Fatalf("with hourly/: %v %v", f.names, err)
	}
}

// With CLUSTER_NAME the fleet is that cluster's compactor alone, and
// discovery lists nothing.
func TestFleetOfOneCluster(t *testing.T) {
	w := newWorld(t)
	w.put(strings.Replace(rawKey(h10, "n1", "1000-a"), testCluster, "b", 1), logs(h10, 1, "ns1", "p1", "u1")...)
	f := newFleet(w.c, testCluster)
	if err := f.discover(w.ctx, true, time.Minute); err != nil || !reflect.DeepEqual(f.names, []string{testCluster}) || f.byName[testCluster] != w.c {
		t.Fatalf("%v %v", f.names, err)
	}
}
