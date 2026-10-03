package main

import (
	"testing"
	"time"
)

var (
	day = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h10 = day.Add(10 * time.Hour)
	h11 = day.Add(11 * time.Hour)
)

// The objects of hour 10: one incarnation's logs in two uploads, the
// second uploaded twice (a retry after an unacknowledged success), a
// journal line, scrapes, the pod's events, and another namespace's pod.
func (w *world) putA() string {
	return w.put(rawKey(h10, "n1", "0000-a"), append(logs(h10, 5, "ns1", "p1", "u1"),
		podLine("pod-observed", h10, "ns1", "p1", "u1"),
		containerLine(h10, "ns1", "p1", "u1", "app", 0),
		scrapeLine(h10.Add(10*time.Second), "ns1", "p1", "u1", "app", 0, "requests", 1))...)
}

func bLines() []line {
	t := h10.Add(30 * time.Minute)
	return append(logs(t, 5, "ns1", "p1", "u1"), journalLine(t, "k3s up"),
		scrapeLine(t, "ns1", "p1", "u1", "app", 0, "requests", 2))
}

func (w *world) putB() string { return w.put(rawKey(h10, "n1", "3000-b"), bLines()...) }

func (w *world) putBRetry() string { return w.put(rawKey(h10, "n1", "3000-b2"), bLines()...) }

func (w *world) putC() string {
	return w.put(rawKey(h10, "n2", "0100-c"), append(logs(h10.Add(time.Minute), 3, "ns2", "q1", "u2"),
		podLine("pod-observed", h10, "ns2", "q1", "u2"),
		scrapeLine(h10.Add(time.Minute), "ns2", "q1", "u2", "app", 0, "requests", 3))...)
}

// hour 11: the incarnation of hour 10 goes on logging, exits 0 and is
// deleted; ns2's pod logs on
func (w *world) putE() string {
	return w.put(rawKey(h11, "n1", "0000-e"), append(logs(h11, 5, "ns1", "p1", "u1"),
		exitLine(h11.Add(5*time.Second), "ns1", "p1", "u1", "app", 0, 0),
		podLine("pod-deleted", h11.Add(10*time.Second), "ns1", "p1", "u1"))...)
}

func (w *world) putF() string {
	return w.put(rawKey(h11, "n2", "0000-f"), logs(h11.Add(2*time.Minute), 3, "ns2", "q1", "u2")...)
}

const h10Logs, h10Metrics = 5 + 5 + 1 + 3, 2 + 2 + 2

// before and after h10's grace (10m)
var (
	during  = h10.Add(40 * time.Minute)
	beforeG = h10.Add(65 * time.Minute)
	afterG  = h10.Add(71 * time.Minute)
)

func TestIncrementalFoldEqualsFullFold(t *testing.T) {
	inc := newWorld(t)
	inc.putA()
	if r := inc.fold(h10, h10.Add(5*time.Minute)); !r.wrote || r.full || r.keys != 1 {
		t.Fatalf("first fold: %+v", r)
	}
	inc.putB()
	inc.putBRetry()
	if r := inc.fold(h10, during); r.keys != 2 || r.full {
		t.Fatalf("second fold took %d keys, full %v; want the 2 new ones", r.keys, r.full)
	}
	inc.putC()
	if r := inc.fold(h10, beforeG); r.keys != 1 || r.final {
		t.Fatalf("third fold: %+v", r)
	}
	if r := inc.fold(h10, beforeG); r.wrote {
		t.Fatal("a fold with no new keys wrote")
	}

	full := newWorld(t)
	full.putA()
	full.putB()
	full.putBRetry()
	full.putC()
	if r := full.fold(h10, afterG); !r.full || !r.final || r.keys != 4 {
		t.Fatalf("full fold: %+v", r)
	}

	for _, tbl := range []string{"logs", "metrics", "podindex", "pods"} {
		sameRows(t, inc.c.db, "hourly "+tbl, inc.from(inc.c.l.hourlyStem(tbl, h10)), full.from(full.c.l.hourlyStem(tbl, h10)))
	}
	if n := inc.hourlyRows("logs", h10); n != h10Logs {
		t.Fatalf("logs rows %d, want %d: DISTINCT folds the retried upload", n, h10Logs)
	}
	if n := inc.hourlyRows("metrics", h10); n != h10Metrics {
		t.Fatalf("metrics rows %d, want %d", n, h10Metrics)
	}
	f := inc.folded(h10)
	if len(f) != 4 {
		t.Fatalf("sidecar lists %d keys, want 4", len(f))
	}
	for k, final := range f {
		if final {
			t.Fatalf("incremental fold marked %s final", k)
		}
	}
	for k, final := range full.folded(h10) {
		if !final {
			t.Fatalf("full read left %s not final", k)
		}
	}
}

func TestCrashBeforeSidecarRefoldsWithoutDuplicates(t *testing.T) {
	w := newWorld(t)
	w.putA()
	w.fold(h10, h10.Add(5*time.Minute))
	b := w.putB()
	crashes := 0
	w.c.beforeCommit = func(string) error { crashes++; return errCrash }
	if _, err := w.c.fold(w.ctx, h10, nil, during); err != errCrash {
		t.Fatalf("fold err %v, want the crash", err)
	}
	if _, ok := w.folded(h10)[b]; ok {
		t.Fatal("the crashed fold committed its key")
	}
	if n := w.hourlyRows("logs", h10); n != 5+5+1 {
		t.Fatalf("the crashed fold's objects hold %d logs rows, want its 11", n)
	}
	w.c.beforeCommit = nil
	if r := w.fold(h10, during); r.keys != 1 {
		t.Fatalf("refold took %d keys, want the uncommitted 1", r.keys)
	}
	if n := w.hourlyRows("logs", h10); n != 5+5+1 {
		t.Fatalf("after the refold: %d logs rows, want 11 without duplicates", n)
	}
	if n := w.hourlyRows("metrics", h10); n != 4 {
		t.Fatalf("after the refold: %d metrics rows, want 4", n)
	}
	if crashes != 1 {
		t.Fatalf("%d crashes", crashes)
	}
}

func TestFullReadMarksFinalAndRepairs(t *testing.T) {
	w := newWorld(t)
	w.putA()
	w.putB()
	w.fold(h10, during)
	// an incremental fold's bug: a row no raw object holds
	conn, err := w.c.conn(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	logsKey := w.path(w.obj(w.c.l.hourlyStem("logs", h10)).key)
	for _, q := range []string{
		"CREATE TEMP TABLE l AS SELECT * FROM read_parquet(" + logsKey + ")",
		"COPY (SELECT * FROM l UNION ALL (SELECT * REPLACE ('bogus' AS message) FROM l LIMIT 1)) TO " + logsKey + " (FORMAT parquet)",
	} {
		if _, err := conn.ExecContext(w.ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	if r := w.fold(h10, beforeG); r.wrote || r.final {
		t.Fatalf("before the grace, with nothing new: %+v", r)
	}
	r := w.fold(h10, afterG)
	if !r.full || !r.final || !r.wrote || r.keys != 2 {
		t.Fatalf("the first fold after the grace: %+v, want the full read of 2 keys", r)
	}
	if n := w.q("SELECT count(*) FROM " + w.from(w.c.l.hourlyStem("logs", h10)) + " WHERE message = 'bogus'"); n != 0 {
		t.Fatalf("the full read kept %d bogus rows", n)
	}
	if n := w.hourlyRows("logs", h10); n != 11 {
		t.Fatalf("logs rows %d, want 11", n)
	}
	if r := w.fold(h10, afterG.Add(3*time.Hour)); r.wrote || r.full || !r.final {
		t.Fatalf("a final hour with nothing new: %+v", r)
	}
	// a late key in a final hour: incremental, never a full read again
	w.putC()
	if r := w.fold(h10, afterG.Add(3*time.Hour)); !r.wrote || r.full || r.keys != 1 || !r.final {
		t.Fatalf("late key: %+v", r)
	}
	f := w.folded(h10)
	if f[rawKey(h10, "n2", "0100-c")] || !f[rawKey(h10, "n1", "0000-a")] {
		t.Fatalf("sidecar after the late key: %v", f)
	}
}

func TestLateKeyAfterFinalIsDeletedOnlyAfterItsCommit(t *testing.T) {
	w := newWorld(t)
	w.c.rawDelete = true
	a, b := w.putA(), w.putB()
	if r := w.fold(h10, during); len(r.deleted) != 0 || !w.exists(a) {
		t.Fatal("deleted raw of an hour that is not final")
	}
	r := w.fold(h10, afterG)
	if !r.final || len(r.deleted) != 2 || w.exists(a) || w.exists(b) {
		t.Fatalf("after the final fold: %+v, a exists %v, b exists %v", r, w.exists(a), w.exists(b))
	}
	late := w.putC()
	w.c.beforeCommit = func(string) error { return errCrash }
	if _, err := w.c.fold(w.ctx, h10, nil, afterG.Add(time.Hour)); err != errCrash {
		t.Fatalf("fold err %v, want the crash", err)
	}
	if !w.exists(late) {
		t.Fatal("a key no committed sidecar lists was deleted")
	}
	w.c.beforeCommit = nil
	r = w.fold(h10, afterG.Add(time.Hour))
	if r.full || r.keys != 1 || len(r.deleted) != 1 || w.exists(late) {
		t.Fatalf("late fold: %+v, late exists %v", r, w.exists(late))
	}
	// the union kept the deleted keys' rows
	if n := w.hourlyRows("logs", h10); n != h10Logs {
		t.Fatalf("logs rows %d, want %d", n, h10Logs)
	}
	if f := w.folded(h10); len(f) != 3 {
		t.Fatalf("sidecar lists %d keys, want 3", len(f))
	}
}

func TestDeletionResumesAfterACrashBetweenCommitAndDelete(t *testing.T) {
	w := newWorld(t)
	a := w.putA()
	w.fold(h10, afterG) // final, deletion still off: as if it crashed after the commit
	w.c.rawDelete = true
	if r := w.fold(h10, afterG); r.wrote || len(r.deleted) != 1 || w.exists(a) {
		t.Fatalf("%+v, a exists %v", r, w.exists(a))
	}
}

// A sidecar that expires while its hour's raw objects remain: the next
// pass folds them again, and the rollup's DISTINCT removes the repeat.
func TestExpiredSidecar(t *testing.T) {
	w := newWorld(t)
	w.putA()
	w.putB()
	w.putC()
	w.fold(h10, afterG)
	w.rollup(day)
	before := w.q("SELECT count(*) FROM " + w.dailySource("logs", day))

	// the sidecar alone expired: the next fold is a full read again
	w.remove(w.c.l.hourlySidecar(h10))
	if err := w.c.past(w.ctx, day.Add(30*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	if f := w.folded(h10); len(f) != 3 {
		t.Fatalf("refolded sidecar lists %d keys", len(f))
	}
	if n := w.hourlyRows("logs", h10); n != h10Logs {
		t.Fatalf("refolded logs rows %d", n)
	}
	// and the hourly objects too, then a late key arrives
	w.remove(w.c.l.hourlySidecar(h10))
	for _, tbl := range []string{"logs", "metrics", "podindex", "pods"} {
		w.remove(w.obj(w.c.l.hourlyStem(tbl, h10)).key)
	}
	w.putBRetry()
	if err := w.c.past(w.ctx, day.Add(31*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	after := w.q("SELECT count(*) FROM " + w.dailySource("logs", day))
	if before != h10Logs || after != before {
		t.Fatalf("daily logs %d before, %d after the refolds; want %d both", before, after, h10Logs)
	}
	if n := w.q("SELECT count(*) FROM read_csv(" + w.path(w.c.l.dailySidecar(day)) + ", header = true)"); n != 4 {
		t.Fatalf("daily sidecar lists %d keys, want 4", n)
	}
}
