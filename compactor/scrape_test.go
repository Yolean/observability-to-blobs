package main

import (
	"testing"
	"time"
)

// An hour folded from scrape lines with workload names has scrape and
// the names in its rows, and its day too.
func TestScrapeAndWorkloadNames(t *testing.T) {
	w := newWorld(t)
	l := scrapeLine(h10, "ns1", "p1", "u1", "app", 0, "rq_total", 2)
	l["k8s_deployment_name"], l["k8s_replicaset_name"] = "web", "web-5d8f7c9b4f"
	w.put(rawKey(h10, "n1", "1000-a"), l)
	w.fold(h10, h10.Add(2*time.Hour))
	hourly := w.from(w.c.l.hourlyStem("metrics", h10))
	if n := w.q("SELECT count(*) FROM " + hourly + " WHERE scrape = 'pod' AND name = 'rq_total' AND k8s_deployment_name = 'web' AND k8s_replicaset_name = 'web-5d8f7c9b4f'"); n != 1 {
		t.Fatalf("%d hourly rq_total rows with scrape and the workload names, want 1", n)
	}
	w.rollup(day)
	if n := w.q("SELECT count(*) FROM " + w.dailySource("metrics", day) + " WHERE scrape = 'pod' AND name = 'rq_total' AND k8s_deployment_name = 'web'"); n != 1 {
		t.Fatalf("%d daily rq_total rows, want 1", n)
	}
}
