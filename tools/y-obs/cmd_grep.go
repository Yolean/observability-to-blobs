package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const grepUsage = `Usage: y-obs grep PATTERN [flags]

Log lines matching PATTERN across pods and hours: the newest --limit
matches, oldest first, as '[NAMESPACE/POD/CONTAINER] TIMESTAMP MESSAGE'.
PATTERN is a substring, or with --regex an RE2 expression.

Scope it with -n or --name: without either it reads every pod of the
cluster in the window and usually hits --max-objects. --count prints the
matches per pod and container instead of lines: the cheap first pass.

Examples:
  y-obs grep 'connection reset' -n qa --since 2h
  y-obs grep OutOfMemory --name '^gateway-' --since 1d --count
  y-obs grep 'timeout|refused' --regex -i -n qa --since 6h --limit 50
  y-obs grep panic -n qa --since 7d --limit 0 --out inv.duckdb:panics

Flags:
`

func cmdGrep(ctx context.Context, args []string) error {
	return runGrep(ctx, args, os.Stdout, os.Stderr, time.Now())
}

func runGrep(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) error {
	c := newSessionCommand("grep", grepUsage, 100)
	c.format = "text"
	c.fs.Lookup("format").DefValue = "text"
	var ns, name, container, ansi string
	var regex, ignoreCase, count bool
	var limit int
	c.fs.StringVar(&ns, "namespace", "", "only this namespace")
	c.fs.StringVar(&ns, "n", "", "short for --namespace")
	c.fs.StringVar(&name, "name", "", "pod names matching this RE2 pattern")
	c.fs.StringVar(&container, "container", "", "only this container")
	c.fs.BoolVar(&regex, "regex", false, "PATTERN is an RE2 expression, not a substring")
	c.fs.BoolVar(&ignoreCase, "ignore-case", false, "match case-insensitively")
	c.fs.BoolVar(&ignoreCase, "i", false, "short for --ignore-case")
	c.fs.BoolVar(&count, "count", false, "matches per pod and container instead of lines")
	c.fs.StringVar(&ansi, "ansi", "", "strip: remove terminal escape sequences before matching and in the output")
	if err := c.parse(args); err != nil {
		return err
	}
	if c.fs.NArg() != 1 {
		return fail(exitUsage, "grep takes one PATTERN, quoted; run y-obs grep -h")
	}
	limit = c.limit
	if !c.limitSet() {
		limit = 100
	}
	c.limit = 0 // the newest matches are cut in SQL, oldest first after
	pattern := c.fs.Arg(0)
	if ansi != "" && ansi != "strip" {
		return fail(exitUsage, "--ansi %q: strip, or leave it out", ansi)
	}
	message := "message"
	if ansi == "strip" {
		message = "regexp_replace(message, " + ansiCSI + ", '', 'g')"
	}
	var match string
	switch {
	case regex:
		p := pattern
		if ignoreCase {
			p = "(?i)" + p
		}
		if _, err := regexp.Compile(p); err != nil {
			return fail(exitUsage, "PATTERN %q: %v", pattern, err)
		}
		match = "regexp_matches(message, " + sqlString(p) + ")"
	case ignoreCase:
		match = "contains(lower(message), " + sqlString(strings.ToLower(pattern)) + ")"
	default:
		match = "contains(message, " + sqlString(pattern) + ")"
	}
	where := []string{"pod IS NOT NULL"}
	if ns != "" {
		where = append(where, "namespace = "+sqlString(ns))
	}
	if name != "" {
		if _, err := regexp.Compile(name); err != nil {
			return fail(exitUsage, "--name %q: %v", name, err)
		}
		where = append(where, "regexp_matches(pod, "+sqlString(name)+")")
	}
	if container != "" {
		where = append(where, "container = "+sqlString(container))
	}
	if c.sf.node != "" {
		where = append(where, "node = "+sqlString(c.sf.node))
	}
	sel := `WITH g AS (
  SELECT DISTINCT time, namespace, pod, pod_uid, container, restart, message FROM (
    SELECT time, namespace, pod, pod_uid, container, restart, ` + message + ` AS message
    FROM w_logs WHERE ` + strings.Join(where, " AND ") + `)
  WHERE ` + match + `)`
	var q, table string
	formatSet := false
	c.fs.Visit(func(f *flag.Flag) { formatSet = formatSet || f.Name == "format" })
	if count {
		if !formatSet {
			c.format = "tsv"
		}
		table = "grep_count"
		q = sel + `
SELECT namespace, pod, pod_uid, container, count(*) AS matches, min(time) AS first, max(time) AS last
FROM g GROUP BY ALL ORDER BY matches DESC, namespace, pod, container`
		c.limit = limit
	} else {
		table = "grep"
		last := sel + ",\nt AS (SELECT * FROM g ORDER BY time DESC, namespace, pod, container, message"
		if limit > 0 {
			last += " LIMIT " + strconv.Itoa(limit)
		}
		last += ")\n"
		if c.format == "text" {
			q = last + `SELECT concat_ws(' ', '[' || namespace || '/' || pod || '/' || coalesce(container, '') || ']',
  strftime(time, '%Y-%m-%dT%H:%M:%S.') || lpad(CAST(epoch_ns(time) % 1000000000 AS VARCHAR), 9, '0') || 'Z', message) AS line
FROM t ORDER BY time, namespace, pod, container, message`
		} else {
			q = last + "SELECT time, namespace, pod, pod_uid, container, restart, message FROM t ORDER BY time, namespace, pod, container, message"
		}
	}
	h := hook{scope: map[string]string{"logs": strings.Join(where, " AND ")}, cut: func(ctx context.Context, s *session, printed int) string {
		if count || limit == 0 || printed < limit {
			return ""
		}
		n, err := s.strings(ctx, sel+" SELECT CAST(count(*) AS VARCHAR) FROM g")
		if err != nil || len(n) == 0 {
			return ""
		}
		total, _ := strconv.Atoi(n[0])
		if total <= printed {
			return ""
		}
		return fmt.Sprintf("%d earlier matches not shown: --count for matches per pod, a narrower -n/--name/--since, or --limit 0", total-printed)
	}}
	return runInvestigation(ctx, c, args, []string{"logs"}, ns, q, table, stdout, stderr, now, h)
}
