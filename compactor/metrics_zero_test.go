package main

import (
	"strings"
	"testing"
)

// Before any run, every job's success and failure counters are there at
// 0, and the process's start time and resident memory.
func TestMetricsFromTheStart(t *testing.T) {
	var b strings.Builder
	newMetrics("c").write(&b)
	for _, want := range []string{
		`blobs_compactor_runs_total{cluster="c",job="rolling",result="failure"} 0` + "\n",
		`blobs_compactor_runs_total{cluster="c",job="past",result="success"} 0` + "\n",
		`blobs_compactor_runs_total{cluster="c",job="rollup",result="failure"} 0` + "\n",
		"process_start_time_seconds 1",
		"process_resident_memory_bytes ",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, b.String())
		}
	}
}
