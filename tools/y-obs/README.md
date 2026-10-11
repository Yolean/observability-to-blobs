# y-obs: the bucket as the only observability backend (draft)

> Draft for review. Open decisions are marked **Decide**; the rest is a
> proposal to change freely. Built so far:
> - `apiserver-at`;
> - `sql` and `schema`, with static keys from the environment (as for
>   `local/blobs.sh`) or `--blobs-dir`;
> - `--out`, `--db` and `--attach` on `sql`;
> - `kubectl --at`, in-process: no duckdb CLI, temp files or go needed
>   when `setup-envtest` or `KUBEBUILDER_ASSETS` provides the release;
> - contexts, `auth` with cache and renewal, and `mint-stub`.
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

- Its server becomes the `kubectl` subcommand. It still downloads
  kube-apiserver and etcd of the cluster's release on first use, into the
  cache directory.
- It embeds DuckDB with `github.com/duckdb/duckdb-go/v2`, the version the
  compactor pins, so the SQL behaves the same as the compactor's.
- It embeds the `.sql` view files with `go:embed`, so the binary needs no
  checkout.
- It embeds the httpfs extension, sha256-pinned per platform as the
  compactor's `build.env` does. On first use the binary writes it to the
  cache directory and `LOAD`s it by path, with autoinstall off.
- It reuses the stdlib S3 client (`images/blobs-to-prometheus/s3.go` with
  `sigv4.go`) for listings and HEADs. DuckDB reads the objects.

**Release.** A tag builds four binaries, linux/amd64, linux/arm64,
darwin/arm64 and darwin/amd64, each on a native runner. This is because
duckdb-go links prebuilt static libraries per platform through cgo. The
release attaches the binaries with their sha256 files, so a `y-bin`
download entry can pin them like any other tool.

## Design rules for LLM callers

An agent calls `y-obs` many times per investigation, reads every byte of
output into its context, and cannot see a progress bar. So:

1. **Bounded by default.** Every command has a time window (default: the
   last hour) and a row limit (default 100 lines, 50 rows). When
   output is cut, the last line on stderr says so and how to narrow:
   `y-obs: 412 more lines; add --grep, --container or --since, or raise --limit`.
2. **Cost guard.** Before reading anything, `y-obs` plans the keys or
   globs from the window, cluster, namespace and pod. If the plan exceeds
   `--max-objects` (default 200), it refuses with exit 5 and prints the
   plan. `--explain` prints the plan without reading: tier, keys, and
   estimated requests.
3. **Stable, parseable output.** `--format text|tsv|json|jsonl`. text is
   the default for `logs` and `grep`, tsv for everything else. Columns
   come in a fixed order with a header line. Timestamps are always UTC
   RFC 3339 with nanoseconds. There is no colour, and none of the output
   depends on terminal width.
4. **Says where the data came from.** On exit, stderr gets one summary
   line: `y-obs: tier=raw+hourly objects=37 requests=118 window=2026-10-11T08:00Z..09:12Z fresh-to=09:11:42Z`.
   An agent can then tell an empty window from data not yet uploaded.
5. **Errors are one line with a next step**, plus documented exit codes:
   - 0: ok, results printed;
   - 1: ok, nothing matched;
   - 2: usage;
   - 3: credentials (`run y-obs auth`);
   - 4: not in the bucket (with the nearest match, for example the
     container names that do exist);
   - 5: the plan exceeds the budget;
   - 6: the store failed.
6. **Self-describing.**
   - `y-obs help <command>` prints usage with three examples.
   - `y-obs schema [table]` prints columns, types and one-line meanings,
     taken from the comments in `raw.sql`.
   - `y-obs help` lists the commands in the order an investigation uses
     them.
7. **Identity is the pod uid.** Names are reused by StatefulSets and
   retries, so every row that names a pod also carries `pod_uid`. Any
   command takes `--uid` to pin one incarnation.

8. **Results can stay local.** Every command takes `--out FILE.duckdb`
   and writes its rows into a table there instead of printing them. Later
   questions then run against that file, with no credentials, no budget
   and no bucket reads (below, "Output to a DuckDB file").

## Commands

```
y-obs [--context=NAME] <command> [flags]

  pods      which pods ran: by namespace, name, label, node, phase, window
  exits     why containers ended: exit code, reason, OOMKilled, eviction
  logs      one pod's lines (all containers merged, or one container)
  grep      a pattern across pods and hours, scoped by namespace/pod/window
  metrics   names, series, rate, latest: every scraped sample, no allowlist
  kubectl   kubectl against the cluster as it was at a time (read-only)
  sql       a DuckDB query over the views, with the same budget guard
  schema    tables, columns, meanings
  auth      fetch or show the read credential (never prints the secret)
  contexts  configured clusters and buckets
  version
```

Shared flags: `--since=1h` | `--from=T --to=T` | `--at=T` (kubectl),
`--namespace/-n`, `--limit`, `--format`, `--explain`, `--max-objects`,
`--out=FILE.duckdb[:TABLE]`, `--append`, `--db=FILE.duckdb`, `--attach=FILE.duckdb`.
T is RFC 3339 or `2026-10-11 09:00`, always UTC.

### pods

```
y-obs pods -n qa --name '^build-' --since 6h
y-obs pods --label app=gateway --at '2026-10-11 09:00'
y-obs pods --node worker-3 --since 1d --phase Failed
```

This command gives one row per incarnation, from the `pods` view: hourly
tier for complete hours, raw for the current one. Its columns:

```
namespace  pod  pod_uid  node  started  ended  phase  restarts  last_exit
```

`--at` keeps the pods alive at that instant. Alive means started before
it, and ended after it or not ended. A pod whose deletion was not seen
counts as alive until one checkpoint interval after its last row, the
gap rule. `--name` takes a RE2 pattern.

### exits

```
y-obs exits -n qa --since 1d
y-obs exits --reason OOMKilled --since 7d
y-obs exits --uid 3f2a...
```

This lists `container-exit` rows from podindex: time, namespace, pod,
pod_uid, container, restart, exit_code, exit_reason and exit_message. For
the limits in force at the exit, it joins memory_limit and cpu_quota
from the container rows.

### logs

```
y-obs logs ci build-abc-verify-pod step-verify --tail 200
y-obs logs ops bot-xyz --grep error --since 3h --timestamps
y-obs logs qa web-1 --uid 3f2a... --restart 2
```

The arguments are those of the logs API and its CLI: `--tail`, applied
after grep, `--grep` (substring, or `^`prefix), `--timestamps`,
`--prefix` and `--ansi=strip`. On top of those:

- `--since`, `--from` and `--to` set a time window, which the API never
  had.
- `--uid` and `--restart` pick an incarnation and a run.
- `--previous` picks the run before the last restart.

Journal rows use the namespace `_node` with the node as pod, for example
`y-obs logs _node worker-3 --grep kubelet`.

### grep

```
y-obs grep 'connection reset' -n qa --since 2h
y-obs grep 'OutOfMemory' --name '^gateway-' --since 1d --count
```

It searches log content across pods. It needs `-n` or `--name`; with
neither, the plan covers the whole cluster and normally hits the cost
guard. It prints matching lines prefixed `[ns/pod/container]`. With
`--count` it prints matches per pod instead, which is the cheap first
pass before reading lines.

### metrics

```
y-obs metrics names -n qa --pod '^web-'
y-obs metrics series container_memory_working_set_bytes --pod web-1 --since 6h --step 1m
y-obs metrics rate http_requests_total -n qa --by code --since 1h --step 5m
y-obs metrics latest up --where job=pod --value 0
```

Every scraped sample is in the bucket, so there is no allowlist and no
90-minute cliff: history is the bucket's retention. The subcommands:

- `series` gives samples, bucketed by `--step` (last value per step).
- `rate` gives the per-second increase with counter resets handled, the
  same as PromQL's `rate()`.
- `latest` gives the latest sample per series.
- `--where label=value` filters on labels and `--by label,...` groups
  by them.

PromQL itself is out of scope here. `local/README.md`'s Prometheus plus
blobs-to-prometheus stack serves it from the same bucket.

### kubectl

```
y-obs kubectl --at '2026-10-11 09:00' -- get pods -A
y-obs kubectl --at '2026-10-11 09:00' -- describe deploy -n qa web
```

This is `local/kubectl-at.sh` as a subcommand: apiserver-at over
`apiserver_at(T)`, with the kubeconfig in a temporary directory.

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
  wrote, and the time written. `fresh-to` joins them once the planner
  knows upload times. A later reader can then see
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

### auth and contexts

A context is what one credential can read: one bucket at one endpoint,
and the mint that hands out its read credentials. The credential's scope
may narrow the bucket to some clusters; `--cluster` picks among them,
and defaults to the only one.

`~/.config/y-obs/contexts.yaml` (or `Y_OBS_CONFIG`):

```yaml
current: prod
contexts:
  prod:
    bucket: node-data
    endpoint: objects.example.net
    region: auto
    credentials:
      url: https://obs-creds.example.net/v1/read     # behind an identity-aware proxy
      token: y-remote-iap print https://obs-creds.example.net
```

- **Which context.** `--context`, else `Y_OBS_CONTEXT`, else `current`,
  else the only one. `S3_BUCKET` in the environment wins over `current`,
  as `local/blobs.sh`'s static keys.
- **Minting.** y-obs runs the token command and POSTs to `url` with the
  output as a bearer token. The answer is `{accessKeyId, secretAccessKey,
  sessionToken, expiration}`, optionally with `bucket`, `endpoint` and
  `region` overriding the context's, and `clusters`, the scope.
- **Cache and renewal.** The credential is cached 0600 in
  `$XDG_RUNTIME_DIR/y-obs/` (else the user cache directory). It is used
  until a margin before it expires: 2 min, or half its lifetime when
  that is shorter. A session that outlives it gets a new one at the
  margin. The new secret replaces the database's from a second
  connection, so a running query carries on; httpfs re-reads secrets on
  a refused request.
- **The auth command.** `y-obs auth` prints the context, bucket, scope,
  source (cached or minted) and expiry, never the secret. `--renew`
  mints even when the cache is valid. `y-obs auth -- duckdb` runs a
  program with the credential in its environment (the `AWS_*` and `S3_*`
  variables of `local/blobs.sh`).
- **Listing contexts.** `y-obs contexts` lists the contexts with the
  clusters their cached credential covers. Listing the clusters present
  in the bucket, with their newest upload, needs a delimiter listing; it
  comes with the stdlib S3 client.
- **The stub mint.** `y-obs mint-stub` is a mint for development. It
  hands out the static keys it was started with, under an expiry: the
  protocol of a real mint, none of its security.
- **Exit codes.** A token command that fails, a mint that refuses or is
  down, and a cluster outside the scope are all exit 3.

## Credentials

Read credentials are short-lived, read-only, and scoped to one bucket
(optionally to `raw1/{cluster}/`, `hourly/{cluster}/` and
`daily/{cluster}/`). Write credentials stay as they are: one per cluster,
long-lived, write-only.

What mints them is per store:

- **Decide:** where the minting principal runs. Keys that can create
  other keys never exist in the hosting, so the mint runs outside the
  clusters that produce the data, behind the same identity-aware proxy
  the logs CLI already authenticates with.
- **Decide:** the TTL within 5–15 min, and whether the cache above is
  acceptable. Without it, every invocation mints.

Per store (read 2026-10-11 from provider docs and DuckDB v1.5.6 httpfs
source; marked where untested):

| store | mint | minting principal | DuckDB secret |
|---|---|---|---|
| Cloudflare R2 | temporary credentials, `object-read-only` with `prefixes`, TTL in seconds; minted locally by signing with the parent's S3 secret, no API call | a bucket-scoped read-only token's S3 key pair, which cannot create tokens; runs in a Worker with the pair as a secret | `TYPE s3` with `SESSION_TOKEN` |
| GCS | a downscoped OAuth token (Credential Access Boundary: `storage.objectViewer`, `startsWith` and `objectListPrefix` per prefix), TTL at most the source token's | a service account with object read only and no key-creation permission, keyless on a serverless runtime | `TYPE gcs` with `BEARER_TOKEN`, which httpfs sends for `gs://` URLs only, so the context's prefixes are `gs://`. Untested against a downscoped token |
| Ceph RGW (S3-compatible hosting) | none documented. The gateway answers STS actions, so `GetSessionToken` from a reader account may work; untested | a separate reader account granted read on the bucket by bucket policy. The provider API key that creates accounts and keys stays out of every cluster | `TYPE s3` with `SESSION_TOKEN` if STS works |
| versitygw (itest) | `AssumeRoleWithWebIdentity` since v1.8.0, the itest pins v1.7.0 | an OIDC issuer the gateway trusts | `TYPE s3` with `SESSION_TOKEN`; untested |

If a store has no temporary credentials, the fallbacks are both weaker:

- **A long-lived reader key on the operator's machine.** It is read-only
  and outside the hosting, but it does not expire.
- **A read proxy that holds that key.** The proxy sees every object key
  read.

Presigned URLs alone do not work, because the views list prefixes.

On the DuckDB side, httpfs v1.5.6 reads a secret when it opens a file. On
401, 403 or `ExpiredToken` it re-reads the secret manager and retries
once (`httpfs_enable_credential_refresh`, on by default). So y-obs renews
by `CREATE OR REPLACE SECRET` from a timer, and a long query survives the
TTL. This was read in the source, not tested.

## Encrypted tiers

With tier encryption on, reading the hourly and daily objects needs the
parquet keys as well as the S3 credential. A parquet key never changes,
so a key handed to a laptop stays valid past any credential TTL.

- **Decide:** whether the mint also returns the keys (the credential then
  bounds access to the objects, not to their content), or node data
  relies on the store's own encryption at rest and the tiers stay plain.

## Reads are not logged

`y-obs` runs every query locally, and nothing sends the query or its
results anywhere. The mint sees who asked for a credential, and when. The
store sees GETs of object keys under that credential, and those keys
carry namespace and pod. To keep reads unidentifiable, the node-data
bucket's access logging stays off. That is a property of the bucket, not
of y-obs, so the deployment documents it.

## Order of work

1. Turn `apiserver-at` into `y-obs`: embedded DuckDB, embedded SQL, and
   the `sql`, `schema` and `kubectl` subcommands, using static keys from
   the environment. The itest calls `y-obs sql` in place of the duckdb
   CLI for a few assertions, so the binary is tested.
2. Add `pods`, `exits`, `logs` and `grep`, with the planner, budget and
   output rules. Then `metrics`.
3. Add the release workflow, then the `y-bin` entry.
4. Build the credential mint for the chosen store, then `auth` and the
   contexts.
5. Replace the consuming repositories' log, pod-index and metrics skills
   with `SKILL.md` from this directory.
