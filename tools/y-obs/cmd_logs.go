package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const logsUsage = `Usage: y-obs logs NAMESPACE POD [CONTAINER] [flags]

One pod's log lines in the window, all containers merged by time or one
container, oldest first: the logs API's options, plus a window and the
pod's incarnations and runs. Works for deleted pods and past days.

The node's journal is namespace _node with the node as POD and the
systemd unit as CONTAINER: y-obs logs _node worker-3 kubelet.service

--tail applies after --grep: the last N matching lines. Lines are
'[pod/POD/CONTAINER] TIMESTAMP MESSAGE', the first two parts with
--prefix and --timestamps; --format jsonl gives time, namespace, pod,
pod_uid, container, restart, stream and message as fields instead, and
--out keeps those typed columns.

Examples:
  y-obs logs qa web-1 --tail 200 --timestamps
  y-obs logs qa web-1 app --grep error --since 3h
  y-obs logs qa web-1 --uid 3f2a... --previous
  y-obs logs ci build-abc-verify-pod step-verify --since 2d --tail 0 --out inv.duckdb:verify

Flags:
`

func cmdLogs(ctx context.Context, args []string) error {
	return runLogs(ctx, args, os.Stdout, os.Stderr, time.Now())
}

// ansiCSI matches a CSI escape sequence (ESC [, parameter and
// intermediate bytes, a final byte), as the logs API strips them.
const ansiCSI = `chr(27) || '\[[0-?]*[ -/]*[@-~]'`

func runLogs(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) error {
	c := newSessionCommand("logs", logsUsage, 0)
	c.format = "text"
	c.fs.Lookup("format").DefValue = "text"
	var tail, restart int
	var grep, uid, ansi string
	var timestamps, prefix, previous bool
	c.fs.IntVar(&tail, "tail", 100, "the last N lines, after --grep; 0 for every line")
	c.fs.StringVar(&grep, "grep", "", "lines containing this, or with a leading ^ starting with it")
	c.fs.BoolVar(&timestamps, "timestamps", false, "prefix each line with its timestamp")
	c.fs.BoolVar(&prefix, "prefix", false, "prefix each line with [pod/POD/CONTAINER]")
	c.fs.StringVar(&ansi, "ansi", "", "strip: remove terminal escape sequences before --grep and in the output")
	c.fs.StringVar(&uid, "uid", "", "only this incarnation of the pod")
	c.fs.IntVar(&restart, "restart", -1, "only this run (the kubelet restart count)")
	c.fs.BoolVar(&previous, "previous", false, "only the run before each container's latest")
	if err := c.parse(args); err != nil {
		return err
	}
	if n := c.fs.NArg(); n < 2 || n > 3 {
		return fail(exitUsage, "logs takes NAMESPACE POD [CONTAINER]; run y-obs logs -h")
	}
	ns, pod, container := c.fs.Arg(0), c.fs.Arg(1), c.fs.Arg(2)
	if ansi != "" && ansi != "strip" {
		return fail(exitUsage, "--ansi %q: strip, or leave it out", ansi)
	}
	if tail < 0 {
		tail = 0
	}
	journal := ns == "_node"
	where := []string{"namespace = " + sqlString(ns)}
	who, part := "pod", "container"
	if journal {
		who, part = "node", "unit"
	}
	where = append(where, who+" = "+sqlString(pod))
	if container != "" {
		where = append(where, part+" = "+sqlString(container))
	}
	if uid != "" {
		where = append(where, "pod_uid = "+sqlString(uid))
	}
	if restart >= 0 {
		where = append(where, "restart = "+strconv.Itoa(restart))
	}
	message := "message"
	if ansi == "strip" {
		message = "regexp_replace(message, " + ansiCSI + ", '', 'g')"
	}
	var match string
	switch {
	case grep == "":
	case strings.HasPrefix(grep, "^"):
		match = "starts_with(message, " + sqlString(strings.TrimPrefix(grep, "^")) + ")"
	default:
		match = "contains(message, " + sqlString(grep) + ")"
	}
	// a retried upload repeats lines: each once
	sel := `WITH l AS (
  SELECT DISTINCT time, namespace, ` + who + ` AS pod, pod_uid, ` + part + ` AS container, restart, stream, ` + message + ` AS message
  FROM w_logs WHERE ` + strings.Join(where, " AND ") + `)`
	if previous {
		sel += `,
p AS (SELECT l.* FROM l JOIN (SELECT container, max(restart) - 1 AS r FROM l GROUP BY container) m
  ON l.container IS NOT DISTINCT FROM m.container AND l.restart = m.r)`
	} else {
		sel += `,
p AS (SELECT * FROM l)`
	}
	if match != "" {
		sel += `,
g AS (SELECT * FROM p WHERE ` + match + `)`
	} else {
		sel += `,
g AS (SELECT * FROM p)`
	}
	last := sel + `,
t AS (SELECT * FROM g ORDER BY time DESC, container, restart, message`
	if tail > 0 {
		last += " LIMIT " + strconv.Itoa(tail)
	}
	last += ")\n"
	var q string
	// the line format is for reading; --out keeps the typed columns
	if c.format == "text" && c.of.out == "" {
		var parts []string
		if prefix {
			parts = append(parts, "'["+who+"/' || "+sqlString(pod)+" || '/' || coalesce(container, '') || ']'")
		}
		if timestamps {
			parts = append(parts, "strftime(time, '%Y-%m-%dT%H:%M:%S.') || lpad(CAST(epoch_ns(time) % 1000000000 AS VARCHAR), 9, '0') || 'Z'")
		}
		parts = append(parts, "message")
		q = last + "SELECT concat_ws(' ', " + strings.Join(parts, ", ") + ") AS line FROM t ORDER BY time, container, restart, message"
	} else {
		q = last + "SELECT time, namespace, pod, pod_uid, container, restart, stream, message FROM t ORDER BY time, container, restart, message"
	}
	h := hook{
		cut: func(ctx context.Context, s *session, printed int) string {
			if tail == 0 || printed < tail {
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
			return fmt.Sprintf("%d earlier lines not shown: --tail 0 for every line, or narrow with --grep, --since or a container", total-printed)
		},
		// the namespace's rows: the query, the cut count and the closest
		// pods all come from them
		scope: map[string]string{"logs": "namespace = " + sqlString(ns)},
		empty: func(ctx context.Context, s *session) error {
			return logsMissing(ctx, s, ns, pod, container, who, part, match != "" || uid != "" || restart >= 0 || previous)
		},
	}
	return runInvestigation(ctx, c, args, []string{"logs"}, ns, q, "logs", stdout, stderr, now, h)
}

// logsMissing explains no lines: the pod's containers when the container
// has none, the namespace's closest pods when the pod has none.
func logsMissing(ctx context.Context, s *session, ns, pod, container, who, part string, filtered bool) error {
	base := "FROM w_logs WHERE namespace = " + sqlString(ns)
	if container != "" {
		known, err := s.strings(ctx, "SELECT DISTINCT "+part+" "+base+" AND "+who+" = "+sqlString(pod)+" ORDER BY 1")
		if err == nil && len(known) > 0 && !slices.Contains(known, container) {
			return fail(exitNotFound, "%s %q of %s/%s has no lines in %s; it has: %s", part, container, ns, pod, s.w, strings.Join(known, ", "))
		}
	}
	if filtered {
		return nil
	}
	n, err := s.strings(ctx, "SELECT CAST(count(*) AS VARCHAR) "+base+" AND "+who+" = "+sqlString(pod))
	if err != nil || (len(n) > 0 && n[0] != "0") {
		return nil
	}
	near, err := s.strings(ctx, "SELECT "+who+" FROM (SELECT DISTINCT "+who+" "+base+" AND "+who+" IS NOT NULL) ORDER BY levenshtein("+who+", "+sqlString(pod)+"), "+who+" LIMIT 5")
	if err != nil {
		return nil
	}
	if len(near) == 0 {
		return fail(exitNotFound, "no lines in namespace %s in %s: check the namespace and the window (y-obs pods -n %s --since ...)", ns, s.w, ns)
	}
	return fail(exitNotFound, "no lines of %s/%s in %s; closest with lines: %s", ns, pod, s.w, strings.Join(near, ", "))
}
