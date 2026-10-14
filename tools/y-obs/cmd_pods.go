package main

import (
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

const podsUsage = `Usage: y-obs pods [flags]

Which pods ran in the window: one row per incarnation (pod_uid), newest
knowledge per pod, from the pods index of the tiers.

Columns: namespace pod pod_uid node workload owner_kind started ended
phase restarts last_exit containers. phase is running (no deletion seen),
succeeded, failed or unknown (deleted, no exit seen). started is the
earliest sandbox start seen in the window (a new sandbox, after a node
restart, starts later). ended is when the deletion was seen; empty when
not seen, which is not proof of running. workload and owner_kind come
from labels, when the window has the pod's labels.

--at T keeps the pods alive at T: started at or before it, and ended
after it, or, without a deletion seen, seen within the hour before it
(the gap rule). Without --from/--to the window is then T-1h..T+1h.

Examples:
  y-obs pods -n qa --name '^web-' --since 6h
  y-obs pods --label app=gateway --at '2026-10-11 09:00'
  y-obs pods --node worker-3 --since 1d --phase failed
  y-obs pods -n qa --since 1d --out inv.duckdb

Flags:
`

func cmdPods(ctx context.Context, args []string) error {
	return runPods(ctx, args, os.Stdout, os.Stderr, time.Now())
}

func runPods(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) error {
	c := newSessionCommand("pods", podsUsage, 50)
	var ns, name, uid, workload, phase, at string
	var labels stringList
	c.fs.StringVar(&ns, "namespace", "", "only this namespace")
	c.fs.StringVar(&ns, "n", "", "short for --namespace")
	c.fs.StringVar(&name, "name", "", "pod names matching this RE2 pattern (unanchored: ^ and $ anchor)")
	c.fs.Var(&labels, "label", "key=value a pod's labels must hold; repeatable")
	c.fs.StringVar(&uid, "uid", "", "only this incarnation")
	c.fs.StringVar(&workload, "workload", "", "only this workload (Deployment, StatefulSet, Job name)")
	c.fs.StringVar(&phase, "phase", "", "running, succeeded, failed or unknown")
	c.fs.StringVar(&at, "at", "", "only pods alive at this time, UTC")
	if err := c.parse(args); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return fail(exitUsage, "pods takes no arguments, only flags; run y-obs pods -h")
	}
	var where, having []string
	if ns != "" {
		where = append(where, "namespace = "+sqlString(ns))
	}
	if name != "" {
		if _, err := regexp.Compile(name); err != nil {
			return fail(exitUsage, "--name %q: %v", name, err)
		}
		where = append(where, "regexp_matches(pod, "+sqlString(name)+")")
	}
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return fail(exitUsage, "--label %q: use key=value", l)
		}
		where = append(where, "labels["+sqlString(k)+"] = "+sqlString(v))
	}
	if uid != "" {
		where = append(where, "pod_uid = "+sqlString(uid))
	}
	if workload != "" {
		where = append(where, "workload = "+sqlString(workload))
	}
	if c.sf.node != "" {
		where = append(where, "node = "+sqlString(c.sf.node))
	}
	switch phase {
	case "":
	case "running", "succeeded", "failed", "unknown":
		having = append(having, "phase = "+sqlString(phase))
	default:
		return fail(exitUsage, "--phase %q: running, succeeded, failed or unknown", phase)
	}
	if at != "" {
		t, err := parseTime(at)
		if err != nil {
			return fail(exitUsage, "--at: %v", err)
		}
		if c.sf.from == "" && c.sf.to == "" {
			c.sf.from, c.sf.to = t.Add(-time.Hour).Format(time.RFC3339), t.Add(time.Hour).Format(time.RFC3339)
		}
		ts := "TIMESTAMP_NS " + sqlString(t.Format("2006-01-02 15:04:05.000000000"))
		having = append(having, "started <= "+ts, "coalesce(ended, last_seen + INTERVAL 1 HOUR) >= "+ts)
	}
	agg := `SELECT any_value(namespace) AS namespace, any_value(pod) AS pod, pod_uid, any_value(node) AS node,
  coalesce(any_value(workload) FILTER (owner_kind IS NOT NULL), any_value(workload)) AS workload, any_value(owner_kind) AS owner_kind,
  coalesce(min(pod_started), min(first_log)) AS started, max(pod_deleted) AS ended,
  max_by(phase, last_seen) AS phase, max(restart) AS restarts,
  max_by(CASE WHEN exit_reason IS NOT NULL THEN exit_reason || ' ' || exit_code END, container_exited) AS last_exit,
  string_agg(DISTINCT container, ',' ORDER BY container) FILTER (WHERE coalesce(container, '') <> '') AS containers,
  coalesce(max(last_seen), max(last_log)) AS last_seen
FROM w_pods`
	if len(where) > 0 {
		agg += "\nWHERE " + strings.Join(where, " AND ")
	}
	agg += "\nGROUP BY pod_uid"
	// the per-pod conditions name the aggregated columns: an outer WHERE
	q := "SELECT * EXCLUDE (last_seen) FROM (" + agg + ")"
	if len(having) > 0 {
		q += "\nWHERE " + strings.Join(having, " AND ")
	}
	q += "\nORDER BY namespace, pod, started"
	return runInvestigation(ctx, c, args, []string{"pods"}, ns, q, "pods", stdout, stderr, now)
}
