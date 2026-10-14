package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pi(at, kind, pod, uid string, extra map[string]interface{}) map[string]interface{} {
	r := map[string]interface{}{"table": "podindex", "time": at, "ts": at, "cluster": "c", "node": "n1",
		"kind": kind, "namespace": "qa", "pod": pod, "pod_uid": uid}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

// podsFixture: web-1 (u1) of a Deployment, running, its app container
// restarted once after an OOM kill; job-1 (u2) deleted after failing.
func podsFixture(t *testing.T) string {
	dir := t.TempDir()
	web := map[string]interface{}{"labels": map[string]string{"app": "web", "pod-template-hash": "abc"}, "pod_started": "2026-10-11T08:00:00Z"}
	job := map[string]interface{}{"labels": map[string]string{"job-name": "job"}, "pod_started": "2026-10-11T09:10:00Z"}
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/09/n1/a.jsonl.zst"), []map[string]interface{}{
		pi("2026-10-11T09:00:00Z", "pod-checkpoint", "web-abc-x1y2z", "u1", web),
		pi("2026-10-11T09:05:00Z", "container", "web-abc-x1y2z", "u1", map[string]interface{}{"container": "app", "restart": 0, "memory_limit": 1 << 20}),
		pi("2026-10-11T09:20:00Z", "container-exit", "web-abc-x1y2z", "u1", map[string]interface{}{"container": "app", "restart": 0, "exit_code": 137, "exit_reason": "OOMKilled"}),
		pi("2026-10-11T09:21:00Z", "container", "web-abc-x1y2z", "u1", map[string]interface{}{"container": "app", "restart": 1, "memory_limit": 1 << 20}),
		pi("2026-10-11T09:10:00Z", "pod-observed", "job-1", "u2", job),
		pi("2026-10-11T09:11:00Z", "container", "job-1", "u2", map[string]interface{}{"container": "main", "restart": 0}),
		pi("2026-10-11T09:30:00Z", "container-exit", "job-1", "u2", map[string]interface{}{"container": "main", "restart": 0, "exit_code": 1, "exit_reason": "Error"}),
		pi("2026-10-11T09:31:00Z", "pod-deleted", "job-1", "u2", job),
	})
	return dir
}

func runCmd(t *testing.T, run func(context.Context, []string, *bytes.Buffer, *bytes.Buffer) error, dir string, args ...string) (string, string, error) {
	t.Helper()
	var o, e bytes.Buffer
	args = append([]string{"--cluster", "c", "--blobs-dir", dir, "--from", "2026-10-11 09:00", "--to", "2026-10-11 10:00"}, args...)
	err := run(context.Background(), args, &o, &e)
	return o.String(), e.String(), err
}

func pods(ctx context.Context, args []string, o, e *bytes.Buffer) error {
	return runPods(ctx, args, o, e, time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC))
}

func TestPods(t *testing.T) {
	dir := podsFixture(t)
	out, errOut, err := runCmd(t, pods, dir, "--format", "jsonl")
	if err != nil {
		t.Fatal(err, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("pods: %s", out)
	}
	for _, want := range []string{`"pod":"job-1"`, `"workload":"job"`, `"owner_kind":"Job"`, `"ended":"2026-10-11T09:31:00.000000000Z"`, `"phase":"failed"`, `"last_exit":"Error 1"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("job-1 lacks %s: %s", want, lines[0])
		}
	}
	for _, want := range []string{`"workload":"web"`, `"owner_kind":"Deployment"`, `"ended":null`, `"phase":"running"`, `"restarts":1`, `"last_exit":"OOMKilled 137"`, `"containers":["app"]`} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("web lacks %s: %s", want, lines[1])
		}
	}
	if !strings.Contains(errOut, "hours raw=1") {
		t.Errorf("summary: %s", errOut)
	}
	for _, c := range []struct {
		args []string
		pods string
	}{
		{[]string{"--label", "app=web"}, "web-abc-x1y2z"},
		{[]string{"--phase", "failed"}, "job-1"},
		{[]string{"--name", "^job-"}, "job-1"},
		{[]string{"--workload", "web"}, "web-abc-x1y2z"},
		{[]string{"--at", "2026-10-11 09:05"}, "web-abc-x1y2z"},
		{[]string{"--at", "2026-10-11 09:20"}, "job-1,web-abc-x1y2z"},
		{[]string{"--at", "2026-10-11 09:40"}, "web-abc-x1y2z"},
	} {
		out, _, err := runCmd(t, pods, dir, append(c.args, "--format", "csv")...)
		var got []string
		for i, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if i > 0 && l != "" {
				got = append(got, strings.Split(l, ",")[1])
			}
		}
		if strings.Join(got, ",") != c.pods {
			t.Errorf("%v: %v (%v)", c.args, got, err)
		}
	}
	if _, _, err := runCmd(t, pods, dir, "--phase", "gone"); exitOf(err) != exitUsage {
		t.Errorf("bad phase: %v", err)
	}
	if _, _, err := runCmd(t, pods, dir, "--label", "app=none"); exitOf(err) != exitNoRows {
		t.Errorf("no match: %v", err)
	}
}

// The --limit note is a line of its own with the command's own advice,
// and the summary stays the last line.
func TestLimitNoteBeforeSummary(t *testing.T) {
	dir := podsFixture(t)
	_, e, err := runCmd(t, pods, dir, "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(e), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[len(lines)-1], "y-obs: cluster=c") ||
		lines[len(lines)-2] != "y-obs: more rows than --limit 1: narrow with -n, --name, --label, --phase or a shorter window, or raise --limit" {
		t.Errorf("stderr: %q", e)
	}
}

// Without a window flag pods lists the pods alive now; --at takes now
// and a duration before now.
func TestPodsAtDefaultsToNow(t *testing.T) {
	dir := podsFixture(t)
	for _, c := range []struct {
		args []string
		pods string
	}{
		{nil, "web-abc-x1y2z"},
		{[]string{"--at", "now"}, "web-abc-x1y2z"},
		{[]string{"--at", "-40m"}, "job-1,web-abc-x1y2z"},
		{[]string{"--at=-55m"}, "web-abc-x1y2z"},
		{[]string{"--since", "1h"}, "job-1,web-abc-x1y2z"},
	} {
		var o, e bytes.Buffer
		err := pods(context.Background(), append([]string{"--cluster", "c", "--blobs-dir", dir, "--format", "csv"}, c.args...), &o, &e)
		var got []string
		for i, l := range strings.Split(strings.TrimSpace(o.String()), "\n") {
			if i > 0 && l != "" {
				got = append(got, strings.Split(l, ",")[1])
			}
		}
		if strings.Join(got, ",") != c.pods {
			t.Errorf("%v: %v (%v) %s", c.args, got, err, e.String())
		}
	}
	var o, e bytes.Buffer
	if err := pods(context.Background(), []string{"--cluster", "c", "--blobs-dir", dir, "--at", "-soon"}, &o, &e); exitOf(err) != exitUsage || !strings.Contains(err.Error(), "-30m") {
		t.Errorf("bad --at: %v", err)
	}
}
