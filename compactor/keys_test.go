package main

import (
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
	if got := l.dailyPartition("logs", h, "x/y z"); got != "daily/itest/logs/2026-10-01/namespace=x%2Fy%20z/data_0.parquet" {
		t.Fatalf("dailyPartition %s", got)
	}
	if ns, ok := partitionNamespace(l.dailyPartition("metrics", h, "")); !ok || ns != "" {
		t.Fatalf("the empty namespace: %q %v", ns, ok)
	}
	if got, ok := sidecarHour(l.hourlySidecar(h)); !ok || !got.Equal(h) {
		t.Fatalf("sidecarHour %s", got)
	}
}
