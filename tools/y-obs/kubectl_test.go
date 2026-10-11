package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func writeRaw(t *testing.T, path string, lines []map[string]interface{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	for _, l := range lines {
		b, _ := json.Marshal(l)
		zw.Write(append(b, '\n'))
	}
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// configMaps are the names of the ConfigMaps in a state.
func configMaps(lines []Line) string {
	var names []string
	for _, l := range lines {
		if l.Resource == "configmaps" {
			names = append(names, l.Name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// The state at a time from the raw tier, and from the hourly tier written
// from the same rows: asof_test.go's first snapshot and the events after it.
func TestStateAt(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-07/10/n1/0000-a.jsonl.zst"), []map[string]interface{}{
		cm("10:00", "object-snapshot", "a", "ua", "10"),
		cm("10:00", "object-snapshot", "b", "ub", "11"),
		cm("10:00", "snapshot", "", "", ""),
		cm("10:10", "object-modified", "a", "ua", "13"),
		cm("10:12", "object-added", "d", "ud1", "14"),
		cm("10:20", "object-deleted", "b", "ub", "17"),
	})
	ctx := context.Background()
	sf := &sessionFlags{cluster: "c", blobsDir: dir, maxObjects: 10}
	at := time.Date(2026, 10, 7, 10, 15, 0, 0, time.UTC)

	lines, minor, err := stateAt(ctx, sf, at, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if got := configMaps(lines); got != "a,b,d" || minor != "" {
		t.Errorf("raw at 10:15: %s, minor %q", got, minor)
	}
	if _, _, err := stateAt(ctx, sf, at, "tier"); exitOf(err) != exitNoRows {
		t.Errorf("no tier objects yet: %v", err)
	}

	// the hourly tier's apiserver object for 10, from the raw view
	s, err := openSession(ctx, sf, window{from: at.Truncate(time.Hour), to: at.Truncate(time.Hour).Add(time.Hour)}, storeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.prepare(ctx, map[string]bool{"raw": true}, 10); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "hourly/c/apiserver/2026-10-07")
	os.MkdirAll(out, 0o755)
	if err := s.exec(ctx, "COPY (SELECT * EXCLUDE (filename) FROM apiserver) TO "+sqlString(filepath.Join(out, "10.parquet"))); err != nil {
		t.Fatal(err)
	}
	s.close()
	os.RemoveAll(filepath.Join(dir, "raw1"))
	if lines, _, err = stateAt(ctx, sf, at.Add(10*time.Minute), "tier"); err != nil {
		t.Fatal(err)
	}
	if got := configMaps(lines); got != "a,d" {
		t.Errorf("hourly at 10:25: %s", got)
	}
	if _, _, err := stateAt(ctx, sf, time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC), "tier"); exitOf(err) != exitNoRows {
		t.Errorf("before the first snapshot: %v", err)
	}
}
