package main

import (
	"runtime"
	"strings"
	"testing"
)

// Before any run, every job's success and failure counters are there at
// 0, and the process's start time and resident memory.
func TestMetricsFromTheStart(t *testing.T) {
	var b strings.Builder
	newMetrics("c").write(&b)
	want := []string{
		`blobs_compactor_runs_total{cluster="c",job="rolling",result="failure"} 0` + "\n",
		`blobs_compactor_runs_total{cluster="c",job="past",result="success"} 0` + "\n",
		`blobs_compactor_runs_total{cluster="c",job="rollup",result="failure"} 0` + "\n",
		"process_start_time_seconds 1",
	}
	// read from /proc, which a developer's macOS has not
	if runtime.GOOS == "linux" {
		want = append(want, "process_resident_memory_bytes ")
	}
	for _, want := range want {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, b.String())
		}
	}
}
