package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestKeys(t *testing.T) {
	l := layout{"itest"}
	h, ok := l.rawHour("raw1/itest/2026-10-01/14/node-a/0102-abc.jsonl.zst")
	if !ok || !h.Equal(time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("rawHour %s %v", h, ok)
	}
	for _, k := range []string{
		"raw1/other/2026-10-01/14/node-a/0102-abc.jsonl.zst",
		"raw1/itest/2026-10-01/14/0102-abc.jsonl.zst",
		"raw1/itest/2026-10-01/14/node-a/0102-abc.jsonl",
	} {
		if _, ok := l.rawHour(k); ok {
			t.Errorf("rawHour accepted %s", k)
		}
	}
	if got := tierName(l.dailyPartitionStem("logs", h, "x/y z"), "").key; got != "daily/itest/logs/2026-10-01/namespace=x%2Fy%20z/data_0.parquet" {
		t.Fatalf("dailyPartition %s", got)
	}
	if ns, ok := partitionNamespace(tierName(l.dailyPartitionStem("metrics", h, ""), "").key); !ok || ns != "" {
		t.Fatalf("the empty namespace: %q %v", ns, ok)
	}
	if ns, ok := partitionNamespace(tierName(l.dailyPartitionStem("metrics", h, "a-b"), "k-2").key); !ok || ns != "a-b" {
		t.Fatalf("an encrypted partition: %q %v", ns, ok)
	}
	if got, ok := sidecarHour(l.hourlySidecar(h)); !ok || !got.Equal(h) {
		t.Fatalf("sidecarHour %s", got)
	}
}

func TestTierNames(t *testing.T) {
	l := layout{"itest"}
	h := time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)
	stem := l.hourlyStem("logs", h)
	for _, tc := range []struct{ id, key string }{
		{"", "hourly/itest/logs/2026-10-01/14.parquet"},
		{"k-2026-10", "hourly/itest/logs/2026-10-01/14.k-k-2026-10.parquet"},
	} {
		o := tierName(stem, tc.id)
		if o.key != tc.key || o.keyID != tc.id || tierKeyID(o.key) != tc.id {
			t.Fatalf("tierName(%q) = %+v", tc.id, o)
		}
		if got, ok := stemObject(stem, o.key); !ok || got != o {
			t.Fatalf("stemObject(%s) = %+v %v", o.key, got, ok)
		}
	}
	for _, k := range []string{
		"hourly/itest/logs/2026-10-01/14.k-.parquet",
		"hourly/itest/logs/2026-10-01/14.k-A.parquet",
		"hourly/itest/logs/2026-10-01/14.parquet.tmp",
		"hourly/itest/logs/2026-10-01/14.x.parquet",
	} {
		if o, ok := stemObject(stem, k); ok {
			t.Errorf("stemObject accepted %s as %+v", k, o)
		}
	}
	if got := tierName(l.dailyStem("pods", h), "a").key; got != "daily/itest/pods/2026-10-01.k-a.parquet" {
		t.Fatalf("dailyStem %s", got)
	}
	// the plain readers' globs (README) do not match an encrypted name
	for _, g := range []struct{ glob, name string }{{"[0-9][0-9].parquet", "14.k-a.parquet"}, {"[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9].parquet", "2026-10-01.k-a.parquet"}} {
		if ok, _ := filepath.Match(g.glob, g.name); ok {
			t.Fatalf("%s matches %s", g.glob, g.name)
		}
	}
}
