package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// wholeDay compares the daily tier with one read of every raw key of the
// day through sql/, DISTINCT, and pods over those rows: what folding the
// whole day at once would write.
func (w *world) wholeDay(t *testing.T, keys []string) {
	t.Helper()
	conn, err := w.c.conn(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	urls := make([]string, len(keys))
	for i, k := range keys {
		urls[i] = w.c.raw.URL(k)
	}
	if err := setRawFiles(w.ctx, conn, urls); err != nil {
		t.Fatal(err)
	}
	if err := loadSQL(w.ctx, conn); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TEMP TABLE logs AS SELECT DISTINCT * EXCLUDE (filename) FROM logs",
		"CREATE TEMP TABLE metrics AS SELECT DISTINCT * EXCLUDE (filename) FROM metrics",
		"CREATE TEMP TABLE podindex AS SELECT DISTINCT * EXCLUDE (filename) FROM podindex",
		"CREATE TEMP TABLE apiserver AS SELECT DISTINCT * EXCLUDE (filename) FROM apiserver",
		"COPY logs TO " + sqlString(filepath.Join(w.dir, "whole-logs.parquet")) + " (FORMAT parquet)",
		"COPY metrics TO " + sqlString(filepath.Join(w.dir, "whole-metrics.parquet")) + " (FORMAT parquet)",
		"COPY podindex TO " + sqlString(filepath.Join(w.dir, "whole-podindex.parquet")) + " (FORMAT parquet)",
		"COPY apiserver TO " + sqlString(filepath.Join(w.dir, "whole-apiserver.parquet")) + " (FORMAT parquet)",
		"COPY (SELECT * FROM pods) TO " + sqlString(filepath.Join(w.dir, "whole-pods.parquet")) + " (FORMAT parquet)",
	} {
		if _, err := conn.ExecContext(w.ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, tbl := range []string{"logs", "metrics", "podindex", "apiserver", "pods"} {
		sameRows(t, w.c.db, "daily "+tbl, w.dailySource(tbl, day),
			"read_parquet("+sqlString(filepath.Join(w.dir, "whole-"+tbl+".parquet"))+")")
	}
}

func TestRollupEqualsFoldingTheWholeDay(t *testing.T) {
	w := newWorld(t)
	keys := []string{w.putA(), w.putB(), w.putBRetry(), w.putC(), w.putE()}
	w.fold(h10, afterG)
	w.fold(h11, h11.Add(20*time.Minute))
	r := w.rollup(day)
	if len(r.hours) != 2 || r.namespaces != 3 {
		t.Fatalf("first rollup: %+v, want 2 hours and namespaces ns1, ns2, _node", r)
	}
	w.wholeDay(t, keys)
	w.obj(w.c.l.dailyPartitionStem("logs", day, "_node"))
	w.obj(w.c.l.dailyPartitionStem("metrics", day, "ns2"))
	if r := w.rollup(day); len(r.hours) != 0 {
		t.Fatalf("a rollup with nothing new: %+v", r)
	}

	// a late key of hour 11: the rollup rewrites the namespaces of hour
	// 11's objects (ns1, ns2) and keeps the others' (_node) as they are
	node := filepath.Join(w.dir, w.obj(w.c.l.dailyPartitionStem("logs", day, "_node")).key)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(node, old, old); err != nil {
		t.Fatal(err)
	}
	keys = append(keys, w.putF())
	w.fold(h11, h11.Add(30*time.Minute))
	r = w.rollup(day)
	if len(r.hours) != 1 || !r.hours[0].Equal(h11) || r.namespaces != 2 {
		t.Fatalf("late rollup: %+v, want hour 11 and namespaces ns1, ns2", r)
	}
	if fi, err := os.Stat(node); err != nil || !fi.ModTime().Equal(old) {
		t.Fatalf("the late rollup rewrote _node's logs: %v", err)
	}
	w.wholeDay(t, keys)
}

func TestPodsRecomputedPerTier(t *testing.T) {
	w := newWorld(t)
	w.putA()
	w.putB()
	w.putE()
	w.fold(h10, afterG)
	w.fold(h11, h11.Add(20*time.Minute))
	w.rollup(day)
	pods := func(src string) (rows, lines int64, phase string) {
		t.Helper()
		if err := w.c.db.QueryRowContext(w.ctx, "SELECT count(*), sum(lines), any_value(phase) FROM "+src+
			" WHERE pod_uid = 'u1' AND container = 'app'").Scan(&rows, &lines, &phase); err != nil {
			t.Fatal(err)
		}
		return
	}
	if rows, lines, phase := pods(w.from(w.c.l.hourlyStem("pods", h10))); rows != 1 || lines != 10 || phase != "running" {
		t.Fatalf("hour 10 pods: %d rows, %d lines, %s; want 1, 10, running", rows, lines, phase)
	}
	if rows, lines, phase := pods(w.from(w.c.l.hourlyStem("pods", h11))); rows != 1 || lines != 5 || phase != "succeeded" {
		t.Fatalf("hour 11 pods: %d rows, %d lines, %s; want 1, 5, succeeded", rows, lines, phase)
	}
	// the day's row is an aggregate over the day's rows, not a concatenation
	if rows, lines, phase := pods(w.dailySource("pods", day)); rows != 1 || lines != 15 || phase != "succeeded" {
		t.Fatalf("daily pods: %d rows, %d lines, %s; want 1, 15, succeeded", rows, lines, phase)
	}
	if n := w.q("SELECT count(*) FROM " + w.dailySource("pods", day) + " WHERE pod_uid = 'u1' AND first_log = TIMESTAMP_NS '2026-10-01 10:00:00' AND last_log = TIMESTAMP_NS '2026-10-01 11:00:04' AND metric_names = ['requests', 'up']"); n != 1 {
		t.Fatal("daily pods row lacks the day's first_log, last_log or metric names")
	}

	// a rollup of an hour with only ns2 keeps ns1's pods row
	h12 := day.Add(12 * time.Hour)
	w.put(rawKey(h12, "n2", "0000-g"), append(logs(h12, 2, "ns2", "q1", "u2"),
		podLine("pod-observed", h12, "ns2", "q1", "u2"))...)
	w.fold(h12, h12.Add(time.Minute))
	if r := w.rollup(day); r.namespaces != 1 {
		t.Fatalf("rollup of the late ns2 key rewrote %d namespaces", r.namespaces)
	}
	if rows, lines, _ := pods(w.dailySource("pods", day)); rows != 1 || lines != 15 {
		t.Fatalf("ns1's pods row after an ns2 rollup: %d rows, %d lines", rows, lines)
	}
	if n := w.q("SELECT count(*) FROM " + w.dailySource("pods", day) + " WHERE pod_uid = 'u2'"); n != 1 {
		t.Fatalf("ns2's pods rows: %d, want 1", n)
	}
}

func TestRollupCrashBeforeDailySidecar(t *testing.T) {
	w := newWorld(t)
	keys := []string{w.putA(), w.putB(), w.putC()}
	w.fold(h10, afterG)
	w.rollup(day)
	keys = append(keys, w.putE())
	w.fold(h11, h11.Add(20*time.Minute))
	w.c.beforeCommit = func(string) error { return errCrash }
	if _, err := w.c.rollup(w.ctx, day); err != errCrash {
		t.Fatalf("rollup err %v, want the crash", err)
	}
	w.c.beforeCommit = nil
	if r := w.rollup(day); len(r.hours) != 1 {
		t.Fatalf("after the crash: %+v, want hour 11 again", r)
	}
	w.wholeDay(t, keys)
}

func TestRollupOfAnHourWithoutObjects(t *testing.T) {
	w := newWorld(t)
	w.putA()
	w.fold(h10, afterG)
	for _, tbl := range []string{"logs", "metrics", "podindex", "apiserver", "pods"} {
		w.remove(w.obj(w.c.l.hourlyStem(tbl, h10)).key)
	}
	if r := w.rollup(day); len(r.hours) != 1 || r.rows["logs"] != 0 {
		t.Fatalf("%+v", r)
	}
	if !w.exists(w.c.l.dailySidecar(day)) {
		t.Fatal("no daily sidecar")
	}
}
