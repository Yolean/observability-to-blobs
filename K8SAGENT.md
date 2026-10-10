# The cluster agent: kubectl from the blobs

Exploration, branch `k8sagent`, started 2026-10-07. The node agent ships
what the node knows and never talks to the API server. The cluster agent
is a separate producer that does: one replica per cluster that ships every
object the API server serves, Events of all namespaces included, as a
fourth table of the same blobs. From those blobs alone, `kubectl get` and
`kubectl describe` can be run against the cluster as it was at any time
the blobs cover.

It is the API-side producer DESIGN.md ("Enrichment") anticipated, widened
from a pod enricher to every resource. The node agent keeps zero API
access, and the consumers that must survive the loss of a cluster still
need nothing but the node agent's tables. The apiserver table adds what
only the API server knows: owner references, eviction reasons,
ConfigMap and Secret references, Events, and the state of everything that
is not a pod.

## What it ships

`k8sagent/` is a one-replica `Recreate` Deployment of the node agent's
image, running `in_k8s_objects.so` (`images/fluentbit/plugins/k8s_objects`)
with the node agent's `out_s3`. Its keys put the producer where a node
agent's key has its node:

```
/raw1/{cluster}/{date}/{HH}/_k8sagent/{MMSS}-{uuid}.jsonl.zst
```

so the compactor's hour LIST and fold take them with no change. Every line
is `table = 'apiserver'`:

| kind | when | fields |
|---|---|---|
| `object-snapshot` | for every object, when the resource's watch has listed, and at every multiple of `snapshot_interval` (default 1h) on the wall clock | `time` (the snapshot's, the same on every row of it), `api_version`, `resource`, `namespace` (absent when cluster-scoped), `name`, `object_uid`, `resource_version`, `object` |
| `snapshot` | after the rows of one resource's snapshot | `api_version`, `resource`, `rows` (the count of its `object-snapshot` rows) |
| `object-added`, `object-modified`, `object-deleted` | a watch event after the initial list | as `object-snapshot`, `time` when it was seen |

`apijson` is the object's JSON as text. The rows' columns are in
`compactor/sql/raw.sql`, and `compactor/sql/tables.sql` has the `apiserver`
view, which adds `api_group`.

- **What is watched**: discovery's preferred version of every resource
  that can be listed and watched, re-run every `discovery_interval` (1m),
  so a CRD installed later is watched from its first snapshot. A group
  whose discovery fails (an aggregated API with its backend down) is left
  out until it answers.
- **Events**: core `v1` `events`, all namespaces. Events about
  cluster-scoped objects (Nodes, PersistentVolumes) live in namespace
  `default`, so they are included too. `events.k8s.io` is the same storage
  seen through another group, so it is excluded, as are `leases`: every
  controller and node renews one every few seconds, and nothing a person
  asks of a past cluster needs them. `exclude` (comma-separated
  `resource.group`) replaces that list.
- **managedFields** are removed from every object: they record field
  ownership, are often larger than the object, and no `kubectl get` shows
  them.
- **CRDs** ship without their schemas by default: the input's `crd_mode`
  (env `CRD_MODE` in `k8sagent/`) is `redacted`. Every version's
  `openAPIV3Schema` is replaced by a placeholder that accepts any object
  (`type: object`, `x-kubernetes-preserve-unknown-fields: true`, and a
  description saying it was removed), the conversion webhook's `caBundle`
  and the `kubectl.kubernetes.io/last-applied-configuration` annotation
  (the whole CRD again) are removed, and the CRD gets the annotation
  `observability-to-blobs/redacted: schemas`. Everything else is kept: the
  names, group, scope, every version's name, `served`, `storage`,
  deprecation, `additionalPrinterColumns`, subresources and
  `selectableFields`, the conversion strategy and webhook service, and
  the status (stored versions, accepted names, conditions). So the CRDs
  say what existed, at which versions, and an API server serves their
  custom resources from them as they were stored, with their printer
  columns: `kubectl get`, `-o wide`, `describe`, any served version (with
  conversion strategy `None`). What is lost: `kubectl explain` of a custom
  resource, and the schema's defaults and validation, which a read-only
  server does not need since the stored objects were defaulted and
  validated when written. `storage-schema` keeps the storage version's
  schema (annotation `schemas-except-storage`), `full` ships CRDs as
  served. Schemas are most of a CRD and of a snapshot: on a single-node
  cluster with 46 CRDs (gateway API, envoy-gateway, cilium, cert-manager)
  and about 1,400 objects:

  | per snapshot / per hour | `full` | `storage-schema` | `redacted` |
  |---|---|---|---|
  | the 46 CRDs, JSON | 3.47 MB | 2.95 MB | 0.08 MB |
  | the 46 CRDs, zstd | 226 KB | 217 KB | 7 KB |
  | the whole snapshot, JSON | 6.9 MB | 6.4 MB | 3.2 MB |
  | the whole snapshot, zstd (raw) | 554 KB | | 301 KB |
  | an hourly `apiserver` object | 535-584 KB | 525-574 KB | 291-340 KB |
  | the daily object, 18 hours | 1,024 KB | 1,014 KB | 803 KB |

  The storage versions' schemas are 85 % of the CRDs' bytes, so
  `storage-schema` saves almost nothing over `full`.
- **Secrets** are not watched at all by default: the input's
  `secrets_mode` (env `SECRETS_MODE` in `k8sagent/`) is `skip`, so no
  Secret value ever reaches the agent and `kubectl get secrets` from the
  blobs is empty. With `secrets_mode: scrambled` they are watched, and
  keep their type and key names, and the annotation
  `observability-to-blobs/redacted: data`. Every value is scrambled:
  `k8sobs_` and random letters and digits, a tenth of the original
  value's length in bytes, rounded up. A 10-byte password becomes
  `k8sobs_` and one character, a 1.6 KB certificate `k8sobs_` and 160. So
  a reader sees that a key had a value, and roughly how long it was, and
  nothing of the value. In `data` the scrambled value is base64-encoded
  like any value, so an API server, apiserver-at's included, reads the
  Secret. An empty value stays empty. Scrambling is fresh at every
  ingest: an unchanged Secret keeps its scrambled values while it stays
  in the agent's store, and gets new ones after a restart or a relist. Their
  `kubectl.kubernetes.io/last-applied-configuration`, which holds the
  data, is removed. This happens in the informer's transform, so the agent
  never holds Secret data, even in memory. `kubectl get secrets` still
  counts the keys. Secret values written as literals in a pod's env, or
  in ConfigMaps, are not Secrets, and ship as they are, exactly as anyone
  with `kubectl get pods` can read them.
- **A row that cannot be queued** (Fluent Bit stopped draining) is dropped,
  and a snapshot is requested, so the state is right again at the next
  snapshot row. The node agent drops the oldest metrics in the same
  situation. Here, losing a change would leave the state wrong until the
  object changed again.

RBAC is `get, list, watch` on everything (`k8sagent/rbac.yaml`), Secrets
included, since RBAC cannot grant everything but one resource. With the
default `secrets_mode: skip` the agent never uses that right. An overlay
that must not grant it lists the resources it grants instead; the agent
skips what it may not list.

## Compaction

`apiserver` is a fourth folded table:

```
/hourly/{cluster}/apiserver/{date}/{HH}.parquet
/daily/{cluster}/apiserver/{date}.parquet
```

Each is folded as podindex is: `DISTINCT` rows, one object per hour and
one per day. Rows are sorted by `(api_group, resource, namespace, name,
time)`, so an unchanged object's snapshot rows are adjacent repeats of one
string, which parquet's dictionary encoding stores once. Row groups are
5000 rows, because rows are whole objects.

## Reading: the state at a time

`local/apiserver_at.sql` is the macro `apiserver_at(t)` over any table or
view named `apiserver`: the raw view, an hourly object with the hour before
it, or a daily object with the day before it. For each resource
(`api_group`, `resource`), the base is that resource's latest complete
snapshot at or before `t`. Complete means its `snapshot` row exists. A
snapshot cut off by a crash has no `snapshot` row and is never a base.
The events after the base and up to `t` are laid over it, and for each
object the latest row wins. The rules this gives, asserted on crafted rows
in `tools/apiserver-at/asof_test.go`:

- an object deleted while the agent was down disappears at the first
  snapshot without it, the restart's
- a name deleted and created again is the new uid from its creation on
- after the server's preferred version of a resource changes, the new
  version's snapshot is the base
- a resource with no complete snapshot at or before `t` has no rows: at
  `t` it is unknown, not empty

With hourly snapshots, the hour of `t` and the two before it always hold
the base, so a read of a time is three hourly objects. Two would be
enough if rows were filed by their own time, but an object is filed by
its key's hour, the time out_s3 opened it: a full hour's snapshot rows
can sit in the object opened a few seconds before that hour. So a time
just after a full hour, whose base is the previous full hour's snapshot,
may find it two hours back.

`tools/apiserver-at` serves such a state to kubectl. It loads the objects
into a new etcd under kube-apiserver's own key layout (`registry.go`,
checked against a real API server's etcd by the acceptance test), and
starts a stock kube-apiserver on it, using setup-envtest's binaries at the
cluster's release. The API server does the serving: discovery, every
printer column of every built-in and custom resource, field and label
selectors, `describe` with its Events, `explain`, `-o wide`. A proxy in
front lets only GET and HEAD through, and refuses everything else with a
`MethodNotAllowed` Status. Of the API server's own startup work, the
pieces that would rewrite the state are turned off:

- the endpoint reconciler, which would point `default/kubernetes` at this
  process
- the priority-and-fairness bootstrap
- admission webhooks, whose services are not there

and the NodePort allocation is written with the objects, from the
Services' ports, so the API server's repair loop finds every NodePort
allocated instead of recording a `PortNotAllocated` Warning Event that
the cluster never had. Objects go into etcd in transactions within its
request limit, so CRDs with schemas of hundreds of KB load too.

APIServices of aggregated APIs are left out, so discovery does not warn
about every group whose backend is missing.

```bash
local/kubectl-at.sh itest '2026-10-07 10:12:00' get pods -A
local/kubectl-at.sh itest '2026-10-07 10:12:00' describe pod -n default reused
local/kubectl-at.sh itest '2026-10-07 10:12:00'      # serves; export the printed KUBECONFIG
```

The script reads the hourly tier, else the daily tier (`SOURCE=raw`: the
raw tier's hours that still have objects; the compactor may delete an
hour's raw objects at its final fold), and takes the API server's release from the kubelet version
the cluster's Nodes reported at that time. A state loads and serves in
about ten seconds.

The release must be the cluster's minor. The first itest ran apiserver-at
1.37 against a k3s 1.35 cluster. Its tables had columns that 1.35 does not
print: CRDs with SCOPE and VERSIONS, a Node's kernel with `(amd64)`. And
its RBAC bootstrap, which cannot be turned off, added 1.37's four new
system ClusterRoles to the served state. At 1.35 all of them matched.

What apiserver-at shows differently from the cluster at that time:

- `kube-system/extension-apiserver-authentication` holds apiserver-at's
  own client CAs; kube-apiserver writes it when it starts
- `resourceVersion` is apiserver-at's etcd revision, not the cluster's.
  The cluster's is kept in the rows
- ages are relative to now, not to the time served
- `kubectl get leases` shows only apiserver-at's own lease
- whatever a request does past the API server's storage is not there:
  `logs`, `exec`, `port-forward`, `top`, the proxy subresources, and
  aggregated APIs. Neither are reads of a custom resource at a version
  that needs a conversion webhook

## Tests

| test | what |
|---|---|
| `kobjects` units (fake clients) | snapshot rows then the snapshot row, no initial-list events, changes in all namespaces, a requested snapshot, Secret values scrambled on every row (empty ones kept empty), Secrets not watched unless secrets_mode is scrambled, CRD schemas, caBundle and applied manifest removed in each crd_mode and nothing else changed, CRD rows redacted by default, discovery filtering, snapshot alignment |
| `tools/apiserver-at` `TestLoadLargeObjects` | objects of 1.2 MiB among small ones load into etcd |
| `tools/apiserver-at` `TestObjectsAt` | `apiserver_at` over crafted raw rows, through `raw.sql` and `tables.sql` |
| `tools/apiserver-at` `TestAcceptance` | a real API server as the source. The watcher's rows go through the SQL to apiserver-at. Every object of the 62 resources is equal at a past time and at the present, the CRD with its schemas redacted. At the present, the server-side Tables of all of them are equal, as is `kubectl describe` of a pod, a node, a deployment and a custom resource, and `kubectl get` of the custom resources at the version that is not stored. The source's etcd keys are the keys apiserver-at writes. A write is refused. A NodePort Service gets no `PortNotAllocated` Event. About 40 s, part of `./test.sh checks` |
| `./test.sh` itest | the cluster agent in k3s, through out_s3 and the compactor: its keys and lines, built-in and custom resources snapshotted at the scheduled interval, snapshot counts, no managedFields, Secret values scrambled to a tenth of their length and no Secret data anywhere, Events of all namespaces including one about the Node, three pod incarnations, the apiserver tier equal to raw hourly and daily, CRDs without schemas (a two-version CRD with 300 KB of schemas in under 4 KB, printer columns kept). Then kubectl through apiserver-at: a ConfigMap before its creation, at each of its two values, after its deletion, and seventeen `kubectl get`/`describe` outputs equal to the live cluster's, three of them of that CRD's custom resources |

## Measured

The itest cluster, a single k3s 1.35 node with about 650 objects, in 33
minutes of a run with `SNAPSHOT_INTERVAL` 2m, which is 30 times the
default:

| | rows | bytes |
|---|---|---|
| raw, the cluster agent's 36 objects | 12,421 snapshot rows, 644 change rows, 1,121 snapshot rows | 13.5 MB of object JSON, 2.7 MB zstd |
| raw, the node agent's 65 objects, for comparison | | 1.9 MB zstd |
| hourly `apiserver` (3 hours) | the same | 0.32 MB parquet, about the size of `logs` |
| daily `apiserver` | | 0.16 MB |

The agent used 36 MiB and 4m of CPU while it watched 59 resources. A
snapshot of the whole cluster is about 0.7 MB of JSON. At the default 1h
interval, a cluster of this size costs well under a megabyte a day in the
daily tier.

## Open

- **Size of the daily tier**: it keeps every hourly snapshot. Keeping only
  the day's first snapshot and the snapshots after a gap would shrink it.
  Wait for real cluster measurements first.
- **Snapshot-only resources**: leases could be listed at snapshot time
  without being watched, if a use for them shows up.
- **Large clusters**: the informers hold the whole cluster in memory, as
  any controller's cache does. The streaming list (`WatchList`) would
  lower the startup peak.
- **`kubectl logs`** could be served by apiserver-at's proxy from the logs
  API (compactor/README.md, "The logs API"), which already answers by
  namespace, pod and container.
- **Data handling**: the bucket now holds every ConfigMap and the env of every pod,
  as the API server does. Secret values are scrambled, but values that
  workloads put elsewhere are not.
- **Backpressure**: while Fluent Bit does not drain, every dropped row
  asks for a snapshot, and a snapshot's rows can be dropped too. That
  holds the state right at the cost of repeated full passes over the
  stores. A backoff between the requested snapshots would make it cheaper.
- **Snapshot size**: one resource's snapshot is one callback's data, a
  single chunk. At about 0.7 MB for the whole itest cluster that is
  nothing, but a cluster with tens of thousands of pods would want the
  snapshot split across callbacks, with the snapshot row last.
- **Pin**: the compactor's code moved, so `compactor/build.sh bump` is due
  before this merges (CLAUDE.md). It builds both platforms with docker,
  which the host this was explored on does not have.
