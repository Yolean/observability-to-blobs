package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// An empty plan is exit 4 with what the bucket has; a plan with objects
// and no matching rows stays exit 1.
func TestExplainEmpty(t *testing.T) {
	dir := fixture(t)
	run := func(args ...string) error {
		var o, e bytes.Buffer
		return runPods(context.Background(), append([]string{"--blobs-dir", dir}, args...), &o, &e, time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	}
	err := run("--cluster", "c", "--from", "2026-10-12 08:00", "--to", "2026-10-12 09:00")
	if exitOf(err) != exitNotFound || !strings.Contains(err.Error(), "cluster c has no data in 2026-10-12T08:00:00Z..2026-10-12T09:00:00Z; it has raw 2026-10-11..2026-10-11, hourly 2026-10-11..2026-10-11") {
		t.Errorf("window before the data: %v", err)
	}
	err = run("--cluster", "cc", "--from", "2026-10-11 09:00", "--to", "2026-10-11 10:00")
	if exitOf(err) != exitNotFound || !strings.Contains(err.Error(), `no cluster "cc" in the bucket; clusters with data: c`) {
		t.Errorf("a cluster typo: %v", err)
	}
	if err := run("--cluster", "c", "--from", "2026-10-11 09:00", "--to", "2026-10-11 10:00", "--name", "^nothing$"); exitOf(err) != exitNoRows {
		t.Errorf("objects, no match: %v", err)
	}
	var o, e bytes.Buffer
	err = runSQL(context.Background(), []string{"--blobs-dir", dir, "--cluster", "c", "--from", "2026-10-13 00:00", "--to", "2026-10-13 01:00", "SELECT * FROM logs"}, &o, &e, time.Now())
	if exitOf(err) != exitNotFound {
		t.Errorf("sql over nothing: %v", err)
	}
}
