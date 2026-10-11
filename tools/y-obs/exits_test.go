package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func exits(ctx context.Context, args []string, o, e *bytes.Buffer) error {
	return runExits(ctx, args, o, e, time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC))
}

func TestExits(t *testing.T) {
	dir := podsFixture(t)
	out, _, err := runCmd(t, exits, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "time\tnamespace\tpod\tpod_uid\tcontainer\trestart\texit_code\texit_reason\texit_message\tmemory_limit\tcpu_quota\tnode\n" +
		"2026-10-11T09:30:00.000000000Z\tqa\tjob-1\tu2\tmain\t0\t1\tError\t\t\t\tn1\n" +
		"2026-10-11T09:20:00.000000000Z\tqa\tweb-abc-x1y2z\tu1\tapp\t0\t137\tOOMKilled\t\t1048576\t\tn1\n"
	if out != want {
		t.Errorf("exits:\n%s\nwant\n%s", out, want)
	}
	if out, _, err := runCmd(t, exits, dir, "--reason", "OOMKilled", "--format", "csv"); err != nil || strings.Count(out, "\n") != 2 || !strings.Contains(out, "web-abc-x1y2z") {
		t.Errorf("--reason: %q %v", out, err)
	}
	if _, _, err := runCmd(t, exits, dir, "--uid", "u3"); exitOf(err) != exitNoRows {
		t.Errorf("unknown uid: %v", err)
	}
}
