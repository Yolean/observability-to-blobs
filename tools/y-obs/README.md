# y-obs: the bucket as the only observability backend (draft)

> Draft for review. Open decisions are marked **Decide**; the rest is a
> proposal to change freely. Built so far:
> - `apiserver-at`;
> - `sql` and `schema`, on the bucket or `--blobs-dir`;
> - `--out`, `--db` and `--attach` on `sql`;
> - `kubectl --at`, in-process: no duckdb CLI, temp files or go needed
>   when `setup-envtest` or `KUBEBUILDER_ASSETS` provides the release;
> - S3 access through the AWS SDK's default credential chain
>   (`credential_process` included), with renewal, and contexts that name
>   a bucket, a cluster and an AWS profile;
> - the window planner (each hour from one tier), `pods`, `exits`, `logs`, `grep` and `metrics`;
> - release builds with httpfs embedded (`release.sh`, `.github/workflows/release.yaml`), and `version`;
> - `prometheus`: a local Prometheus UI over the bucket;
> - `status`: contexts, credentials, instances, and each cluster's tiers.
> The rest is design.

`y-obs` is one local executable that answers the questions people (and
LLM agents) ask during an investigation: which pods ran, why one died,
what it logged, what a metric did, what the cluster's objects looked like.
It reads only the bucket: the raw tier for the last hour or two, and the
hourly and daily tiers before that. No logs API, no Prometheus, no
kubectl access to the cluster.

Two things make this possible now:

- **Freshness.** The agent flushes a raw object about every 30 s per node,
  so the raw tier lags production by under a minute. An archive that
  buffers until megabytes or an hour have gathered does not.
- **One engine.** The views the compactor and the itest use
  (`compactor/sql/raw.sql`, `tables.sql`, `pods.sql`,
  `local/apiserver_at.sql`) already define every table. `y-obs` embeds
  DuckDB and reads the same files, so a query means the same thing
  everywhere.

## What it replaces

| today | y-obs |
|---|---|
| a logs API per cluster, and a CLI for it | `y-obs logs` |
| a pod search page, and a pod index on a separate datalake | `y-obs pods`, `y-obs exits` |
| pyarrow scripts for searching log content across pods | `y-obs grep` |
| Prometheus (minutes of history), a long-term store (allowlisted metrics) | `y-obs metrics` over every scraped sample |
| `local/blobs.sh`, `local/kubectl-at.sh` (need go, duckdb, kubectl and a checkout) | `y-obs sql`, `y-obs kubectl` |

## Built from tools/apiserver-at

`tools/apiserver-at` becomes `tools/y-obs`:

- Its server becomes the `kubectl` subcommand, and stays as
  `apiserver-at` for test.sh and `local/kubectl-at.sh`. kube-apiserver
  and etcd of the cluster's release come from setup-envtest, as before.
- It embeds DuckDB with `github.com/duckdb/duckdb-go/v2`, the version the
  compactor pins, so the SQL behaves the same as the compactor's.
- It embeds the `.sql` view files with `go:embed`, so the binary needs no
  checkout.
- It embeds the httpfs extension, sha256-pinned per platform as the
  compactor's `build.env` does. On first use the binary writes it to the
  cache directory and `LOAD`s it by path, with autoinstall off.
- It lists the bucket by DuckDB's glob, and DuckDB reads the objects.
  For the delimiter listings that say what a bucket holds (the clusters,
  and a cluster's dates per tier, for exit 4), it has a third
  byte-identical copy of the compactor's SigV4 signer, as
  `images/blobs-to-prometheus` has (`go generate` and
  `TestSigV4Copied`).
- **The engine is bounded.** Memory is capped at 2 GB
  (`Y_OBS_MEMORY_LIMIT`), spilling to a temporary directory. Threads
  follow the limit, 8 per GB and at most the CPU count, so 16 by default
  (`Y_OBS_THREADS` overrides): each DuckDB thread reserves its own
  buffers. Under 2 GB, `pods` on 30 minutes ran out of memory at 32
  threads and not at 16, while 8 made one- and two-day windows 3.5x
  slower against a remote store. Running out of memory is exit 5, like
  a plan over budget. Each command reads its window once,
  into a temporary table of the rows it scopes to, which its query and
  its follow-up questions (a cut count, the closest names) then read.

**Release.** A tag `v*` of the repository runs
`.github/workflows/release.yaml`, which releases y-obs with the host
agent's tarballs on that tag's release; there are no y-obs tags of its
own. It builds four binaries, linux/amd64, linux/arm64, darwin/arm64 and
darwin/amd64, each on a native runner: duckdb-go links prebuilt static
libraries per platform through cgo, so there is no cross compiling.

- **Building.** `tools/y-obs/release.sh` is the one build step.
  - It downloads DuckDB's signed httpfs extension for the platform and
    checks it against `release.env`, whose linux checksums equal
    `compactor/build.env`'s.
  - It embeds the extension with the `httpfs_embed` build tag. At run
    time the binary writes it once to the user cache, under its
    checksum, and `LOAD`s it by path: no extension download, and no
    `~/.duckdb`.
- **Smoke test.** The workflow runs `version`, `schema` and a query on
  an empty blobs directory.
- **Publishing.** It attaches the binaries with their sha256 files to
  the tag's release as `obs-{linux,darwin}-{amd64,arm64}`. The asset is
  `obs`: ystack's `y-bin` download entry pins the checksums and installs
  it with its own `y-` prefix, as for its other tools.
- **Development builds.** Without the tag, httpfs is installed from
  DuckDB's signed repository at first use. `TestReleaseEnv` fails when
  `release.env` pins another engine version than the one linked.

## Design rules for LLM callers

An agent calls `y-obs` many times per investigation, reads every byte of
output into its context, and cannot see a progress bar. So:

1. **Bounded by default.** Every command has a time window (default: the
   last hour) and a bound:
   - `pods` and `exits`: 50 rows;
   - `logs --tail` and `grep`: 100 lines;
   - `metrics names`: 200, `series` and `rate`: 500, `latest`: 100;
   - `sql`: 100.

   When output is cut, stderr says so and how to narrow:
   `y-obs: 412 earlier lines not shown: --tail 0 for every line, or narrow with --grep, --since or a container`.
2. **Cost guard.** Before reading anything, `y-obs` plans the objects
   each view reads.
   - Raw keys are cut to the window by when the agent opened them, from
     2 minutes before it to 5 minutes past it, the margin after taking
     lines that arrived late.
   - A tier object costs 1 and a raw object a tenth: raw objects are
     small and come one per node per 30 s, so the default `--max-objects`
     of 200 fits an hour of raw on about 15 nodes.
   - A plan over budget is exit 5 with its counts and what would shrink
     it: `--node` for raw, `-n` for the daily tier, a shorter window.
   - `--explain` prints the plan (the hours per tier, the objects per
     view and the cost) without reading, and exits 5 after printing when
     the plan is over budget.
3. **Stable, parseable output.** `--format text|tsv|csv|json|jsonl`.
   text is the default for `logs` and `grep`, tsv for everything else.
   Columns come in a fixed order with a header line. Timestamps are
   always UTC RFC 3339 with nine fractional digits. A MAP (labels) or a
   list (`pods.containers`) is JSON in a cell. Floats are plain decimals
   to 12 significant digits in the text formats (`0.00433333333333`);
   JSON keeps every digit. There is no colour, and none of the output depends on
   terminal width.
4. **Says where the data came from.** On exit, stderr gets one summary
   line:
   `y-obs: cluster=prod window=2026-10-11T08:00:00Z..2026-10-11T10:00:00Z hours raw=1 hourly=1 objects hourly_logs=1 raw=40 rows=12 fresh-to=2026-10-11T09:59:30Z`.
   `fresh-to` is when the newest raw object read was opened, from its
   key; its rows reach up to 30 s later. An event after it is not in
   what was read. Notes, such as how many lines a cut left out or that
   there are more rows than `--limit` (with what narrows that command's
   rows), come before the summary, which is always the last line.
5. **Errors are one line with a next step**, plus documented exit codes:
   - 0: ok, results printed;
   - 1: ok, nothing matched;
   - 2: usage, a SQL syntax error or unknown column included;
   - 3: credentials (none in the AWS chain, a `credential_process` that
     fails or refuses, or the store refusing the key);
   - 4: not in the bucket, with what is: the pod's containers, the
     closest pod or metric names, DuckDB's "did you mean" for a view;
     and when the window has no object at all, the clusters with data,
     or the cluster's dates per tier;
   - 5: the plan exceeds the budget, or the engine its memory;
   - 6: the store or the engine failed.
6. **Self-describing.**
   - `y-obs <command> -h` (or `y-obs help <command>`) prints usage with
     examples.
   - `y-obs schema [view]` prints columns, types and one-line meanings,
     from `raw.sql`'s comments or the view's own.
   - `y-obs help` lists the commands in the order an investigation uses
     them, with the exit codes.
7. **Identity is the pod uid.** Names are reused by StatefulSets and
   retries, so every row that names a pod also carries `pod_uid`.
   `pods`, `exits` and `logs` take `--uid` to pin one incarnation.
8. **Results can stay local.** Every command that prints rows takes
   `--out FILE.duckdb` and writes them into a table there instead. Later
   questions then run against that file, with no credentials, no budget
   and no bucket reads (below, "Output to a DuckDB file").

## Commands

```
y-obs <command> [flags]

  status        what y-obs can reach and what is there
  pods          which pods ran: one row per incarnation
  exits         why containers ended: exit code, reason, the limits in force
  logs          one pod's lines (all containers merged, or one), or a node's journal
  grep          a pattern across pods and hours, scoped by namespace or name
  metrics       names, series, rate, latest: every scraped sample, no allowlist
  sql           a DuckDB query over the views, with the same budget guard
  schema        views, columns, meanings
  prometheus    a local Prometheus UI: the window loaded, older ranges on query
  kubectl       kubectl against the cluster as it was at a time (read-only)
  contexts      the configured contexts: bucket, cluster and AWS profile
  version       y-obs, DuckDB and Go versions, and where httpfs comes from
  apiserver-at  serve a state file to kubectl (what kubectl and the itest use)
```

Shared flags of the commands that read the bucket:

- `--context`, `--bucket`, `--cluster`, `--node`;
- `--since=1h`, or `--from=T` and `--to=T`;
- `--limit`, `--format`, `--explain`, `--max-objects`;
- `--out=FILE.duckdb[:TABLE]`, `--append`, `--attach=FILE.duckdb`;
- `--blobs-dir` (a local directory laid out like the bucket) and
  `--parquet-keys-dir`.

`sql` also takes `--db=FILE.duckdb`, and `pods` and `kubectl` take
`--at=T`. T is RFC 3339 or `2026-10-11 09:00`, always UTC. `--at` also
takes `now`, its default, and a duration before now: `-30m`, `-2d`.

**Which tier.** `pods`, `exits`, `logs`, `grep` and `metrics` read each
hour of the window from exactly one tier. It is the hourly tier when the
hour is final (more than the compactor's grace, at most 30 min, past its
end), else raw when raw objects remain. A day that only the daily tier
still has reads the daily tier. Rows are cut to the window by time. The
summary line counts the hours per tier.

### status

```
y-obs status
y-obs status --cluster prod --full
y-obs status --format json
```

What y-obs can reach, and what is there, in three sections (TSV with a
header each, or one JSON object):

- **contexts.** Like `kubectl config get-contexts`: each context's
  bucket, cluster and AWS profile, the one in use marked, and for that
  one the S3 endpoint and where its credential comes from and until
  when. Never the secret.
- **prometheus.** The running `y-obs prometheus` instances.
- **data.** Per cluster in the bucket in use, and per tier (raw,
  hourly, daily):
  - the first and last hour (raw) or day;
  - objects and bytes;
  - the newest upload and how long ago;
  - for raw, the nodes that uploaded in its last hour, the cluster agent
    apart.

  The clusters come from the credential's scope, or from delimiter
  listings that skip prefixes holding no cluster's data. Hourly and
  daily are listed in full. Raw, about 120 objects per node per hour,
  is measured over its last 24 hours of data, unless `--full` lists all
  of it.

Retention is the bucket's lifecycle rules, which a read-only key cannot
see: the spans say how far back each tier reaches, not when it expires.
On one hour of a single-node cluster's bucket it made 33 listing
requests in 0.1 s.

### pods

```
y-obs pods -n qa --name '^build-' --since 6h
y-obs pods --label app=gateway --at '2026-10-11 09:00'
y-obs pods --node worker-3 --since 1d --phase failed
```

One row per incarnation (pod_uid), aggregated from the window's pods
index:

```
namespace  pod  pod_uid  node  workload  owner_kind  started  ended  phase  restarts  last_exit  containers
```

- `workload` and `owner_kind` come from labels.
- `phase` is running (no deletion seen), succeeded, failed or unknown.
- `started` is the earliest sandbox start seen in the window.
- `ended` is the deletion seen, empty when none was.

Filters: `-n`, `--name` (RE2), `--label k=v` (repeatable), `--node`,
`--uid`, `--workload` and `--phase`.

`--at` keeps the pods alive at that instant: started by then, and ended
after it, or, with no deletion seen, seen in the hour before it (the gap
rule). The window then defaults to T±1h, up to now.

Without `--at` and without a window flag (`--since`, `--from`, `--to`),
`pods` lists the pods alive now, like `kubectl get pods`. With a window
flag and no `--at` it lists every pod seen in the window.

### exits

```
y-obs exits -n qa --since 1d
y-obs exits --reason OOMKilled --since 7d
y-obs exits --uid 3f2a... --failed
```

The `container-exit` rows of the window's podindex, newest first, each
once:

- time, namespace, pod, pod_uid, container, restart, exit_code,
  exit_reason and exit_message;
- memory_limit and cpu_quota from that run's container row, and the
  node.

### logs

```
y-obs logs ci build-abc-verify-pod step-verify --tail 200
y-obs logs ops bot-xyz --grep error --since 3h --timestamps
y-obs logs qa web-1 --uid 3f2a... --previous
```

The arguments are those of the logs API and its CLI, with the same line
format: `--tail`, applied after grep (default 100, 0 for all), `--grep`
(substring, or `^`prefix), `--timestamps`, `--prefix` and
`--ansi=strip`. On top of those:

- `--since`, `--from` and `--to` set a time window, which the API never
  had.
- `--uid` and `--restart` pick an incarnation and a run.
- `--previous` picks the run before each container's latest.
- `--format jsonl` gives time, container, restart, pod_uid and message
  as fields.

Journal rows use the namespace `_node`, with the node as pod and the unit
as container: `y-obs logs _node worker-3 kubelet.service --grep error`.
An unknown container or pod is exit 4, with the pod's containers or the
namespace's closest pods.

### grep

```
y-obs grep 'connection reset' -n qa --since 2h
y-obs grep 'OutOfMemory' --name '^gateway-' --since 1d --count
y-obs grep 'timeout|refused' --regex -i -n qa --since 6h
```

It searches log content across pods: a substring, or an RE2 expression
with `--regex`, `-i` for either. Without `-n` or `--name` the plan
covers the whole cluster and usually meets the cost guard.

- **Lines.** It prints the newest `--limit` matches oldest first,
  prefixed `[ns/pod/container] TIMESTAMP`.
- **Counts.** `--count` prints matches per pod and container run with
  the first and last, the cheap first pass.

### metrics

```
y-obs metrics names -n qa --pod '^web-'
y-obs metrics series container_memory_working_set_bytes --pod '^web-1$' --since 6h --step 1m
y-obs metrics rate http_requests_total -n qa --by code --since 1h --step 5m
y-obs metrics latest up --where scrape=pod --value 0
y-obs metrics rate http_requests_total -n qa --by k8s_deployment_name --since 1h
```

Every scraped sample is in the bucket, so there is no allowlist and no
90-minute cliff: history is the bucket's retention. A series is one
target (pod_uid, container, instance) and label set. The subcommands:

- `names` gives the metric names with type, series, samples and last.
- `series` gives the last value of each series per `--step` (default 1m).
- `rate` gives the per-second increase per `--step` (default 5m). A drop
  is a counter reset counted from zero, as PromQL's `rate()` does.
- `latest` gives the newest sample per series; `--value` keeps series
  at a value.

`--where key=value` and `--by key,...` take labels or the namespace,
pod, pod_uid, container, node, scrape (`--scrape`) and instance columns, and the workload
names (`k8s_deployment_name` and the rest). `--agg` (sum,
avg, max, min) combines the series sharing the `--by` keys. An unknown
name is exit 4, with the closest names in the window.

PromQL itself is out of scope here. `local/README.md`'s Prometheus plus
blobs-to-prometheus stack serves it from the same bucket.

### prometheus

```
y-obs prometheus                      # the last hour loaded, then following
y-obs prometheus --since 6h
y-obs prometheus --from '2026-10-11 08:00' --to '2026-10-11 10:00'
y-obs prometheus --status
y-obs prometheus --stop
```

A local Prometheus over the bucket, for exploring metrics in its UI. It
prints the URL (stdout) and opens it in the browser.

- **The window is loaded at start.** `--since`, default 1h, is
  remote-written into it, so autocomplete knows every name and label of
  the window, and its range is local. It loads in two steps, measured on
  one hour of a single-node cluster (1.6 M samples):
  1. **Catalog.** The oldest sample of every series. Every name
     autocompletes after about 4 s, and the browser opens then.
  2. **Samples.** The rest of each series in order; the hour is in
     after about 6.5 s (4.9 s before the engine's memory and thread
     caps). In order matters: sending a series' newest sample
     first made Prometheus take the rest out of order, at less than half
     the speed.

  A window with no object is exit 4 with what the bucket has, and one
  whose objects hold no metrics exit 1; a load over budget exits 5.

  Progress goes to stderr, as one updating line on a terminal or a line
  per step otherwise. The command returns, and prints the URL to stdout,
  once the whole window is in, so a script's first query sees all of
  it. A repeated call while it loads shows the same progress. Without
  `--to` it follows: every `--poll` (30 s) it lists the raw keys from 10
  minutes before the last poll, and loads only those no load has read.
  Raw objects never change, so that is about one object per node per
  poll.
- **Older ranges load on query.** Prometheus is configured with
  `remote_read` to y-obs, which answers from the bucket through the
  window planner. A namespace matcher prunes the daily tier.
  - Each read is cut to before the loaded window. Prometheus asks for
    every range anyway: its `read_recent: false` goes by the head's
    start, which the loaded out-of-order samples do not move.
  - Measured: 6 h of one name 0.3 s, 24 h 0.7 s, 6 h of 4.3 M samples
    5 s.
  - A read over `--max-objects` or `--max-samples` comes back as a
    warning in the UI, with y-obs's message.
- **One instance per context and cluster.** A repeated call reuses it
  (its process alive and Prometheus ready). A call with other `--since`,
  `--from` or `--to` than it was started with is exit 2, rather than an
  answer for the old window. `--stop` ends it, and
  `--status` lists the running ones. State and logs are in
  `$XDG_RUNTIME_DIR/y-obs/prometheus/`, and Prometheus's data is in a
  temporary directory removed at stop.
- **The Targets page** has one target, y-obs itself. Its `yobs_*`
  metrics say what was loaded and what remote reads cost. The original
  scrape targets are not targets here; their `up` series are queryable
  as any other.
- **Series.** The series are blobs-to-prometheus's: `__name__`, the
  non-empty cluster, node, scrape, instance, namespace, pod, container and
  workload names, then the sample's labels, renamed `exported_<name>` on
  a clash.
- **The server binary.** It is ystack's `y-prometheus` (Prometheus
  3.15.0), `prometheus` on PATH, or `PROMETHEUS`.
- **Implementation.** `prometheus-serve` is the part that runs detached.

### kubectl

```
y-obs kubectl -- get pods -A
y-obs kubectl --at -2h -- get deploy -n qa
y-obs kubectl --at '2026-10-11 09:00' -- get pods -A
y-obs kubectl --at '2026-10-11 09:00' -- describe deploy -n qa web
y-obs kubectl --at '2026-10-11 11:58' --source raw -- get events -n qa
```

This is `local/kubectl-at.sh` as a subcommand, in-process: `apiserver_at(T)`
in the embedded engine, the state served by apiserver-at, with the
kubeconfig in a temporary directory.

- **The time.** `--at` is now by default, or `-30m`, `-2d`, or a time.
- **Objects.** They come from the hourly tier (T's hour and the two
  before), else the daily tier (T's day and the day before), or the raw
  tier with `--source raw`: the cluster agent's objects only (node
  `_k8sagent`, K8SAGENT.md), unless `--node` says otherwise. The default,
  `--source auto`, reads raw while T's hourly object is not final (its
  hour's end plus 30 min) and the tiers after, so `now` is current and
  not the last final hour.
- **Ages count to T.** The API server prints `kubectl get`'s tables and
  computes their ages from its own clock. y-obs's read-only proxy
  rewrites them to count to T: `AGE` from each object's
  creationTimestamp, and `LAST SEEN`, `LAST SCHEDULE` and a custom
  resource's date columns shifted back by now minus T. `kubectl
  describe` computes its event ages itself, from now; its timestamps are
  absolute.
- **Left out.** APIServices served by a Service, such as
  metrics-server's `v1beta1.metrics.k8s.io`, are not loaded: their
  backends are not there, and every discovery would warn. So
  `kubectl top` and other aggregated APIs do not answer; `-v` counts them.
- **The release.** kube-apiserver and etcd are the cluster's release at
  T: `--assets`, `KUBEBUILDER_ASSETS`, or setup-envtest.
- **Without kubectl arguments** it serves and prints the `KUBECONFIG`.

### Output to a DuckDB file

Every command that prints rows can write them to a DuckDB database file
instead. That includes `pods`, `exits`, `logs`, `grep`, the `metrics`
subcommands and `sql`. `kubectl` does not, because its output is
kubectl's.

```
y-obs pods -n qa --since 1d --out inv.duckdb                    # table pods
y-obs logs qa web-1 --since 6h --limit 0 --out inv.duckdb:web1  # table web1
y-obs sql "SELECT * FROM metrics WHERE name LIKE 'container_memory%'" \
    -n qa --since 1d --out inv.duckdb:mem
y-obs sql --db inv.duckdb "SELECT pod, max(value) FROM mem GROUP BY 1"  # local only
```

- **The table.** The table is named after the command (`pods`, `exits`,
  `logs`, `grep`, `metrics_series`; `result` for `sql`) unless `:TABLE`
  names it.
- **Replace or append.** The table is replaced; `--append` adds rows to
  an existing table with the same columns instead.
- **Typed rows, uncut.** The rows are typed as in the views, so times
  stay `TIMESTAMP_NS` and labels stay a `MAP`, not text. `--limit`
  applies only when given: written output is not cut by default.
- **What is printed instead.** stdout gets what an agent needs next: the
  table, its row count, its columns with types, and one example query.

  ```
  wrote 18342 rows to inv.duckdb table web1 (replaced)
  columns: time TIMESTAMP_NS, namespace VARCHAR, pod VARCHAR, pod_uid VARCHAR, container VARCHAR, restart INTEGER, stream VARCHAR, message VARCHAR
  next: y-obs sql --db inv.duckdb "SELECT * FROM web1 ORDER BY time DESC LIMIT 20"
  ```

- **Provenance.** Each write adds a row to the file's `y_obs_runs`
  table: the table, the mode (created, replaced or appended), the command
  line, cluster, window, the objects read per view, the rows that run
  wrote, and the time written. (`fresh-to` is in the summary line, not
  yet in this table.) A later reader can then see
  what a table covers and how fresh it was. Credentials are never
  written.
- **Querying a file.** `--db FILE` makes `sql` work on the file alone,
  its tables unqualified, whatever bucket settings the environment holds.
  `--attach FILE` attaches the file as `local` beside the bucket's views,
  so a query can join them. For example, `pods` from the bucket against
  `local.uids`, saved earlier. Two flags, not one: a saved table named
  `pods` must never shadow the bucket's view of that name, or the reverse.
- **Other formats.** `--out` also takes `.parquet`, `.csv` and `.jsonl`,
  through DuckDB's COPY, for handing a result to another tool. The table
  name and `y_obs_runs` then do not apply.
- **The file is sensitive.** It holds log lines, so it is created 0600.
  A file is the user's own copy, outside the bucket's retention; the
  skill tells agents to keep it in a scratch directory and delete it
  when the investigation ends.

### sql

```
y-obs sql "SELECT pod, count(*) FROM logs WHERE namespace = 'qa' GROUP BY 1 ORDER BY 2 DESC" --since 1h -n qa
```

The views are those of `local/blobs.sh`. `--since`, `-n` and `--name`
scope the raw view's files, so the budget guard applies. `--tier=hourly`
reads the `hourly_*` views instead.

### S3 access

y-obs reads the bucket with the AWS SDK's default credential chain, as
the AWS CLI does, and has no credential settings of its own:

- **Credentials.** `AWS_ACCESS_KEY_ID` and the rest from the
  environment, else the profile (`AWS_PROFILE`, or the context's) in
  `~/.aws/config` and `~/.aws/credentials`: static keys,
  `credential_process`, SSO, or a role; else a container or instance
  role. A `credential_process` command prints `AccessKeyId`,
  `SecretAccessKey`, an optional `SessionToken` and `Expiration`.
- **Renewal.** A credential with an expiry is renewed 2 min before it
  (or half its remaining life, when shorter) by resolving the chain
  again, which runs `credential_process` again. The new secret replaces
  the database's from a second connection, so a running query carries
  on; httpfs re-reads secrets on a refused request. A renewal that fails
  is printed, and the reads that then fail end the command with exit 3.
- **Endpoint and region.** `AWS_ENDPOINT_URL_S3`, else the profile's
  `services` section's `s3` `endpoint_url`, else `AWS_ENDPOINT_URL` or
  the profile's `endpoint_url`, else AWS's own for the region; TLS as the
  URL's scheme says. The region from `AWS_REGION` or the profile, else
  `us-east-1`. Requests are path-style, which every S3 implementation
  takes.
- **Bucket and cluster.** `--bucket`, else `S3_BUCKET`, else the
  context's. The cluster is `--cluster` (or `CLUSTER_NAME`), else the
  context's when the bucket is the context's, else the bucket's only
  cluster.
- **What is explicit wins.** `AWS_PROFILE` wins over the profile of a
  context that the file picks (its `current` or only one); a context
  named by `--context` or `Y_OBS_CONTEXT` keeps its own.
- **Contexts** name where to read and never hold a secret. In
  `~/.config/y-obs/contexts.yaml` (or `Y_OBS_CONFIG`):

  ```yaml
  current: prod
  contexts:
    prod:
      bucket: node-data
      cluster: prod-eu
      profile: observability-read
  ```

  with, in `~/.aws/config`:

  ```ini
  [profile observability-read]
  credential_process = obs-grant --read prod-eu
  endpoint_url = https://objects.example.net
  region = auto
  ```

  `--context` picks one, else `Y_OBS_CONTEXT`, else `current`, else the
  only one. `y-obs contexts` lists them; `y-obs status` also resolves the
  one in use and shows its endpoint and its credential's source and
  expiry, and exits 3 when it gets no credential.

What issues the credentials (a grant service behind `credential_process`,
an SSO role, a reader key) is the deployment's; y-obs needs read and list
on the bucket's `raw1/`, `hourly/` and `daily/` prefixes of the clusters
it reads.

## Encrypted tiers

With tier encryption on, reading the hourly and daily objects needs the
parquet keys as well as the S3 credential. A parquet key never changes,
so a key handed to a laptop stays valid past any credential TTL.

- **Decide:** whether whatever issues the S3 credential also hands out
  the keys (the credential then
  bounds access to the objects, not to their content), or node data
  relies on the store's own encryption at rest and the tiers stay plain.

## Reads are not logged

`y-obs` runs every query locally, and nothing sends the query or its
results anywhere. Whatever issues the credential sees who asked for it,
and when. The
store sees GETs of object keys under that credential, and those keys
carry namespace and pod. To keep reads unidentifiable, the node-data
bucket's access logging stays off. That is a property of the bucket, not
of y-obs, so the deployment documents it.

## Order of work

1. Done: `apiserver-at` became `y-obs`, with embedded DuckDB and SQL,
   and `sql`, `schema` and `kubectl`. The itest runs `y-obs
   apiserver-at`, and a phase of its own runs the investigation commands
   and `prometheus` against what the agents shipped.
2. Done: the window planner, then `pods`, `exits`, `logs`, `grep` and
   `metrics`, each with its rules for LLM callers.
3. Done: the release workflow and `release.sh`. The `y-bin` entry
   follows the first release, whose checksums it pins.
4. Done: S3 access through the AWS default credential chain, with
   renewal, and contexts that name a bucket, a cluster and a profile.
5. Started: consuming repositories add `SKILL.md` from this directory as
   an `observability` skill, beside their log, pod-index and metrics
   skills. The old skills point at it for clusters that have moved, and
   stay until every cluster has. Each cluster gets a context line in
   the skill when it moves.
6. Done: `fresh-to` (from the raw keys), and the clusters and dates of
   a bucket for exit 4 (delimiter listings).
