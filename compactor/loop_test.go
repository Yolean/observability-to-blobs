package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The cadence over a day boundary: the previous hour is folded
// incrementally until its grace has passed, then read in full, and its
// day rolled up.
func TestRollingPassFinalizesThePreviousHourAndRollsUpItsDay(t *testing.T) {
	w := newWorld(t)
	h23 := day.Add(23 * time.Hour)
	next := day.Add(24 * time.Hour)
	w.put(rawKey(h23, "n1", "5900-a"), logs(h23.Add(59*time.Minute), 3, "ns1", "p1", "u1")...)
	w.put(rawKey(next, "n1", "0000-b"), logs(next, 2, "ns1", "p1", "u1")...)

	if err := w.c.rollingPass(w.ctx, next.Add(5*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	if f := w.folded(h23); len(f) != 1 || f[rawKey(h23, "n1", "5900-a")] {
		t.Fatalf("before the grace: sidecar %v, want the key, not final", f)
	}
	if !w.exists(w.c.l.hourlySidecar(next)) {
		t.Fatal("the current hour was not folded")
	}
	if w.exists(w.c.l.dailySidecar(day)) {
		t.Fatal("rolled up a day whose last hour is not final")
	}
	if err := w.c.rollingPass(w.ctx, next.Add(11*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	if f := w.folded(h23); !f[rawKey(h23, "n1", "5900-a")] {
		t.Fatalf("after the grace: sidecar %v, want final", f)
	}
	if n := w.q("SELECT count(*) FROM " + w.dailySource("logs", day)); n != 3 {
		t.Fatalf("daily logs %d, want 3", n)
	}
	if !w.c.finalSeen.Equal(h23) {
		t.Fatalf("finalSeen %s", w.c.finalSeen)
	}
	var b strings.Builder
	w.c.m.write(&b)
	for _, want := range []string{
		`blobs_compactor_last_success_timestamp_seconds{job="rolling"} `,
		`blobs_compactor_last_success_timestamp_seconds{job="rollup"} `,
		`blobs_compactor_rows{tier="daily",table="logs"} 3`,
		`blobs_compactor_raw_keys_read_total 3`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, b.String())
		}
	}
}

// The past pass finds every other hour with new keys from its listings,
// gives a skipped hour its full read, and rolls up past days.
func TestPastPass(t *testing.T) {
	w := newWorld(t)
	w.c.rawDelete = true
	a := w.putA()
	w.putE()
	now := day.Add(26 * time.Hour)
	cur := now.Truncate(time.Hour)
	current := w.put(rawKey(cur, "n1", "0000-z"), logs(cur, 1, "ns1", "p1", "u1")...)
	uploaded := time.Now().Add(-time.Hour)
	if err := os.Chtimes(w.c.raw.URL(current), uploaded, uploaded); err != nil {
		t.Fatal(err)
	}
	if err := w.c.past(w.ctx, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, h := range []time.Time{h10, h11} {
		if f := w.folded(h); len(f) != 1 {
			t.Fatalf("hour %s: sidecar %v", h, f)
		}
	}
	if w.exists(w.c.l.hourlySidecar(cur)) {
		t.Fatal("the past pass folded the current hour")
	}
	if w.exists(a) || !w.exists(current) {
		t.Fatal("deletion: want the final hours' keys gone, the current hour's kept")
	}
	if !w.exists(w.c.l.dailySidecar(day)) {
		t.Fatal("no rollup of the past day")
	}
	var b strings.Builder
	w.c.m.write(&b)
	for _, want := range []string{
		"blobs_compactor_raw_objects 1\n",
		"blobs_compactor_raw_deleted_total 2\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, b.String())
		}
	}
	if !strings.Contains(b.String(), "blobs_compactor_raw_oldest_age_seconds 36") {
		t.Fatalf("want the age of the object uploaded an hour ago:\n%s", b.String())
	}
}

// The raw gauges stay true when the rolling pass, not the past pass,
// deletes an hour: the age is the oldest object still there.
func TestRawGaugesFollowEveryPassesDeletions(t *testing.T) {
	w := newWorld(t)
	w.c.rawDelete = true
	h09 := day.Add(9 * time.Hour)
	upload := func(key string, at time.Time) {
		if err := os.Chtimes(w.c.raw.URL(key), at, at); err != nil {
			t.Fatal(err)
		}
	}
	k9 := w.put(rawKey(h09, "n1", "0030-a"), logs(h09.Add(30*time.Second), 2, "ns1", "p1", "u1")...)
	upload(k9, h09.Add(30*time.Second))
	k10 := w.put(rawKey(h10, "n1", "0030-b"), logs(h10.Add(30*time.Second), 2, "ns1", "p1", "u1")...)
	upload(k10, h10.Add(30*time.Second))
	if err := w.c.past(w.ctx, h10.Add(5*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	gauges := func() string {
		var b strings.Builder
		w.c.m.write(&b)
		return b.String()
	}
	w.c.m.now = func() time.Time { return h10.Add(6 * time.Minute) }
	for _, want := range []string{"blobs_compactor_raw_objects 2\n", "blobs_compactor_raw_oldest_age_seconds 3930\n"} {
		if g := gauges(); !strings.Contains(g, want) {
			t.Fatalf("after the past pass, metrics lack %q:\n%s", want, g)
		}
	}
	// the rolling pass after h09's grace: its final fold deletes k9
	if err := w.c.rollingPass(w.ctx, h10.Add(11*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	if w.exists(k9) {
		t.Fatal("k9 not deleted")
	}
	// a new object of the current hour, which the next rolling pass lists
	k10b := w.put(rawKey(h10, "n2", "3000-c"), logs(h10.Add(30*time.Minute), 1, "ns1", "p1", "u1")...)
	upload(k10b, h10.Add(30*time.Minute))
	if err := w.c.rollingPass(w.ctx, h10.Add(31*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	w.c.m.now = func() time.Time { return h10.Add(61 * time.Minute) }
	for _, want := range []string{"blobs_compactor_raw_objects 2\n", "blobs_compactor_raw_oldest_age_seconds 3630\n"} {
		if g := gauges(); !strings.Contains(g, want) {
			t.Fatalf("after the rolling pass deleted hour 09, metrics lack %q:\n%s", want, g)
		}
	}
}

// A stop during a pass lets the job in flight commit and starts no other.
func TestStopEndsAPassBetweenJobs(t *testing.T) {
	w := newWorld(t)
	w.putA()
	w.putE()
	h12 := day.Add(12 * time.Hour)
	w.put(rawKey(h12, "n1", "0000-g"), logs(h12, 2, "ns1", "p1", "u1")...)
	ctx, stop := context.WithCancel(w.ctx)
	defer stop()
	commits := 0
	w.c.beforeCommit = func(string) error { commits++; stop(); return nil }
	if err := w.c.past(ctx, day.Add(30*time.Hour), time.Minute); !errors.Is(err, errStopped) {
		t.Fatalf("past pass err %v, want stopped", err)
	}
	if commits != 1 || len(w.folded(h10)) != 1 {
		t.Fatalf("%d commits, hour 10 sidecar %v: want the job in flight committed", commits, w.folded(h10))
	}
	if w.exists(w.c.l.hourlySidecar(h11)) || w.exists(w.c.l.hourlySidecar(h12)) || w.exists(w.c.l.dailySidecar(day)) {
		t.Fatal("jobs ran after the stop")
	}

	// the rolling pass: the current hour's fold commits, the previous
	// hour's does not start
	w.c.beforeCommit = nil
	cur := day.Add(13 * time.Hour)
	w.put(rawKey(cur, "n1", "0000-h"), logs(cur, 2, "ns1", "p1", "u1")...)
	ctx, stop = context.WithCancel(w.ctx)
	w.c.beforeCommit = func(string) error { stop(); return nil }
	if err := w.c.rollingPass(ctx, cur.Add(20*time.Minute), time.Minute); !errors.Is(err, errStopped) {
		t.Fatalf("rolling pass err %v, want stopped", err)
	}
	if !w.exists(w.c.l.hourlySidecar(cur)) || w.exists(w.c.l.hourlySidecar(h12)) {
		t.Fatal("rolling pass: want the current hour committed and the previous not started")
	}
}

// listings records the prefixes a store is listed with.
type listings struct {
	dirBucket
	prefixes []string
}

func (l *listings) List(ctx context.Context, prefix string) ([]Object, error) {
	l.prefixes = append(l.prefixes, prefix)
	return l.dirBucket.List(ctx, prefix)
}

// The past pass never lists more than an hour of raw keys at once, nor
// more than a day of hourly sidecars: dates and hours come from listings
// with a delimiter.
func TestPastPassListsHourByHour(t *testing.T) {
	w := newWorld(t)
	raw := &listings{dirBucket: w.c.raw.(dirBucket)}
	w.c.raw = raw
	w.putA()
	w.putE()
	next := day.Add(24 * time.Hour)
	w.put(rawKey(next, "n1", "0000-x"), logs(next, 1, "ns1", "p1", "u1")...)
	if err := w.c.past(w.ctx, day.Add(50*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	want := []string{w.c.l.rawHourPrefix(h10), w.c.l.rawHourPrefix(h11), w.c.l.rawHourPrefix(next)}
	if strings.Join(raw.prefixes, " ") != strings.Join(want, " ") {
		t.Fatalf("raw listings %v, want %v", raw.prefixes, want)
	}
	for _, d := range []time.Time{day, next} {
		if !w.exists(w.c.l.dailySidecar(d)) {
			t.Fatalf("no rollup of %s", d)
		}
	}
}
