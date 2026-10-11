package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func logsRun(ctx context.Context, args []string, o, e *bytes.Buffer) error {
	return runLogs(ctx, args, o, e, time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC))
}

func TestLogs(t *testing.T) {
	dir := t.TempDir()
	l := func(at, container string, restart int, msg string) map[string]interface{} {
		r := logLine(at, msg)
		r["container"], r["restart"] = container, restart
		return r
	}
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/09/n1/a.jsonl.zst"), []map[string]interface{}{
		l("2026-10-11T09:01:00.000000001Z", "app", 0, "starting"),
		l("2026-10-11T09:02:00Z", "app", 0, "error: one"),
		l("2026-10-11T09:03:00Z", "app", 1, "starting again"),
		l("2026-10-11T09:04:00Z", "app", 1, "\x1b[31merror: two\x1b[0m"),
		l("2026-10-11T09:05:00Z", "side", 0, "sidecar line"),
		{"table": "logs", "ts": "2026-10-11T09:06:00Z", "cluster": "c", "node": "n1", "stream": "journal", "unit": "kubelet.service", "message": "journal line"},
	})
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"qa", "web-1"}, "starting\nerror: one\nstarting again\n\x1b[31merror: two\x1b[0m\nsidecar line\n"},
		{[]string{"qa", "web-1", "app", "--tail", "1", "--timestamps", "--prefix"}, "[pod/web-1/app] 2026-10-11T09:04:00.000000000Z \x1b[31merror: two\x1b[0m\n"},
		{[]string{"qa", "web-1", "--grep", "error", "--ansi", "strip"}, "error: one\nerror: two\n"},
		{[]string{"qa", "web-1", "--grep", "^start"}, "starting\nstarting again\n"},
		{[]string{"qa", "web-1", "app", "--previous"}, "starting\nerror: one\n"},
		{[]string{"qa", "web-1", "--restart", "1", "--timestamps"}, "2026-10-11T09:03:00.000000000Z starting again\n2026-10-11T09:04:00.000000000Z \x1b[31merror: two\x1b[0m\n"},
		{[]string{"_node", "n1", "--prefix"}, "[node/n1/kubelet.service] journal line\n"},
	} {
		out, _, err := runCmd(t, logsRun, dir, c.args...)
		if err != nil || out != c.want {
			t.Errorf("%v: %q %v, want %q", c.args, out, err, c.want)
		}
	}
	if _, e, err := runCmd(t, logsRun, dir, "qa", "web-1", "--tail", "2"); err != nil || !strings.Contains(e, "3 earlier lines not shown") {
		t.Errorf("cut note: %q %v", e, err)
	}
	if out, _, err := runCmd(t, logsRun, dir, "qa", "web-1", "app", "--tail", "1", "--format", "jsonl"); err != nil ||
		out != `{"time":"2026-10-11T09:04:00.000000000Z","container":"app","restart":1,"pod_uid":"u1","message":"\u001b[31merror: two\u001b[0m"}`+"\n" {
		t.Errorf("jsonl: %q %v", out, err)
	}
	if _, _, err := runCmd(t, logsRun, dir, "qa", "web-1", "nope"); exitOf(err) != exitNotFound || !strings.Contains(err.Error(), "it has: app, side") {
		t.Errorf("unknown container: %v", err)
	}
	if _, _, err := runCmd(t, logsRun, dir, "qa", "web-2"); exitOf(err) != exitNotFound || !strings.Contains(err.Error(), "closest with lines: web-1") {
		t.Errorf("unknown pod: %v", err)
	}
	if _, _, err := runCmd(t, logsRun, dir, "qa", "web-1", "--grep", "absent"); exitOf(err) != exitNoRows {
		t.Errorf("no match: %v", err)
	}
	if _, _, err := runCmd(t, logsRun, dir, "qa"); exitOf(err) != exitUsage {
		t.Errorf("no pod: %v", err)
	}
}
