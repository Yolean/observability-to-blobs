package main

import (
	"testing"
	"time"
)

// Raw objects from before job was renamed scrape and after it: an hour
// folded from both, and its day, have scrape for every row and no job,
// and the workload names where the agent wrote them.
func TestScrapeAcrossTheRename(t *testing.T) {
	w := newWorld(t)
	w.put(rawKey(h10, "n1", "1000-old"), legacyScrapeLine(h10, "ns1", "p1", "u1", "app", 0, "old_total", 1))
	w.fold(h10, h10.Add(20*time.Minute))
	newer := scrapeLine(h10.Add(time.Minute), "ns1", "p1", "u1", "app", 0, "new_total", 2)
	newer["k8s_deployment_name"], newer["k8s_replicaset_name"] = "web", "web-5d8f7c9b4f"
	w.put(rawKey(h10, "n1", "1100-new"), newer)
	// the hour's existing object, written from the older line, is read
	// back and united with the newer
	w.fold(h10, h10.Add(30*time.Minute))
	hourly := w.from(w.c.l.hourlyStem("metrics", h10))
	columns := func(from string) map[string]bool {
		rows, err := w.c.db.QueryContext(w.ctx, "SELECT column_name FROM (DESCRIBE SELECT * FROM "+from+")")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			out[c] = true
		}
		return out
	}
	if c := columns(hourly); c["job"] || !c["scrape"] || !c["k8s_deployment_name"] {
		t.Fatalf("hourly columns %v: want scrape and the workload names, no job", c)
	}
	if n := w.q("SELECT count(*) FROM " + hourly + " WHERE scrape IS DISTINCT FROM 'pod'"); n != 0 {
		t.Fatalf("%d hourly rows without scrape pod", n)
	}
	if n := w.q("SELECT count(*) FROM " + hourly + " WHERE name = 'new_total' AND k8s_deployment_name = 'web' AND k8s_replicaset_name = 'web-5d8f7c9b4f'"); n != 1 {
		t.Fatalf("%d new_total rows with the workload names, want 1", n)
	}
	w.fold(h10, h10.Add(2*time.Hour))
	w.rollup(day)
	daily := w.dailySource("metrics", day)
	if c := columns(daily); c["job"] || !c["scrape"] {
		t.Fatalf("daily columns %v: want scrape, no job", c)
	}
	if n := w.q("SELECT count(*) FROM " + daily + " WHERE scrape = 'pod' AND name IN ('old_total', 'new_total')"); n != 2 {
		t.Fatalf("%d daily rows of old_total and new_total with scrape pod, want 2", n)
	}
}

// A day whose hours were all folded before the rename: the rollup writes
// scrape from their job.
func TestScrapeFromOlderHourlyObjects(t *testing.T) {
	w := newWorld(t)
	w.put(rawKey(h10, "n1", "1000-old"), legacyScrapeLine(h10, "ns1", "p1", "u1", "app", 0, "old_total", 1))
	w.fold(h10, h10.Add(2*time.Hour))
	// an hourly object as the compactor wrote them before the rename
	stem := w.c.l.hourlyStem("metrics", h10)
	if _, err := w.c.db.ExecContext(w.ctx, "COPY (SELECT * EXCLUDE (scrape) REPLACE ('x' AS k8s_job_name), scrape AS job FROM "+
		w.from(stem)+") TO "+w.path(stem+".old.parquet")+" (FORMAT parquet)"); err != nil {
		t.Fatal(err)
	}
	w.remove(w.obj(stem).key)
	if _, err := w.c.db.ExecContext(w.ctx, "COPY (SELECT * FROM read_parquet("+w.path(stem+".old.parquet")+")) TO "+w.path(stem+".parquet")+" (FORMAT parquet)"); err != nil {
		t.Fatal(err)
	}
	w.remove(stem + ".old.parquet")
	w.rollup(day)
	if n := w.q("SELECT count(*) FROM " + w.dailySource("metrics", day) + " WHERE scrape = 'pod' AND name = 'old_total'"); n != 1 {
		t.Fatalf("%d daily old_total rows with scrape pod, want 1", n)
	}
}
