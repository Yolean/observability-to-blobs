# kubernetes-node-agent-to-blobs

One agent per node that ships everything the node knows about its workloads
to S3-compatible storage: one zstd-compressed JSON-lines object per node
about every 30 s, holding container and node logs, Prometheus samples and
pod lifecycle events. A compactor turns those raw objects into the parquet
tiers that people and alerting read. Successor to
[kubernetes-logs-datalake](https://github.com/Yolean/kubernetes-logs-datalake),
built on a fork of Fluent Bit that carries a single patch.

Status: the agent and the compactor (its design, build and deployment
in [compactor/README.md](compactor/README.md)) are built and tested; the
agents can also upload to the compactor's S3 put endpoint instead of the
bucket, so that raw objects never reach object storage (spool mode); the
compactor serves container logs by URL, for clients of an existing logs
API; the alerting tailer follows. The PoC-era `CONSUMING.md` and `COMPACTING.md` keep measurements
from the first iteration and are superseded where they differ.

## What the agent ships

```
/raw1/{cluster}/{YYYY-MM-DD}/{HH}/{node}/{MMSS}-{uuid}.jsonl.zst
```

Every line is one event with a `table` field:

- **logs**: every container log line (CRI, partial lines joined, nothing
  filtered) with namespace, pod, `pod_uid`, container and the container's
  restart count from the log path; plus the node's kubelet, containerd and
  kernel journal entries with `unit` and `priority`.
- **metrics**: one line per Prometheus scrape of every pod on the node that
  opts in via `prometheus.io/*` annotations or the OpenTelemetry port-scoped
  hints (multi-container pods opt in per port), plus node_exporter and the
  agent's own metrics, with the samples as an array and the target's
  `pod_uid`.
- **podindex**: pod and container lifecycle events from the CRI
  (`pod-observed`, `pod-checkpoint`, `pod-deleted`, `container`,
  `container-exit`), the source of "which incarnations of this pod name
  existed when, and how did they die": pod rows carry labels, sandbox
  start, QoS class and pod IP; container rows the image as written and
  its resolved digest, start time and the memory and CPU limits in force;
  exit rows the code, reason and runtime message.

The raw tier is for the compactor and the alerting tailer, which LIST an
hour prefix and take new keys. It is not a query tier; the compacted
`hourly/` and `daily/` parquet tiers are (`compactor/README.md`).

What it needs from the node: `/var/log/pods`, `/var/log/journal`, the
containerd socket, `/proc` and `/sys`, and nothing else of `/var/log`. It
never talks to the Kubernetes API. An agent that cannot reach
the bucket buffers on disk and ships the backlog afterwards; the itest
proves it with a 100 s outage. The buffer is bounded at about 8G per node
(`agent/fluent-bit.yaml`); an outage that outgrows it drops the oldest
input chunks.

## Layout

| path | what |
|---|---|
| `images/fluentbit/` | Dockerfile (Fluent Bit v5.1.2, one patch), the `cri_scrape` Go input plugin, `UPSTREAM.md` with findings to report |
| `agent/` | kustomize base: DaemonSet, `fluent-bit.yaml` pipeline (stock processors, no Lua), the `pipeline-extra.yaml` composition hook |
| `itest/` | y-cluster configs for a local k3s node (qemu and docker), versitygw, the workloads that exercise each path, the compactor's overlays (bucket mode, and spool mode with stock Fluent Bit as a second shipper) |
| `images/blobs-to-prometheus/`, `tools/rules-manifest/`, `local/`, `alerts-cf/` | the alerting consumer from the PoC; rewired to the new blob in phase 3 |
| `compactor/` | `blobs-compactor` (fold, rollup and the loop that runs them, and the logs API), its design (`README.md`), its SQL (`sql/raw.sql` is the raw line contract every consumer reads), its build script and `deploy/` |
| `examples/gke/` | sketch of a production overlay |
| `test.sh` | the one command |

## Running

```bash
./test.sh            # checks, unit tests, image build, itest on a local k3s node
./test.sh checks     # checks and unit tests only
./test.sh up         # provision + deploy the agent, the compactor and sample workloads, no tests
```

Needs go, kubectl, shellcheck, duckdb, docker (or, without docker, `buildctl`:
`IMAGE_BUILDER` in `test.sh` builds the image with a rootless BuildKit in the
itest cluster) and
[y-cluster](https://github.com/Yolean/y-cluster/releases). With `/dev/kvm`
the itest node is a qemu VM (Ubuntu, systemd, containerd: node logs and CRI
discovery behave as in production); otherwise k3s-in-docker, which has no
journal. CI runs the same script. The compactor's image is built by
`compactor/build.sh` for the host's arch, which needs only docker.
`SKIP_BUILD=1 ./test.sh itest` reuses the last images for a quick loop
against the running cluster.

**Run the tests before every push**, in this repository and in forks
alike: `./test.sh` for any change to code, manifests, `test.sh` or
`itest/`, and at least `./test.sh checks` for documentation only. Say in
the pull request what you ran and its result. CI runs on pushes and pull
requests in the upstream repository only; in a fork it runs only when
started by hand (Actions, "ci", "Run workflow"), so a push there is not
tested unless you tested it.

## What the itest verifies

Against a real k3s node, from the raw objects read back with DuckDB's
`read_json` and the column types the compactor pins:

- keys `/raw1/cluster/date/HH/node/MMSS-uuid.jsonl.zst`, one to three
  objects in every minute after the outage, each carrying logs and
  metrics lines; `ts` on every line, within a microsecond of `time`
- container logs with namespace/pod/pod_uid/container/restart from the
  path, stderr kept apart, CRI partial lines (>16 KiB) joined, distinct
  sub-second timestamps within a burst, the agent's own logs excluded
- a line read just before a DaemonSet restart arrives exactly once, from
  the exit flush
- the bucket's Service removed for 100 s: a line written meanwhile arrives
  exactly once, the generator's counter has no gap, and the agent's logs
  have no "will not retry" or quarantine line
- three incarnations of one pod name: three `pod_uid`s in the logs and
  three `pod-observed` events with their own `pod_started`
- journal rows and a `/dev/kmsg` line with `unit = 'kernel'` (qemu backend)
- podindex: identity and `pod_started`, container image and restart count,
  labels without `io.kubernetes.*` noise, QoS class and pod IP, image
  digest and container start time, limits of a Guaranteed pod, exit codes
  of the crash-looper, observed and deleted for a short-lived pod,
  checkpoints, and absent fields being absent rather than zero
- scraping by annotation: OpenTelemetry port-scoped hints (by number and
  by name, with `metrics_path`), classic prometheus.io with and without a
  port, no scrape without opt-in, a hanging target timing out with `up=0`,
  node_exporter and self metrics through the static target with the agent
  pod's identity, a healthy target scraped three to five times in every
  minute after the outage

And the compactor, deployed from `compactor/deploy/base` (the
`itest/compactor` overlay: versitygw, a 20 s rolling interval, a 1 min
grace, and Parquet encryption on, with keys `itest-1` and `itest-2`
generated once per checkout in `target/itest/parquet-keys`), folding the
same bucket while all of the above runs. Every tier read below goes
through the per-key union (`tier` in `test.sh`); the plain mode is
covered by the Go tests:

- after a rolling and a past pass that both started after a snapshot of
  the raw keys (read from the compactor's `/metrics`), every key of the
  snapshot is listed in its hour's sidecar, and the sidecars list no key
  the raw tier lacks
- per folded hour, the hourly `logs`, `metrics` and `podindex` objects
  hold exactly the distinct rows of the raw keys its sidecar lists, read
  through `compactor/sql/`: the same rows both ways, each once
- the hourly `pods` index has the three incarnations of `reused`
- with the compactor stopped, `blobs-compactor serve` (the read-only logs
  API) in a pod of its own serves a line the agent uploaded meanwhile,
  once, from `raw1/`: the line is in no hourly object
- with the compactor stopped, `blobs-compactor rollup <day>` in a pod of
  its own, for every day with folded hours (two when the run crosses
  midnight UTC), writes the daily tier: its `logs`, `metrics` and
  `podindex` hold exactly the distinct rows of every raw key of the day,
  a namespace's logs are one object under its hive key, the daily sidecar
  lists the day's keys, and the daily `pods` has the three incarnations,
  each with its line and exit 0
- an incremental rollup: a raw object of a namespace of its own, in a
  free hour of the last day, folded with `blobs-compactor fold` and
  rolled up again, rewrites that one namespace, leaves the other
  namespaces' `pods` rows as they were, and the day still holds exactly
  the distinct rows of its keys
- the compactor's `/metrics` reach the blobs through the agent's scrape
- the key rotation, before the snapshot: the hourly objects written under
  `itest-1` keep their names, so their key, after `PARQUET_KEY_ID` moves
  to `itest-2`; the last day's daily objects, first written after it, are
  under `itest-2`; every hourly and daily object is encrypted, one per
  hour, table and namespace; a read without the key fails

And a second compactor in spool mode (`itest/compactor-put`:
`compactor/deploy/spool` in namespace `observability-put`, cluster
`itest-put`, plain tiers in the same bucket), fed by stock Fluent Bit
v5.1.3 (the official image, pinned by digest) as a second shipper: it
tails the same pod logs with the agent's enrichment in stock processors
and uploads with `out_s3` to the compactor's put endpoint, never to the
bucket. After a rolling and a past pass of that compactor that started
after the snapshot:

- the shipper's log-generator lines are in `hourly/itest-put/` with
  namespace, pod, `pod_uid`, container, restart and stream from the CRI
  path; the restart marker, the outage marker and a marker written while
  the put compactor was down each arrive exactly once, and the
  generator's counter has no gap
- the put compactor stopped for 60 s: the shipper's log has failed
  uploads between the pod's end and its return, and it gave up on no
  chunk
- no `raw1/itest-put/` object exists in the bucket
- aws-cli as another client: its signed PutObject of an object for an
  hour three hours back is stored, folded by the past pass in a full read,
  so its hour is final, and its file is gone from the spool; the same PUT
  with a wrong secret is refused with `SignatureDoesNotMatch`, without a
  signature with `AccessDenied`, and no sidecar lists either key
- the spool, listed by a pod that mounts its volume, holds no file of an
  hour whose final fold has committed that was written before the put
  compactor's last past pass (a later upload to that hour waits for the
  next one)

And the logs API of both compactors (`compactor/deploy/logs`, through a
port-forward to the Service `blobs-compactor-logs`), at the end of the
run:

- the generator's last five lines with `tail=5&timestamps=true`: five
  lines, each a nine-digit CRI timestamp and the message, as
  `text/plain; charset=utf-8`, with `X-Logs-Source: blob`,
  `X-Logs-Tail-Effect: cap`, `X-Logs-Timestamp-End` the last line's, and
  `X-Logs-Pod-Phase: Running` (`Unknown` from the stock shipper, which
  ships no podindex)
- the restart, outage and put markers, each by `grep` on all containers,
  each exactly once
- the terminated pod `reused`: only its third incarnation's line,
  prefixed `[pod/reused/reused]`, phase `Succeeded` (`Unknown` from the
  stock shipper)
- an unknown pod and an unknown container are 404 problem JSON whose
  details name the pod, or the container and the known ones;
  `source=k8s` is a 400, and `/openapi.json` the spec

Not part of `test.sh` because they take the cluster down or load it:
`itest/node-restart.sh`, `itest/rotation.sh` and `itest/stress.sh` (PoC
era; to be re-run against the new blob). They still read the PoC's
parquet keys, so they find nothing in the current bucket.

The cluster is left running. `y-cluster teardown -c itest/cluster/qemu` (or
`.../docker`) removes it. While it runs, `local/blobs.sh` opens a DuckDB
session over the bucket with a view per table (`local/README.md`);
`PARQUET_KEYS_DIR=target/itest/parquet-keys` gives it the itest's keys.

## Known gaps

Design changes rather than bug fixes, so the agent lives with them until a
branch takes them on.

The agent (`cri_scrape`):

- **A pod that ends while the agent is down gets no `pod-deleted` and no
  `container-exit`.** Discovery lists ready sandboxes only, and a fresh
  tracker knows nothing, so a Job that completes during a rollout or a
  crash of the agent stays `running` in `pods` with its exit codes lost.
  This is wider than the polling gap below. A fix lists the
  not-ready sandboxes too and gives their rows deterministic times (the
  containers' `finishedAt`), so that every agent start writes identical
  rows and `DISTINCT` folds them.
- **A sandbox recreated under the same `pod_uid`.** A round that sees
  neither sandbox as ready writes `pod-deleted` for a live pod, and
  `pods` then derives a final phase for it. When both are seen in one
  round, the old facts (pod IP, `pod_started`) are kept, and the old
  sandbox's exited containers, the crash that caused the recreation, are
  not inspected. The tracker wants its sandboxes keyed by id under the
  pod.
- **Metrics follow the old `restart` for up to `discovery_interval`**
  after a container restart, so the old series shows a counter reset;
  and while a container is not running, a numeric `prometheus.io/port`
  resolves to container `""`, so its rows lose their attribution.
- **The queue cap counts records, not samples**, and podindex rows share
  it with scrapes. Under backpressure a dropped `pod-deleted` is lost for
  good, because the tracker counts it as emitted.
- `exitsEmitted` gains one id per restart for the pod's lifetime, and a
  container first seen in CREATED never gets its `container_started`.
- Exits are observed at the discovery cadence (10 s): a pod deleted within
  one interval of its exit takes its container away before the agent sees
  it, so that exit is missing.

The compactor:

- `pods.sql`'s `succeeded` ignores a container that has no exit row or a
  NULL `exit_code`, so a pod with one exit 0 and one unknown exit reads
  as succeeded. Since the agent now retries a failed status call, a NULL
  exit code means the container was removed before its status could be
  read.
- Raw deletion from the bucket (`RAW_DELETE=after-final`) is tested in
  Go, against a directory and, by hand, against versitygw, but not in the
  itest: a final hour needs an hour of wall clock past the test's start.
  Deletion from the spool is in the itest, through an object uploaded for
  an hour already past.
- Spool mode has one compactor holding the spool: its put endpoint is
  down while it restarts (shippers buffer and retry), and acknowledged
  data is as durable as its volume until folded. The itest's agent keeps
  uploading to the bucket, so the fork's own `out_s3` is not run against
  the put endpoint; stock Fluent Bit v5.1.3's is.
- The logs API serves retrieval only: no pod search, no live logs from
  the Kubernetes API, no host logs, and no authentication of its own.
  `serve` in spool mode sees no spool, so it serves a line after its
  fold.
- A raw object the engine cannot read fails its hour's fold on every
  pass, visible in `blobs_compactor_runs_total{result="failure"}`;
  nothing skips it.
- With encrypted tiers, a reader's view over one key's objects cannot
  align columns across raw1 versions (`union_by_name` on an encrypted
  read is an engine error in DuckDB 1.5), so it fails on an object older
  than a column it reads; `local/blobs.sh`'s hourly views have this gap,
  the compactor does not (`compactor/README.md`, "Encryption").

The alerting tailer, as requirements for its rewrite onto `raw1/`:

- Windows advance even when the node list is empty or a remote write
  failed, so an early S3 or DNS failure, a new node, or a remote-write
  outage longer than the grace loses whole windows for good.
- Exposed labels named like a meta column (`namespace`, `pod`,
  `container`, `job`, `instance`, `node`) give a series duplicate label
  names, which Prometheus drops while answering 2xx, and selectors match
  them against the scraping pod's namespace.
- No HTTP timeouts: a Prometheus that accepts and never answers blocks
  the tailer.
- One remote-write request per object, so one out-of-order or too-old
  sample rejects the whole object.
- No staleness markers, and regex matchers lack PromQL's `(?s)`.
