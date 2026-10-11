package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func grepRun(ctx context.Context, args []string, o, e *bytes.Buffer) error {
	return runGrep(ctx, args, o, e, time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC))
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	l := func(at, ns, pod, msg string) map[string]interface{} {
		r := logLine(at, msg)
		r["namespace"], r["pod"], r["pod_uid"] = ns, pod, pod+"-uid"
		return r
	}
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/09/n1/a.jsonl.zst"), []map[string]interface{}{
		l("2026-10-11T09:01:00Z", "qa", "web-1", "Connection reset by peer"),
		l("2026-10-11T09:02:00Z", "qa", "web-2", "connection reset"),
		l("2026-10-11T09:03:00Z", "qa", "web-1", "connection refused"),
		l("2026-10-11T09:04:00Z", "prod", "web-1", "connection reset"),
		{"table": "logs", "ts": "2026-10-11T09:05:00Z", "cluster": "c", "node": "n1", "stream": "journal", "unit": "x", "message": "connection reset in the journal"},
	})
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"connection reset", "-n", "qa"}, "[qa/web-2/app] 2026-10-11T09:02:00.000000000Z connection reset\n"},
		{[]string{"connection reset", "-n", "qa", "-i"}, "[qa/web-1/app] 2026-10-11T09:01:00.000000000Z Connection reset by peer\n[qa/web-2/app] 2026-10-11T09:02:00.000000000Z connection reset\n"},
		{[]string{"reset|refused", "--regex", "--name", "^web-1$"}, "[qa/web-1/app] 2026-10-11T09:01:00.000000000Z Connection reset by peer\n[qa/web-1/app] 2026-10-11T09:03:00.000000000Z connection refused\n[prod/web-1/app] 2026-10-11T09:04:00.000000000Z connection reset\n"},
		{[]string{"connection", "-n", "qa", "-i", "--count"}, "namespace\tpod\tpod_uid\tcontainer\tmatches\tfirst\tlast\nqa\tweb-1\tweb-1-uid\tapp\t2\t2026-10-11T09:01:00.000000000Z\t2026-10-11T09:03:00.000000000Z\nqa\tweb-2\tweb-2-uid\tapp\t1\t2026-10-11T09:02:00.000000000Z\t2026-10-11T09:02:00.000000000Z\n"},
	} {
		out, e, err := runCmd(t, grepRun, dir, c.args...)
		if err != nil || out != c.want {
			t.Errorf("%v: %q %v %s, want %q", c.args, out, err, e, c.want)
		}
	}
	if out, e, err := runCmd(t, grepRun, dir, "connection", "--limit", "1"); err != nil || strings.Count(out, "\n") != 1 || !strings.Contains(out, "prod/web-1") || !strings.Contains(e, "2 earlier matches not shown") {
		t.Errorf("--limit 1, the newest match: %q %q %v", out, e, err)
	}
	if _, _, err := runCmd(t, grepRun, dir, "absent", "-n", "qa"); exitOf(err) != exitNoRows {
		t.Errorf("no match: %v", err)
	}
	if _, _, err := runCmd(t, grepRun, dir, "(", "--regex"); exitOf(err) != exitUsage {
		t.Errorf("bad regex: %v", err)
	}
}
