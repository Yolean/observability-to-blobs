package main

import (
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

const exitsUsage = `Usage: y-obs exits [flags]

Why containers ended: every container exit the window's pod index saw,
newest first, with the limits in force for that run.

Columns: time namespace pod pod_uid container restart exit_code
exit_reason exit_message memory_limit cpu_quota node. exit_reason
OOMKilled is the container's cgroup limit; 137 with Error is a SIGKILL,
after a deletion's grace period or under node pressure (compare time with
y-obs pods' ended for the pod).

Examples:
  y-obs exits -n qa --since 1d
  y-obs exits --reason OOMKilled --since 7d
  y-obs exits --uid 3f2a... --since 2d
  y-obs exits --failed -n qa --since 6h --format jsonl

Flags:
`

func cmdExits(ctx context.Context, args []string) error {
	return runExits(ctx, args, os.Stdout, os.Stderr, time.Now())
}

func runExits(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) error {
	c := newSessionCommand("exits", exitsUsage, 50)
	var ns, name, uid, container, reason string
	var failed bool
	c.fs.StringVar(&ns, "namespace", "", "only this namespace")
	c.fs.StringVar(&ns, "n", "", "short for --namespace")
	c.fs.StringVar(&name, "name", "", "pod names matching this RE2 pattern")
	c.fs.StringVar(&uid, "uid", "", "only this incarnation")
	c.fs.StringVar(&container, "container", "", "only this container")
	c.fs.StringVar(&reason, "reason", "", "only this exit reason: OOMKilled, Error, Completed, ...")
	c.fs.BoolVar(&failed, "failed", false, "only exits with a non-zero code")
	if err := c.parse(args); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return fail(exitUsage, "exits takes no arguments, only flags; run y-obs exits -h")
	}
	where := []string{"kind = 'container-exit'"}
	if ns != "" {
		where = append(where, "namespace = "+sqlString(ns))
	}
	if name != "" {
		if _, err := regexp.Compile(name); err != nil {
			return fail(exitUsage, "--name %q: %v", name, err)
		}
		where = append(where, "regexp_matches(pod, "+sqlString(name)+")")
	}
	if uid != "" {
		where = append(where, "pod_uid = "+sqlString(uid))
	}
	if container != "" {
		where = append(where, "container = "+sqlString(container))
	}
	if reason != "" {
		where = append(where, "exit_reason = "+sqlString(reason))
	}
	if failed {
		where = append(where, "exit_code <> 0")
	}
	if c.sf.node != "" {
		where = append(where, "node = "+sqlString(c.sf.node))
	}
	// a retried upload repeats rows: one per exit
	q := `WITH e AS (
  SELECT DISTINCT time, namespace, pod, pod_uid, container, restart, exit_code, exit_reason, exit_message, node
  FROM w_podindex WHERE ` + strings.Join(where, " AND ") + `),
r AS (
  SELECT pod_uid, container, restart, any_value(memory_limit) AS memory_limit, any_value(cpu_quota) AS cpu_quota
  FROM w_podindex WHERE kind = 'container' GROUP BY ALL)
SELECT e.time, e.namespace, e.pod, e.pod_uid, e.container, e.restart, e.exit_code, e.exit_reason, e.exit_message,
  r.memory_limit, r.cpu_quota, e.node
FROM e LEFT JOIN r ON r.pod_uid = e.pod_uid AND r.container = e.container AND r.restart IS NOT DISTINCT FROM e.restart
ORDER BY e.time DESC`
	// the query reads the exits and the container rows: once
	return runInvestigation(ctx, c, args, []string{"podindex"}, ns, q, "exits", stdout, stderr, now,
		hook{scope: map[string]string{"podindex": "kind IN ('container-exit', 'container')"}})
}
