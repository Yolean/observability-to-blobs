package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// The guard (guard.go): a line whose table the fold does not write fails
// the fold, with nothing written and no raw deleted; a logs or metrics
// row without a namespace fails the rollup.

func withTable(l line, table any) line {
	out := line{}
	for k, v := range l {
		out[k] = v
	}
	if table == nil {
		delete(out, "table")
	} else {
		out["table"] = table
	}
	return out
}

func TestGuardRefusesALineWithoutAFoldedTable(t *testing.T) {
	at := h10.Add(20 * time.Minute)
	logl := logLine(at, "ns1", "p1", "u1", "app", 0, "x")
	for _, c := range []struct {
		name  string
		lines []line
	}{
		// an agent newer than the compactor, or a table's earlier name
		// (the cluster agent's was objects before apiserver)
		{"unknown table", []line{withTable(objectLine("object-added", at, "c9", "9"), "objects"), withTable(logl, "traces")}},
		// an input that an overlay's pipeline-extra.yaml adds, tagged blob, without a table
		{"no table", []line{withTable(logl, nil)}},
		// an agent with a field the compactor does not read: newer, or
		// older than a rename that did not retire the old name
		{"unknown field", []line{withFields(logl, "job", "pod"), withFields(logl, "job", "pod", "zzz", 1)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.c.rawDelete = true
			var logged bytes.Buffer
			w.c.log = slog.New(slog.NewTextHandler(&logged, nil))
			a := w.putA()
			if r := w.fold(h10, h10.Add(5*time.Minute)); !r.wrote {
				t.Fatal("the first fold wrote nothing")
			}
			rows := w.hourlyRows("logs", h10)
			bad := w.put(rawKey(h10, "n1", "1000-bad"), append([]line{logLine(at, "ns1", "p1", "u1", "app", 0, "good")}, c.lines...)...)
			for _, now := range []time.Time{during, afterG} {
				_, err := w.c.foldJob(w.ctx, h10, nil, now, time.Minute)
				var e *unaccountedError
				if !errors.As(err, &e) || e.n != int64(len(c.lines)) || e.sample != bad || e.period != "2026-10-01T10" {
					t.Fatalf("fold at %s: %v, want the guard's refusal of %d lines in %s", now, err, len(c.lines), bad)
				}
				// nothing written, nothing final, nothing deleted
				if f := w.folded(h10); len(f) != 1 || f[a] {
					t.Fatalf("sidecar after the refusal: %v", f)
				}
				if n := w.hourlyRows("logs", h10); n != rows {
					t.Fatalf("logs rows %d after the refusal, want the %d of before", n, rows)
				}
				if !w.exists(a) || !w.exists(bad) {
					t.Fatal("raw deleted by a refused fold")
				}
			}
			if s := "level=ERROR msg=unaccounted job=fold period=2026-10-01T10 count=" + fmt.Sprint(len(c.lines)) +
				" sample_key=" + bad; !strings.Contains(logged.String(), s) {
				t.Errorf("log lacks %q:\n%s", s, logged.String())
			}
			var b strings.Builder
			w.c.m.write(&b)
			if s := "blobs_compactor_unaccounted_total{cluster=\"itest\"} " + fmt.Sprint(2*len(c.lines)) + "\n"; !strings.Contains(b.String(), s) {
				t.Errorf("metrics lack %q:\n%s", s, b.String())
			}
			// the object moved away: the hour folds, final, and deletes its raw
			w.remove(bad)
			if r, err := w.c.foldJob(w.ctx, h10, nil, afterG, time.Minute); err != nil || !r.final || len(r.deleted) != 1 {
				t.Fatalf("after the object went: %+v %v", r, err)
			}
		})
	}
}

func withFields(l line, kv ...any) line {
	out := line{}
	for k, v := range l {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// The refusal of an unknown field names each field with its count of
// lines; a field raw_retired names is no refusal.
func TestGuardNamesUnknownFieldsAndSparesRetiredOnes(t *testing.T) {
	w := newWorld(t)
	var logged bytes.Buffer
	w.c.log = slog.New(slog.NewTextHandler(&logged, nil))
	at := h10.Add(20 * time.Minute)
	logl := logLine(at, "ns1", "p1", "u1", "app", 0, "x")
	bad := w.put(rawKey(h10, "n1", "1000-bad"), logl, withFields(logl, "job", "pod"),
		withFields(scrapeLine(at, "ns1", "p1", "u1", "app", 0, "up", 1), "job", "pod", "zzz", 1))
	_, err := w.c.foldJob(w.ctx, h10, nil, during, time.Minute)
	var e *unaccountedError
	if !errors.As(err, &e) || e.n != 2 || e.fields != "job:2,zzz:1" || e.sample != bad {
		t.Fatalf("fold: %v, want the refusal of 2 lines with job:2,zzz:1 in %s", err, bad)
	}
	if s := "sample_key=" + bad + " fields=job:2,zzz:1"; !strings.Contains(logged.String(), s) {
		t.Errorf("log lacks %q:\n%s", s, logged.String())
	}

	conn, err := w.c.conn(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	urls := []string{w.c.raw.URL(bad)}
	if err := setRawFiles(w.ctx, conn, urls); err != nil {
		t.Fatal(err)
	}
	if err := loadSQL(w.ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(w.ctx, "CREATE OR REPLACE VIEW raw_retired AS SELECT unnest(['job', 'zzz']) AS field"); err != nil {
		t.Fatal(err)
	}
	if err := guardFields(w.ctx, conn, h10, []string{bad}, urls); err != nil {
		t.Fatalf("retired fields refused: %v", err)
	}
}

// A logs row without a namespace in an hourly object (the fold writes
// none: tables.sql makes it _node) fails the rollup, as no daily
// partition would hold it.
func TestGuardRefusesARollupOfARowWithoutNamespace(t *testing.T) {
	w := newWorld(t)
	w.putA()
	w.putC()
	w.fold(h10, afterG)
	key := w.obj(w.c.l.hourlyStem("logs", h10)).key
	p := w.path(key)
	if _, err := w.c.db.ExecContext(w.ctx, "COPY (SELECT * REPLACE (CASE WHEN namespace = 'ns2' THEN NULL ELSE namespace END AS namespace) FROM read_parquet("+
		p+")) TO "+p+" (FORMAT parquet)"); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	w.c.log = slog.New(slog.NewTextHandler(&logged, nil))
	_, err := w.c.rollupJob(w.ctx, day, time.Minute)
	var e *unaccountedError
	if !errors.As(err, &e) || e.n != 3 || e.sample != key || e.period != "2026-10-01" {
		t.Fatalf("rollup: %v, want the guard's refusal: 3 rows of %s", err, key)
	}
	if objs, err := w.c.tiers.List(w.ctx, "daily/"); err != nil || len(objs) != 0 {
		t.Fatalf("the refused rollup wrote %v %v", objs, err)
	}
	if s := "msg=unaccounted job=rollup period=2026-10-01 count=3 sample_key=" + key; !strings.Contains(logged.String(), s) {
		t.Errorf("log lacks %q:\n%s", s, logged.String())
	}
	var b strings.Builder
	w.c.m.write(&b)
	if s := "blobs_compactor_unaccounted_total{cluster=\"itest\"} 3\n"; !strings.Contains(b.String(), s) {
		t.Errorf("metrics lack %q:\n%s", s, b.String())
	}
}
