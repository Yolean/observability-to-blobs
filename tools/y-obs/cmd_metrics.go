package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const metricsUsage = `Usage: y-obs metrics SUBCOMMAND [NAME] [flags]

Every scraped sample is in the bucket: no allowlist, and history is the
bucket's retention.

  names           metric names: type, series, samples, last sample
  series NAME     the last value of each series per --step (default 1m)
  rate NAME       per-second increase per --step (default 5m), counter
                  resets handled as PromQL's rate() does
  latest NAME     the newest sample of each series

A series is one target (pod_uid, container, instance) and label set.
--where key=value filters on labels (or namespace, pod, container, node,
scrape, instance, k8s_deployment_name and the other workload names);
--by key,... sums (--agg) across the series that share those keys.

Examples:
  y-obs metrics names -n qa --pod '^web-'
  y-obs metrics series container_memory_working_set_bytes --pod '^web-1$' --since 6h --step 1m
  y-obs metrics rate http_requests_total -n qa --by code --since 1h --step 5m
  y-obs metrics latest up --where scrape=pod --value 0
  y-obs metrics rate http_requests_total -n qa --by k8s_deployment_name --since 1h
  y-obs metrics series node_load1 --scrape node --since 7d --step 1h --out inv.duckdb:load

Flags:
`

func cmdMetrics(ctx context.Context, args []string) error {
	return runMetrics(ctx, args, os.Stdout, os.Stderr, time.Now())
}

// seriesCols identify a series; byColumns are the --by keys that are
// columns rather than labels.
var seriesCols = []string{"namespace", "pod", "pod_uid", "container", "instance", "labels"}
var byColumns = map[string]bool{"namespace": true, "pod": true, "pod_uid": true, "container": true, "node": true, "scrape": true, "instance": true,
	"k8s_deployment_name": true, "k8s_replicaset_name": true, "k8s_statefulset_name": true, "k8s_daemonset_name": true,
	"k8s_job_name": true, "k8s_cronjob_name": true}

var labelKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_./-]*$`)

// keyExpr is a --where or --by key as SQL: a column, or the label.
func keyExpr(k string) (string, error) {
	if byColumns[k] {
		return k, nil
	}
	if !labelKey.MatchString(k) {
		return "", fail(exitUsage, "%q is not a label name", k)
	}
	return "labels[" + sqlString(k) + "]", nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func runMetrics(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) error {
	// the subcommand is the first argument that is not a flag or a flag's
	// value, wherever it stands: metrics rate X --since 1h, or
	// metrics --since 1h rate X
	sub, at := "", -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprint(stdout, metricsUsage)
			return nil
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			if !strings.Contains(a, "=") && !isBoolFlag(a) {
				i++
			}
			continue
		}
		sub, at = a, i
		break
	}
	if at < 0 {
		return fail(exitUsage, "metrics takes a subcommand: names, series, rate or latest; run y-obs metrics -h")
	}
	rest := append(append([]string{}, args[:at]...), args[at+1:]...)
	defaults := map[string]int{"names": 200, "series": 500, "rate": 500, "latest": 100}
	limit, ok := defaults[sub]
	if !ok {
		return fail(exitUsage, "metrics %s: use names, series, rate or latest", sub)
	}
	c := newSessionCommand("metrics", metricsUsage, limit)
	var ns, pod, container, scrape, match, by, agg, stepFlag, value string
	var where stringList
	c.fs.StringVar(&ns, "namespace", "", "only this namespace")
	c.fs.StringVar(&ns, "n", "", "short for --namespace")
	c.fs.StringVar(&pod, "pod", "", "pod names matching this RE2 pattern")
	c.fs.StringVar(&container, "container", "", "only this container")
	c.fs.StringVar(&scrape, "scrape", "", "pod, node or host: application targets, the node's own, a host agent's")
	c.fs.Var(&where, "where", "key=value a series must have (a label, or namespace/pod/container/node/scrape/instance/k8s_*_name); repeatable")
	c.fs.StringVar(&match, "match", "", "names: metric names matching this RE2 pattern")
	c.fs.StringVar(&by, "by", "", "series, rate: aggregate across series by these comma-separated keys")
	c.fs.StringVar(&agg, "agg", "sum", "series, rate with --by: sum, avg, max or min")
	c.fs.StringVar(&stepFlag, "step", "", "series, rate: the bucket (default 1m for series, 5m for rate)")
	c.fs.StringVar(&value, "value", "", "latest: only series whose latest value is this number")
	if err := c.parse(rest); err != nil {
		return err
	}
	name := ""
	switch {
	case sub == "names" && c.fs.NArg() == 0:
	case sub != "names" && c.fs.NArg() == 1:
		name = c.fs.Arg(0)
	case sub == "names":
		return fail(exitUsage, "metrics names takes no NAME; filter with --match")
	default:
		return fail(exitUsage, "metrics %s takes one metric NAME; y-obs metrics names lists them", sub)
	}
	conds := []string{}
	if name != "" {
		conds = append(conds, "name = "+sqlString(name))
	}
	if ns != "" {
		conds = append(conds, "namespace = "+sqlString(ns))
	}
	for _, re := range []struct{ flag, col, v string }{{"--pod", "pod", pod}, {"--match", "name", match}} {
		if re.v == "" {
			continue
		}
		if _, err := regexp.Compile(re.v); err != nil {
			return fail(exitUsage, "%s %q: %v", re.flag, re.v, err)
		}
		conds = append(conds, "regexp_matches("+re.col+", "+sqlString(re.v)+")")
	}
	if container != "" {
		conds = append(conds, "container = "+sqlString(container))
	}
	if scrape != "" {
		conds = append(conds, "scrape = "+sqlString(scrape))
	}
	if c.sf.node != "" {
		conds = append(conds, "node = "+sqlString(c.sf.node))
	}
	for _, w := range where {
		k, v, ok := strings.Cut(w, "=")
		if !ok {
			return fail(exitUsage, "--where %q: use key=value", w)
		}
		e, err := keyExpr(k)
		if err != nil {
			return err
		}
		conds = append(conds, e+" = "+sqlString(v))
	}
	filter := ""
	if len(conds) > 0 {
		filter = " WHERE " + strings.Join(conds, " AND ")
	}
	var byKeys, byExprs []string
	if by != "" {
		for _, k := range strings.Split(by, ",") {
			k = strings.TrimSpace(k)
			e, err := keyExpr(k)
			if err != nil {
				return err
			}
			byKeys = append(byKeys, quoteIdent(k))
			byExprs = append(byExprs, e+" AS "+quoteIdent(k))
		}
	}
	switch agg {
	case "sum", "avg", "max", "min":
	default:
		return fail(exitUsage, "--agg %q: sum, avg, max or min", agg)
	}
	step := map[string]time.Duration{"series": time.Minute, "rate": 5 * time.Minute}[sub]
	if stepFlag != "" {
		d, err := parseSince(stepFlag)
		if err != nil || d < time.Second {
			return fail(exitUsage, "--step %q: a duration of at least 1s, like 30s, 5m or 1h", stepFlag)
		}
		step = d
	}
	interval := fmt.Sprintf("INTERVAL %d SECOND", int64(step/time.Second))
	series := strings.Join(seriesCols, ", ")
	var q string
	switch sub {
	case "names":
		q = `SELECT name, any_value(type) AS type, count(DISTINCT hash(pod_uid, container, instance, labels)) AS series,
  count(*) AS samples, max(time) AS last
FROM w_metrics` + filter + ` GROUP BY name ORDER BY name`
	case "latest":
		q = `SELECT max(time) AS time, ` + series + `, arg_max(value, time) AS value
FROM w_metrics` + filter + ` GROUP BY ` + series
		if value != "" {
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				return fail(exitUsage, "--value %q is not a number", value)
			}
			q = "SELECT * FROM (" + q + ") WHERE value = " + value
		}
		q += " ORDER BY namespace, pod, container, instance"
	case "series":
		inner := `SELECT time_bucket(` + interval + `, time) AS time, ` + series + `, arg_max(value, time) AS value
FROM w_metrics` + filter + ` GROUP BY ALL`
		if len(byKeys) == 0 {
			q = inner + " ORDER BY namespace, pod, container, instance, time"
		} else {
			q = `SELECT time, ` + strings.Join(byExprs, ", ") + `, ` + agg + `(value) AS value FROM (` + inner + `)
GROUP BY ALL ORDER BY ` + strings.Join(byKeys, ", ") + `, time`
		}
	case "rate":
		// per series: the increase between consecutive samples, a drop
		// being a reset that restarted from zero; per step the increases
		// summed and divided by the step
		d := `WITH s AS (SELECT time, ` + series + `, value FROM w_metrics` + filter + `),
d AS (SELECT *, value - lag(value) OVER w AS dv, lag(value) OVER w AS pv
  FROM s WINDOW w AS (PARTITION BY pod_uid, container, instance, labels ORDER BY time)),
inc AS (SELECT time_bucket(` + interval + `, time) AS time, ` + series + `,
  sum(CASE WHEN dv < 0 THEN value ELSE dv END) AS increase
  FROM d WHERE pv IS NOT NULL GROUP BY ALL)`
		secs := strconv.FormatInt(int64(step/time.Second), 10)
		if len(byKeys) == 0 {
			q = d + `
SELECT time, ` + series + `, increase / ` + secs + ` AS rate FROM inc ORDER BY namespace, pod, container, instance, time`
		} else {
			q = d + `
SELECT time, ` + strings.Join(byExprs, ", ") + `, ` + agg + `(increase / ` + secs + `) AS rate FROM inc
GROUP BY ALL ORDER BY ` + strings.Join(byKeys, ", ") + `, time`
		}
	}
	h := hook{empty: func(ctx context.Context, s *session) error {
		if name == "" {
			return nil
		}
		// one read: the name itself ranks first when it is there
		near, err := s.strings(ctx, "SELECT name FROM (SELECT DISTINCT name FROM w_metrics) ORDER BY name = "+sqlString(name)+" DESC, jaro_winkler_similarity(name, "+sqlString(name)+") DESC LIMIT 5")
		if err != nil || len(near) > 0 && near[0] == name {
			return nil
		}
		if len(near) == 0 {
			return fail(exitNotFound, "no samples of any metric in %s", s.w)
		}
		return fail(exitNotFound, "no metric %s in %s; closest names: %s", name, s.w, strings.Join(near, ", "))
	}}
	return runInvestigation(ctx, c, args, []string{"metrics"}, ns, q, "metrics_"+sub, stdout, stderr, now, h)
}
