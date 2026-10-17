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
API; the alerting tailer (`images/blobs-to-prometheus`) feeds a
Prometheus from the raw tier. The PoC-era `CONSUMING.md` and `COMPACTING.md` keep measurements
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
  hints (multi-container pods opt in per port; "Scrape annotations"
  below), plus node_exporter and the
  agent's own metrics, with the samples as an array and the target's
  `pod_uid`. A target's `metric_relabel_configs` can drop samples and
  labels before they are written ("Metric relabeling" below).
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

The cluster agent (exploration, [K8SAGENT.md](K8SAGENT.md)) is the one
producer that does talk to the API server: one replica per cluster that
ships a fourth table, `apiserver`, with every object and Event under the
producer segment `_k8sagent`, so that `local/kubectl-at.sh` can run
kubectl against the cluster as it was at a time, from the blobs alone.

## Scrape annotations

The agent scrapes a pod when the pod's annotations ask for it, in either
of two conventions. It reads them from the container runtime, which gets
every pod annotation from the kubelet when it creates the pod's sandbox,
and never from the Kubernetes API. Both conventions share these rules:

- A target is the pod IP and a port; a `hostNetwork` pod's is the node IP.
- The **declared ports** are the TCP `containerPort`s of the pod's running
  containers.
- Every target is attributed to the container that declares its port. A
  port that no running container declares is still scraped, with
  container `""`.
- Every target is scraped every 15 s with a 10 s timeout, the agent's
  `scrape_interval` and `scrape_timeout` (`agent/fluent-bit.yaml`), unless
  a `collection_interval` below says otherwise.
- A boolean is true when it is `true`, `1` or `yes`, in any case, and
  false otherwise.
- When both conventions name the same port, the OpenTelemetry one wins
  for that port, in full: no field of the `prometheus.io/*` target is
  kept.

### `prometheus.io/*`

| annotation | value | when absent |
|---|---|---|
| `prometheus.io/scrape` | boolean; true opts the pod in | not scraped |
| `prometheus.io/port` | a port number, or a `containerPort` name: the first container, in name order, that declares it | every declared port |
| `prometheus.io/path` | the path of every target of the pod | `/metrics` |
| `prometheus.io/scheme` | `http` or `https`, for every target of the pod | `http` |

A port name that no running container declares gives no target.
`https` verifies the server's certificate against the image's trusted
roots, with no TLS settings of its own.

### `io.opentelemetry.discovery.metrics[.<port>]/*`

| key | value |
|---|---|
| `…/enabled` | boolean; true opts in |
| `…/scraper` | `prometheus` (also when absent) or `prometheus_simple`; any other value skips the port, because it names a protocol other than Prometheus exposition |
| `…/config` | YAML, of which only three fields are read: `metrics_path` (default `/metrics`), `collection_interval`, a Go duration such as `30s` (default the agent's `scrape_interval`; an invalid value is ignored), and `metric_relabel_configs` ("Metric relabeling" below) |

The **pod-level** keys, `io.opentelemetry.discovery.metrics/*`, enable every
declared port. The **port-scoped** keys,
`io.opentelemetry.discovery.metrics.<port>/*`, where `<port>` is a
`containerPort` number or name, enable that one port. A port-scoped
number that no container declares is scraped with container `""`; a name
that no container declares gives no target, and a name that several
containers declare scrapes the first of them, in name order, only. (The
PodMonitor webhook, `operator-to-annotations/`, resolves a name to every
container that declares it and writes a key per port number, so its pods
do not meet this.) For one port, a port-scoped
field wins over the pod-level one, field by field, and a key scoped by
number wins over one scoped by the port's name. A port-scoped
`enabled: "false"` therefore excludes a port that the pod-level keys
enable.

Multi-container pods use the port-scoped form: each container's port gets
its own `enabled`, path and interval, and each target is attributed to its
own container.

The keys follow the hints of the OpenTelemetry Collector's receiver creator
([`receiver/receivercreator/README.md`, "Generate receiver configurations from provided Hints", at v0.162.0](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/v0.162.0/receiver/receivercreator/README.md#generate-receiver-configurations-from-provided-hints)),
the only specification of them; no implementation-neutral one exists.
This agent reads a subset of them, and differs from that reference where it says so:

- There, `scraper` names any receiver and `config` is that receiver's
  configuration. Here the only scraper is Prometheus exposition, and
  `config` holds what `prometheus_simple` calls `metrics_path` and
  `collection_interval`, and the `metric_relabel_configs` of a
  Prometheus scrape config, which the `prometheus` receiver embeds. Every
  other field is ignored, `endpoint` included: the target is always the
  pod's address and the port.
- There, `<port>` is a port number. Here it may also be a port's name.
- The `io.opentelemetry.discovery.logs/*` keys are not read (below).

### Metric relabeling

A target's `metric_relabel_configs` are applied to every sample of each
scrape before it is written, as Prometheus applies a scrape config's
([`<metric_relabel_configs>`](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#metric_relabel_configs)):
the same format and actions, by Prometheus's own code
(`model/relabel`). Regexes are anchored at both ends, and the metric
name is the label `__name__`. For example, to keep only the series that
an Envoy sidecar's alerts read:

```yaml
io.opentelemetry.discovery.metrics.9901/enabled: "true"
io.opentelemetry.discovery.metrics.9901/config: |
  metrics_path: /stats/prometheus
  metric_relabel_configs:
  - source_labels: [__name__]
    regex: envoy_cluster_upstream_rq(_xx|_timeout)?|envoy_cluster_upstream_cx_connect_timeout|envoy_listener_downstream_cx_(active|overflow)
    action: keep
```

- **A list is one field.** A port-scoped `metric_relabel_configs`
  replaces the pod-level list, and an empty list (`[]`) turns the pod's
  rules off for that port.
- **The rules see the sample's own labels**, and `__name__`. In
  Prometheus they also see the target's labels (`job`, `instance` and
  those that target relabeling adds); here the target's identity is in
  the row's columns, out of the rules' reach.
- **A sample is dropped** when a rule drops it, when its name ends up
  empty, or when its labels end up equal to an earlier sample's in the
  same scrape (a `labeldrop` can do that), which Prometheus rejects too. A
  label with an empty value is no label, as in Prometheus.
- **The scrape's own series are not relabeled**: `up`,
  `scrape_duration_seconds`, `scrape_samples_scraped` (the samples the
  target served) and `scrape_samples_post_metric_relabeling` (those
  written), as in Prometheus. Comparing the last two shows what a
  target's rules drop.
- **A list that does not parse or validate is not applied**: the target is
  scraped, every sample is shipped, and the agent logs why when it adds
  the target. A filter that is wrong costs volume, never an alert input.
- **A rule can rewrite as well as drop.** `replace`, `labelmap` and the
  other actions do what they do in Prometheus, so the rows hold what the
  rules made of the samples, not what the pod served.

`prometheus.io/*` has no counterpart: a pod that needs rules uses the
OpenTelemetry form.

A pod that a PodMonitor selects gets its rules from the PodMonitor's
`metricRelabelings`, which operator-to-annotations translates. Do not
annotate such a pod yourself: the webhook leaves a pod with any
`io.opentelemetry.discovery.*` or `prometheus.io/*` annotation alone,
every port included, so one port annotated by hand turns off the
PodMonitor's other ports for that pod.

### What the annotations cannot do

By design, and so permanently:

- **Annotations are read when the sandbox is created.** Annotating a
  running pod changes nothing until the pod is recreated: the runtime
  never sees the change, and the agent has no access to the Kubernetes
  API.
- **`prometheus.io/*` describes one path and one scheme per pod.** Two
  ports with different paths need the OpenTelemetry form.
- **A port that no container declares has no container.** Its samples
  carry container `""`; declare the port to attribute them.
- **A container that is not running declares no ports.** While it
  restarts, a numeric `prometheus.io/port` or port-scoped key resolves to
  container `""`, and a port name, or a pod that names no port, gives no
  target for it (Known gaps).
- **Logs ship for every container**, so there is nothing for
  `io.opentelemetry.discovery.logs/enabled` to enable, and its `config`
  (`file_log` operators) has no counterpart in the agent's pipeline.

Not built: a scrape timeout or TLS settings per target, in either form;
and in the OpenTelemetry form a `scheme` (it is always `http`), auth, URL
parameters, target relabeling (`relabel_configs`) and `honor_labels`.

## Layout

| path | what |
|---|---|
| `images/fluentbit/` | Dockerfile (Fluent Bit v5.1.2, one patch), the `cri_scrape` Go input plugin, `UPSTREAM.md` with findings to report |
| `images/fluentbit/host/` | host mode: the pipeline, heartbeat script and systemd unit, and `build.sh`, which builds them with the same Fluent Bit into the host tarball ([HOST_MODE.md](HOST_MODE.md)) |
| `agent/` | kustomize base: DaemonSet, `fluent-bit.yaml` pipeline (stock processors, no Lua), the `pipeline-extra.yaml` composition hook |
| `k8sagent/`, `images/fluentbit/plugins/k8s_objects/` | the cluster agent: a one-replica Deployment of the same image with the `k8s_objects` Go input plugin ([K8SAGENT.md](K8SAGENT.md)) |
| `tools/y-obs/`, `local/apiserver_at.sql`, `local/kubectl-at.sh` | a cluster at a time: the objects state SQL, and a stock kube-apiserver serving it read-only to kubectl |
| `itest/` | y-cluster configs for a local k3s node (qemu and docker), versitygw, the workloads that exercise each path, the compactor's overlays (bucket mode, and spool mode with stock Fluent Bit as a second shipper); `itest/host/` the host phase |
| `images/blobs-to-prometheus/`, `tools/rules-manifest/`, `local/`, `alerts-cf/` | the alerting consumer: the tailer from `raw1/` into a Prometheus remote-write receiver, the selectors manifest of a rule set, the local stacks and the Cloudflare container |
| `compactor/` | `blobs-compactor` (fold, rollup and the loop that runs them, and the logs API), its design (`README.md`), its SQL (`sql/raw.sql` is the raw line contract every consumer reads), its build script and `deploy/` |
| `operator-to-annotations/`, `images/operator-to-annotations/` | for clusters whose manifests carry prometheus-operator resources but that run no operator: `crds/`, the five common prometheus-operator CRDs at `v1` only, vendored as released, and `controller/`, a mutating webhook that translates PodMonitors into the port-scoped annotations ([its README](operator-to-annotations/README.md)) |
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

A docker daemon elsewhere works too. With
[y-cluster dockerhost](https://github.com/Yolean/y-cluster) (dockerd and
BuildKit in a KVM guest), `eval "$(y-cluster dockerhost env)"` sets
`BUILDKIT_HOST` and `BUILDKIT_TLS_DIR`, so `test.sh` builds through the
guest's BuildKit, and `DOCKER_HOST`, under which `compactor/build.sh`
copies the repository into a volume instead of mounting it (a remote
daemon shares no files), so `build.sh check` and `bump` run there for both
platforms after one `build.sh host-setup` on the guest.
`SKIP_BUILD=1 ./test.sh itest` reuses the last images for a quick loop
against the running cluster.

**Run the tests before every push**, in this repository and in forks
alike: `./test.sh` for any change to code, manifests, `test.sh` or
`itest/`, and at least `./test.sh checks` for documentation only. Say in
the pull request what you ran and its result. CI runs on pushes and pull
requests in the upstream repository only; in a fork it runs only when
started by hand (Actions, "ci", "Run workflow"), so a push there is not
tested unless you tested it.

## kubectl against a cluster as it was

From any machine with go, duckdb and kubectl, against a production bucket.
The SQL lives in the repository, so a shallow clone of main is the install:

```bash
git clone --depth 1 https://github.com/Yolean/observability-to-blobs
cd observability-to-blobs

export S3_ENDPOINT=<host[:port]> S3_TLS=on S3_REGION=<region> S3_BUCKET=<bucket>
export AWS_ACCESS_KEY_ID=<key id> AWS_SECRET_ACCESS_KEY=<secret>
export PARQUET_KEYS_DIR=<dir>   # only if the compactor encrypts: one file per key id, as the compactor's

local/kubectl-at.sh <cluster> '2026-10-07 10:12:00' get pods -A   # the time is UTC
local/kubectl-at.sh <cluster> '2026-10-07 10:12:00'               # serve until ^C; export the printed KUBECONFIG
```

The credentials need read access only: GET and LIST under `hourly/` and
`daily/` (and `raw1/` with `SOURCE=raw`). They reach DuckDB through a secret
in a private temporary directory, never the command line. `y-obs apiserver-at`
itself takes no credentials: it serves a state file on 127.0.0.1 and writes
a kubeconfig for that endpoint. It runs with `go run`, without a build step:

```bash
go -C tools/y-obs run . apiserver-at -objects state.jsonl -kubeconfig at.kubeconfig
```

where `state.jsonl` is `local/apiserver_at.sql`'s rows for a time, and
`KUBEBUILDER_ASSETS` names etcd and kube-apiserver of the cluster's release
(`local/kubectl-at.sh` does both). See [K8SAGENT.md](K8SAGENT.md), "Reading".

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

The alerting tailer (`images/blobs-to-prometheus`, reading `raw1/`):

- It writes no staleness markers: a series that ends (a pod deleted, a
  target gone) stays in instant queries for Prometheus' lookback delta,
  5 minutes, after its last sample.
- What it can backfill is what the raw tier holds: with the compactor's
  RAW_DELETE=after-final, one to two hours. A Prometheus that needs a
  longer history after losing its volume (a rule over a day's average)
  reads it nowhere; the hourly tier would have to be a second source.
- A key that appears after its hour has left LOOKBACK (an agent's
  backlog after a longer outage) is never read.
- A series Prometheus refuses (out of order, too old) is lost; the
  tailer counts it in `blobs_to_prometheus_dropped_series_total`.
- In spool mode (`RAW_SOURCE=spool`, `compactor/deploy/spool`) `raw1/`
  never reaches the bucket, so the tailer has nothing to read there. Such a
  deployment's alerting reads the `hourly/` tier, which the rolling pass
  rewrites every `ROLLING_INTERVAL`: samples arrive that much later (plus
  the upload interval), which rules' ranges and `for:` have to allow for,
  and the reader needs the Parquet keys.
- The compactor's own label `job` (rolling, past, rollup) collides with the
  meta label `job`; the tailer stores it as `exported_job`, as a scrape with
  `honor_labels: false` does, so the rule its README names selects
  `blobs_compactor_last_success_timestamp_seconds{exported_job="rolling"}`.
