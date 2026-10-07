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
need nothing but the node agent's tables. The objects table adds what
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
is `table = 'objects'`:

| kind | when | fields |
|---|---|---|
| `object-snapshot` | for every object, when the resource's watch has listed, and at every multiple of `snapshot_interval` (default 1h) on the wall clock | `time` (the snapshot's, the same on every row of it), `api_version`, `resource`, `namespace` (absent when cluster-scoped), `name`, `object_uid`, `resource_version`, `object` |
| `snapshot` | after the rows of one resource's snapshot | `api_version`, `resource`, `objects` (the row count) |
| `object-added`, `object-modified`, `object-deleted` | a watch event after the initial list | as `object-snapshot`, `time` when it was seen |

`object` is the object's JSON as text. The rows' columns are in
`compactor/sql/raw.sql`, and `compactor/sql/tables.sql` has the `objects`
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
- **Secrets** keep their type and key names, with every value empty and
  the annotation `observability-to-blobs/redacted: data`. Their
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

RBAC is `get, list, watch` on everything (`k8sagent/rbac.yaml`). An overlay
that must not grant reading Secrets lists the resources it grants instead.
The agent skips what it may not list, and `kubectl get secrets` from the
blobs is then empty.

## Compaction

`objects` is a fourth folded table:

```
/hourly/{cluster}/objects/{date}/{HH}.parquet
/daily/{cluster}/objects/{date}.parquet
```

Each is folded as podindex is: `DISTINCT` rows, one object per hour and
one per day. Rows are sorted by `(api_group, resource, namespace, name,
time)`, so an unchanged object's snapshot rows are adjacent repeats of one
string, which parquet's dictionary encoding stores once. Row groups are
5000 rows, because rows are whole objects.

## Reading: the state at a time

`local/objects_at.sql` is the macro `objects_at(t)` over any table or
view named `objects`: the raw view, an hourly object with the hour before
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

With hourly snapshots, the hour of `t` and the hour before it always hold
the base, so a read of a time is two hourly objects.

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

APIServices of aggregated APIs are left out, so discovery does not warn
about every group whose backend is missing.

```bash
local/kubectl-at.sh itest '2026-10-07 10:12:00' get pods -A
local/kubectl-at.sh itest '2026-10-07 10:12:00' describe pod -n default reused
local/kubectl-at.sh itest '2026-10-07 10:12:00'      # serves; export the printed KUBECONFIG
```

The script reads the hourly tier, else the daily tier (`SOURCE=raw`: the
raw tier), and takes the API server's release from the kubelet version
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
| `kobjects` units (fake clients) | snapshot rows then the snapshot row, no initial-list events, changes in all namespaces, a requested snapshot, Secret redaction on every row, discovery filtering, snapshot alignment |
| `tools/apiserver-at` `TestObjectsAt` | `objects_at` over crafted raw rows, through `raw.sql` and `tables.sql` |
| `tools/apiserver-at` `TestAcceptance` | a real API server as the source. The watcher's rows go through the SQL to apiserver-at. Every object of the 62 resources is equal at a past time and at the present. At the present, the server-side Tables of all of them are equal, as is `kubectl describe` of a pod, a node and a deployment. The source's etcd keys are the keys apiserver-at writes. A write is refused. About 40 s, part of `./test.sh checks` |
| `./test.sh` itest | the cluster agent in k3s, through out_s3 and the compactor: its keys and lines, built-in and custom resources snapshotted at the scheduled interval, snapshot counts, no managedFields, no Secret data anywhere, Events of all namespaces including one about the Node, three pod incarnations, the objects tier equal to raw hourly and daily. Then kubectl through apiserver-at: a ConfigMap before its creation, at each of its two values, after its deletion, and fourteen `kubectl get`/`describe` outputs equal to the live cluster's |

## Measured

The itest cluster, a single k3s 1.35 node with about 650 objects, in 33
minutes of a run with `SNAPSHOT_INTERVAL` 2m, which is 30 times the
default:

| | rows | bytes |
|---|---|---|
| raw, the cluster agent's 36 objects | 12,421 snapshot rows, 644 change rows, 1,121 snapshot rows | 13.5 MB of object JSON, 2.7 MB zstd |
| raw, the node agent's 65 objects, for comparison | | 1.9 MB zstd |
| hourly `objects` (3 hours) | the same | 0.32 MB parquet, about the size of `logs` |
| daily `objects` | | 0.16 MB |

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
  as the API server does. Secrets are redacted, but values that
  workloads put elsewhere are not.
- **Pin**: the compactor's code moved, so `compactor/build.sh bump` is due
  before this merges (CLAUDE.md).
