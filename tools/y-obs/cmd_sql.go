package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

const sqlUsage = `Usage: y-obs sql [flags] "QUERY"

A DuckDB query over the views of the window (y-obs schema lists them).
Only the views the query names are planned and read; the plan must stay
under --max-objects.

Examples:
  y-obs sql --cluster prod --since 6h "SELECT namespace, pod, count(*) AS n FROM logs GROUP BY ALL ORDER BY n DESC"
  y-obs sql --cluster prod --from '2026-10-11 08:00' --to '2026-10-11 10:00' "SELECT * FROM hourly_pods WHERE exit_reason = 'OOMKilled'"
  y-obs sql --cluster prod --explain "SELECT count(*) FROM daily_logs"
  y-obs sql --cluster prod --since 6h --out inv.duckdb:qa_logs "SELECT * FROM logs WHERE namespace = 'qa'"
  y-obs sql --db inv.duckdb "SELECT pod, count(*) FROM qa_logs GROUP BY 1"

Flags:
`

// sessionCommand is the flag set of a command that reads the bucket.
type sessionCommand struct {
	fs     *flag.FlagSet
	sf     sessionFlags
	of     outFlags
	format string
	limit  int
}

func newSessionCommand(name, usage string, limit int) *sessionCommand {
	c := &sessionCommand{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	c.sf.register(c.fs)
	c.of.register(c.fs)
	c.fs.StringVar(&c.format, "format", "tsv", "output: "+strings.Join(formats, ", "))
	c.fs.IntVar(&c.limit, "limit", limit, "print at most this many rows; 0 for all")
	c.fs.Usage = func() {
		fmt.Fprint(c.fs.Output(), usage)
		c.fs.PrintDefaults()
	}
	return c
}

func (c *sessionCommand) limitSet() bool {
	set := false
	c.fs.Visit(func(f *flag.Flag) { set = set || f.Name == "limit" })
	return set
}

func (c *sessionCommand) parse(args []string) error {
	c.fs.SetOutput(os.Stderr)
	if err := c.fs.Parse(interleave(args)); err != nil {
		if err == flag.ErrHelp {
			return &exitError{code: exitOK}
		}
		return fail(exitUsage, "%v; run y-obs %s -h", err, c.fs.Name())
	}
	if !validFormat(c.format) {
		return fail(exitUsage, "--format %q: use %s", c.format, strings.Join(formats, ", "))
	}
	return nil
}

// interleave moves flags in front of positional arguments, so
// `y-obs sql "SELECT ..." --since 2h` works as well as the flags first.
func interleave(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !isBoolFlag(a) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func isBoolFlag(a string) bool {
	switch strings.TrimLeft(a, "-") {
	case "explain", "h", "help", "append", "timestamps", "prefix", "count", "previous", "failed", "i", "ignore-case", "regex", "v":
		return true
	}
	return false
}

func cmdSQL(ctx context.Context, args []string) error {
	return runSQL(ctx, args, os.Stdout, os.Stderr, time.Now())
}

func runSQL(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) error {
	c := newSessionCommand("sql", sqlUsage, 100)
	if err := c.parse(args); err != nil {
		return err
	}
	if c.fs.NArg() != 1 {
		return fail(exitUsage, "sql takes one query, quoted; run y-obs sql -h")
	}
	query := c.fs.Arg(0)
	limitSet := false
	c.fs.Visit(func(f *flag.Flag) { limitSet = limitSet || f.Name == "limit" })
	var target *outTarget
	if c.of.out != "" {
		t, err := parseOut(c.of.out, "result")
		if err != nil {
			return err
		}
		target = &t
	}
	intoFile := func(f string) bool { return target != nil && target.format == "duckdb" && samePath(target.path, f) }
	var s *session
	if c.of.db != "" {
		var err error
		if s, err = openLocal(ctx, c.of.db, intoFile(c.of.db)); err != nil {
			return err
		}
		defer s.close()
	} else {
		w, err := c.sf.window(now)
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
		if s, err = openSession(ctx, &c.sf, w); err != nil {
			return err
		}
		defer s.close()
		ref := referenced(query)
		if err := s.planRefs(ctx, ref); err != nil {
			return err
		}
		if c.sf.explain {
			s.printPlan(stdout)
			if s.cost() > c.sf.maxObjects {
				return s.overBudget(c.sf.maxObjects, "the plan")
			}
			return nil
		}
		if s.cost() > c.sf.maxObjects {
			return s.overBudget(c.sf.maxObjects, "the plan")
		}
		if err := s.createViews(ctx); err != nil {
			return err
		}
		for _, msg := range s.warnings {
			fmt.Fprintln(stderr, "y-obs: "+msg)
		}
		if miss := s.missingViews(ref); len(miss) > 0 {
			return fail(exitNoRows, "%s: no objects in the window %s; widen --since/--from/--to, or read the raw views for the last hours", strings.Join(miss, ", "), w)
		}
		if c.of.attach != "" {
			if err := s.attachLocal(ctx, c.of.attach, intoFile(c.of.attach)); err != nil {
				return err
			}
			s.local = c.of.attach
		}
	}
	if target != nil {
		q := query
		if limitSet {
			q = limited(query, c.limit)
		}
		n, err := s.writeOut(ctx, *target, q, c.of.append, commandLine("sql", args), stdout)
		if err != nil {
			return err
		}
		s.summary(stderr, int(n), false, 0)
		if n == 0 {
			return &exitError{code: exitNoRows}
		}
		return nil
	}
	rows, err := s.conn.QueryContext(ctx, query)
	if err != nil {
		return s.wrap(err)
	}
	defer rows.Close()
	out := bufio.NewWriter(stdout)
	n, more, err := writeRows(out, rows, c.format, c.limit)
	if ferr := out.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		return s.wrap(err)
	}
	if strings.Contains(query, "s3://") {
		fmt.Fprintln(stderr, "y-obs: the query reads s3:// itself: those reads are neither planned nor counted against --max-objects")
	}
	s.summary(stderr, n, more, c.limit)
	if n == 0 {
		if s.cluster != "" && s.noObjects() {
			return s.explainEmpty(ctx)
		}
		return &exitError{code: exitNoRows}
	}
	return nil
}

// summary is the stderr line that says what was read (README.md, rule 4).
func (s *session) summary(w io.Writer, n int, more bool, limit int) {
	line := fmt.Sprintf("y-obs: cluster=%s window=%s objects %s rows=%d", s.cluster, s.w, s.planSummary(), n)
	if s.cluster == "" {
		line = fmt.Sprintf("y-obs: db=%s rows=%d", s.local, n)
	} else if s.wplan != nil {
		line = fmt.Sprintf("y-obs: cluster=%s window=%s %s objects %s rows=%d", s.cluster, s.w, s.sourcesSummary(), s.planSummary(), n)
	}
	if t, ok := s.freshTo(); ok {
		line += " fresh-to=" + t.Format(time.RFC3339)
	}
	if more {
		line += fmt.Sprintf("; more rows than --limit %d: narrow the query (WHERE, a shorter window) or raise --limit", limit)
	}
	fmt.Fprintln(w, line)
}

// freshTo is when the newest raw object read was opened, from its key: its
// rows reach up to UPLOAD_TIMEOUT (30 s) later. An event after it is not
// in what was read.
func (s *session) freshTo() (time.Time, bool) {
	var newest time.Time
	for _, f := range s.plan["raw"] {
		m := rawKeyTime.FindStringSubmatch(f)
		if m == nil {
			continue
		}
		if t, err := time.Parse("2006-01-02 15:04:05", m[1]+" "+m[2]+":"+m[3]+":"+m[4]); err == nil && t.After(newest) {
			newest = t
		}
	}
	return newest, !newest.IsZero()
}

func (s *session) printPlan(w io.Writer) error {
	fmt.Fprintf(w, "cluster %s, window %s, %d objects, cost %d (a raw object counts a tenth)\n", s.cluster, s.w, s.planned(), s.cost())
	for _, v := range s.planViews() {
		fs := s.plan[v]
		fmt.Fprintf(w, "%s\t%d objects\n", v, len(fs))
		show := fs
		if len(show) > 6 {
			show = append(append([]string{}, fs[:3]...), append([]string{fmt.Sprintf("... %d more ...", len(fs)-6)}, fs[len(fs)-3:]...)...)
		}
		for _, f := range show {
			fmt.Fprintf(w, "  %s\n", f)
		}
	}
	return nil
}

// The views schema describes, in the order an investigation reads them.
var viewDocs = []struct{ name, doc string }{
	{"logs", "log lines: containers' (namespace, pod, pod_uid, container, restart) and the node's journal (namespace _node, pod NULL, unit)"},
	{"metrics", "one row per scraped sample: name, type, labels (MAP), value; job pod or node"},
	{"podindex", "pod and container events: pod-observed, pod-checkpoint, pod-deleted, container, container-exit"},
	{"pods", "one row per (pod_uid, container, restart) seen in logs, metrics or podindex, with first/last log, exit and limits"},
	{"apiserver", "the cluster agent's rows: object-added/-modified/-deleted/-snapshot and snapshot markers, apijson the object"},
	{"apiserver_at(t)", "macro: the cluster's objects as they were at t (local/apiserver_at.sql)"},
	{"raw", "every line of the window's raw objects, columns pinned (compactor/sql/raw.sql)"},
	{"hourly_<table>", "the hourly tier of logs, metrics, podindex, pods, apiserver: deduplicated, sorted, complete hours"},
	{"daily_<table>", "the daily tier of the same tables; logs and metrics partitioned by namespace"},
}

const schemaUsage = `Usage: y-obs schema [VIEW]

Without VIEW: the views every query can name. With VIEW: its columns,
types and meanings. Reads nothing from the bucket.

Examples:
  y-obs schema
  y-obs schema logs
  y-obs schema hourly_pods
`

func cmdSchema(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), schemaUsage) }
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return &exitError{code: exitOK}
		}
		return fail(exitUsage, "%v", err)
	}
	if fs.NArg() == 0 {
		for _, v := range viewDocs {
			fmt.Printf("%-16s %s\n", v.name, v.doc)
		}
		return nil
	}
	view := fs.Arg(0)
	base, note := view, ""
	for _, tier := range []string{"hourly_", "daily_"} {
		if strings.HasPrefix(view, tier) {
			base = strings.TrimPrefix(view, tier)
			note = fmt.Sprintf("%s: the columns of %s as the compactor writes them, deduplicated; without filename, which is the tier object's", view, base)
		}
	}
	known := false
	for _, v := range rawViews {
		known = known || v == base
	}
	if !known || base == "apiserver_at" {
		return fail(exitNotFound, "no view %q; y-obs schema lists them", view)
	}
	dir, err := os.MkdirTemp("", "y-obs-schema-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sf := &sessionFlags{cluster: "schema", blobsDir: dir}
	now := time.Now()
	s, err := openSession(ctx, sf, window{from: now.Add(-time.Hour), to: now})
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.prepare(ctx, map[string]bool{"raw": true}, 1); err != nil {
		return err
	}
	rows, err := s.conn.QueryContext(ctx, "SELECT column_name, column_type FROM (DESCRIBE "+base+")")
	if err != nil {
		return s.wrap(err)
	}
	defer rows.Close()
	comments := rawComments()
	if note != "" {
		fmt.Println("# " + note)
	}
	fmt.Println("column\ttype\tmeaning")
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return err
		}
		if note != "" && name == "filename" {
			continue
		}
		meaning := comments[name]
		if d, ok := viewColumnDocs[base][name]; ok {
			meaning = d
		} else if d, ok := viewColumnDocs["*"][name]; ok {
			meaning = d
		}
		fmt.Printf("%s\t%s\t%s\n", name, typ, meaning)
	}
	return rows.Err()
}

// viewColumnDocs are the meanings of columns a view derives, where
// raw.sql's comment on the raw column would mislead (tables.sql, pods.sql).
// "*" applies to every view.
var viewColumnDocs = map[string]map[string]string{
	"*": {
		"kind":      "podindex: pod-observed | pod-checkpoint | pod-deleted | container | container-exit; apiserver: object-added | object-modified | object-deleted | object-snapshot | snapshot",
		"namespace": "the pod's namespace; _node for rows of the node itself (journal, node scrapes)",
		"cluster":   "the cluster, as its agent names it",
		"node":      "the node whose agent shipped the row",
	},
	"logs": {
		"time": "the CRI's nanosecond stamp; for journal rows the agent's microsecond ts",
		"pod":  "NULL for the node's journal rows",
	},
	"metrics": {
		"time":   "scrape start",
		"name":   "the metric name as exposed",
		"type":   "counter | gauge | histogram | summary | untyped",
		"labels": "the sample's labels, a MAP: labels['code']",
		"value":  "the sample value",
	},
	"apiserver": {
		"api_group": "the API group, '' for the core group; a resource is (api_group, resource) whatever version",
	},
	"pods": {
		"workload":          "derived from labels and the name's shape: Deployment, StatefulSet, Job, CronJob or Tekton name",
		"owner_kind":        "TaskRun | CronJob | Job | StatefulSet | Deployment | DaemonSet | Pod, from labels",
		"phase":             "running (no deletion seen) | failed | succeeded | unknown (deleted, no exit seen)",
		"restart":           "the kubelet restart count of this row's run",
		"last_seen":         "the pod's latest podindex row",
		"first_log":         "this run's first log line",
		"last_log":          "this run's last log line",
		"lines":             "this run's log lines",
		"bytes":             "this run's log bytes",
		"container_exited":  "this run's exit, from container-exit",
		"exit_code":         "this run's exit code",
		"exit_reason":       "Completed | Error | OOMKilled | ...",
		"exit_message":      "the termination message",
		"pod_deleted":       "when the pod's deletion was seen; NULL when not (yet) seen",
		"metric_names":      "the metric names scraped from this run",
		"container_started": "this run's start",
	},
}

var rawColumn = regexp.MustCompile(`^\s+"?(\w+)"?:'[^']*',?\s*(?:--\s*(.*))?$`)
var rawCommentLine = regexp.MustCompile(`^\s+--\s*(.*)$`)

// rawComments are raw.sql's column comments, a column's continuation
// lines joined: the contract's own words for what each column holds.
func rawComments() map[string]string {
	b, _ := sqlFS.ReadFile("sql/raw.sql")
	m := map[string]string{}
	last := ""
	inColumns := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "columns={") {
			inColumns = true
			continue
		}
		if !inColumns {
			continue
		}
		if c := rawColumn.FindStringSubmatch(line); c != nil {
			last = c[1]
			m[last] = strings.TrimSpace(c[2])
			continue
		}
		if c := rawCommentLine.FindStringSubmatch(line); c != nil && last != "" && m[last] != "" {
			m[last] += " " + strings.TrimSpace(c[1])
			continue
		}
		last = ""
	}
	m["filename"] = "the object the row came from"
	return m
}
