---
name: observability
description: Investigate a Kubernetes cluster from its observability bucket with y-obs - which pods ran and when, why a container died (exit code, OOMKilled, eviction), a pod's logs, a log pattern across pods and days, any scraped metric with full history, and kubectl against the cluster as it was at a past time. Use when asked why something failed, to read logs, to find pods, for OOM/crash/eviction postmortems, for metric history, or "what did the cluster look like at T". Works for deleted pods and past days; needs no cluster access.
---

# Observability from the bucket (y-obs)

> Draft, paired with `tools/y-obs/README.md`. A consuming repository
> copies this file and adds its contexts under "Contexts".

Everything comes from one bucket that the cluster's agents write to about
every 30 seconds per node. Nothing queries the cluster itself, so
deleted pods, finished jobs and past days work the same as live ones.

## Start with status

`y-obs status` says what you can reach and what is there: the contexts
and their credentials (with expiry), running Prometheus instances, and
per cluster the span, volume and newest upload of each tier. Read it
first when you do not know the cluster's name, or whether its data
reaches back to the time you are asked about.

## Pick the command by what you know

| You know | Start with | Then |
|---|---|---|
| namespace + pod | `y-obs logs NS POD` | `--grep`, `--since`, a container |
| part of a pod name, a label, a node | `y-obs pods` | `logs` with the pod and `--uid` |
| that something died or restarted | `y-obs exits` | `logs ... --previous` |
| only a log pattern | `y-obs grep PATTERN -n NS --count` | drop `--count` for the lines |
| a metric name | `y-obs metrics series/rate/latest NAME` | `metrics names` if unsure |
| a PromQL expression, or a human who wants the Prometheus UI | `U=$(y-obs prometheus --no-open)` | `curl "$U/api/v1/query" --data-urlencode query=...` |
| "what was deployed / configured at T" | `y-obs kubectl --at T -- get ...` | `describe` |
| none of the above | `y-obs schema`, then `y-obs sql` | |

Always pass a window (`--since 2h`, or `--from`/`--to` in UTC) that
matches the question. The default is the last hour, which is wrong for
anything older.

## Reading the output

- stderr ends with a summary line:
  `y-obs: cluster=... window=... hours raw=1 hourly=5 objects ... rows=N fresh-to=...`.
  Read it before you conclude anything from an empty result.
  - It names the window actually read and the tier of each hour.
  - `fresh-to` is when the newest raw object read was opened; its rows
    reach about 30 s later. An event after it is not in what was read
    yet: ask again in a minute.
  - Exit code 1 means the window was read and nothing matched. That is
    evidence of absence for that window only.
- A cut result says so on stderr, before the summary, with how to narrow
  it. Narrow first: `--grep`, a container, a shorter window. Raise
  `--limit` only after.
- Exit 5 means the plan was too broad (the cost guard). The message says
  what shrinks it: `--node` for the raw tier (the last hours), `-n` when
  the daily tier is read, or a shorter window. `--explain` shows the plan
  without reading.
- Exit 3 means credentials: run `y-obs auth`. Credentials last minutes,
  and y-obs renews them itself, so this only happens when the login
  behind them has expired.
- Exit 4 means not in the bucket, and the message names what is: the
  pod's containers, the closest pod or metric names, or, when the window
  has no data at all, the clusters with data or the cluster's dates.
  Move the window or fix the name it points at.
- Exit 2 is usage, a SQL syntax error included: `y-obs <command> -h` and
  `y-obs schema` show what is accepted.

## Commands

```bash
y-obs pods -n qa --name '^web-' --since 6h            # incarnations: pod_uid, node, workload, started, ended, phase, restarts, last_exit
y-obs pods --label app=gateway --at '2026-10-11 09:00' # alive at an instant
y-obs exits -n qa --since 1d                           # exit_code, exit_reason, exit_message, limits in force
y-obs exits --reason OOMKilled --since 7d
y-obs logs qa web-1 --tail 200 --timestamps            # all containers merged by time
y-obs logs qa web-1 app --grep error --since 3h
y-obs logs qa web-1 --uid <pod_uid> --previous         # the run before the last restart
y-obs logs _node worker-3 kubelet.service --grep error # the node's journal, one unit
y-obs grep 'connection reset' -n qa --since 2h --count # matches per pod, cheap
y-obs metrics names -n qa --pod '^web-'
y-obs metrics series container_memory_working_set_bytes --pod '^web-1$' --since 6h --step 1m
y-obs metrics rate http_requests_total -n qa --by code --since 1h --step 5m
y-obs prometheus                                      # Prometheus UI over the bucket: the last hour loaded, older on query
y-obs prometheus --stop                               # ends it; a repeated call reuses it
y-obs kubectl --at '2026-10-11 09:00' -- get deploy -n qa -o yaml
y-obs sql "SELECT ..." --since 1h                     # views: y-obs schema
```

`--format json`, `jsonl` or `csv` gives machine-readable rows. Default
output is plain text for `logs` and `grep`, and TSV with a header for
everything else. `y-obs <command> -h` has every flag and examples.

## Keep results in a local DuckDB file

When you will ask more than two questions of the same data, fetch it
once and query the file. That is faster and reads nothing more from the
bucket.

```bash
DB=$SCRATCH/inv.duckdb                                  # a scratch dir, never the repo
y-obs pods -n qa --since 1d --out $DB                   # table pods
y-obs logs qa web-1 --since 6h --tail 0 --out $DB:web1 # table web1, every line
y-obs sql --db $DB "SELECT * FROM web1 WHERE message ILIKE '%timeout%' ORDER BY time"
y-obs sql --db $DB "SELECT * FROM y_obs_runs"           # what each table covers
```

- `--out` prints the table's columns and a next-step query instead of
  rows.
- `--append` adds to a table rather than replacing it.
- `--attach $DB` instead of `--db $DB` keeps the bucket's views and adds
  the file as `local`: `... --attach $DB "SELECT * FROM local.web1 JOIN pods USING (pod_uid)"`.
- The file holds log lines. Delete it when the investigation is done.

## Investigation patterns

**A pod failed.** Run `exits --uid` (or `-n` and `--name`) first: it gives
the exit code, the reason and the limits. Then `logs --uid ... --tail 200`
around the exit time.

- `OOMKilled` is the container's cgroup limit.
- Exit 137 with reason `Error` is a SIGKILL: either the kill after a
  deletion's grace period, or node pressure. Compare the exit time with
  the pod's `ended` in `pods`. An exit right at the end points to a
  deletion. The bucket has no eviction reason column yet, so say "one of
  the two" when that comparison doesn't settle it.

**An OOM postmortem.** `metrics series container_memory_working_set_bytes`
over the pod's whole lifetime separates a ramp (leak) from a plateau that
spikes (an undersized limit). Every sample is kept, so this works days
later too, provided the cluster scrapes cAdvisor: check with
`metrics names --match '^container_memory'`, since exit 4 only says the
name is absent.

**A timeout or error spike.** Start with `metrics rate` on the relevant
counter, `--by` the label that matters. Counters beat quantiles at low
volume. Then `grep --count` over the window to find which pods logged
it, then `logs` on one of them.

**Something changed.** `kubectl --at` before and after, then diff the
two outputs. A resource with no complete snapshot by T is missing from
the answer, which then means unknown, not empty. A time before the
agent's first snapshot is exit 1.

**PromQL.** `y-obs prometheus` runs a real Prometheus over the bucket,
so any PromQL works, recording-rule style expressions included. It
prints its URL, and its HTTP API takes the usual `/api/v1/query` and
`/api/v1/query_range`. Ranges older than the loaded window (default the
last hour) are read from the bucket as the query needs them. A warning
in the API's answer means y-obs refused part of the range as too broad:
add a namespace or narrow the range. Stop it with `--stop` when done.

## Traps

- **Identity is `pod_uid`, not the name.** StatefulSets and retries reuse
  names. Pin `--uid` once `pods` has shown it.
- **A pod with no `ended` is not necessarily running.** The deletion may
  not have been seen. `pods` applies the gap rule, but an alive-at-T
  answer near the edge of the data is a lower bound.
- **Times are UTC.** Every input and output is UTC; convert local
  times first.
- **Journal lines live under namespace `_node`**, with the node as the
  pod.
- **Secret data is not in the bucket.** `kubectl --at` shows Secrets
  without data, by design.
- **Raw is the only tier for the current hour.** The hourly tier arrives
  minutes after the hour, the daily tier the next day. y-obs picks the
  tier; the summary line says which one it read.

## Contexts

`y-obs contexts` lists the configured clusters. Pass `--context=NAME`
on every call when there is more than one.

<!-- consuming repository: list its contexts and their clusters here -->
