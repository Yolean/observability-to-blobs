# Compacting the blobs

> PoC-era document (iteration 1, branch `alerts-cf`). Kept for the
> measurements and reasoning of the first iteration; where it differs from
> `README.md` or `compactor/README.md`, those are current.

Design for the hourly job that turns the agent's per-minute objects into a
query-shaped lake. Not built yet; this document exists so its cost can be
budgeted next to the shipping and query cost. Measurements come from the
itest cluster after five days of continuous agent output (23,738 objects,
versitygw on the local VM), prices are GCS (class A $0.005/1000, class B
$0.0004/1000).

## Why the per-minute objects cannot be the query format

DuckDB spends 3 to 4 requests per parquet object it touches (HEAD, footer,
row groups), regardless of object size or predicate selectivity. Measured
on the five-day corpus, single node:

| query | objects | requests | wall time |
|---|---|---|---|
| `count(*)`, one hour glob | 60 | 201 | 9.8 s |
| same hour, `WHERE pod = ...` | 60 | 257 | 7.7 s |
| one day glob | 1,440 | 4,239 | 112 s |
| explicit list of 5 files | 5 | 39 | 1.6 s |

Two lessons. Predicates do not reduce requests: the footer has to be
fetched before statistics can exclude anything, and for minute-sized
objects the footer is most of the object. Key-prefix narrowing and
explicit file lists are the only levers that reduce requests, which is
what "use the partitioning to subset what blobs we fetch metadata for"
has to mean mechanically.

The stated scenario, uncompacted: 10 developers x 100 log retrievals per
day against a 10-node cluster, a typical retrieval being one pod over 6
hours. That touches 10 nodes x 360 minutes = 3,600 objects, ~12,000
requests, minutes of wall time per query; 1,000 such queries are ~12M
requests/day, about $5/day in class B operations and unusable latency.
Compaction is not an optimization here, it is what makes the lake usable.

## Shape of the compacted lake

```
compacted/{cluster}/{date}/{HH}/logs/{namespace}.parquet
compacted/{cluster}/{date}/{HH}/metrics/{namespace}.parquet
compacted/{cluster}/{date}/{HH}/manifest.parquet
```

- One object per namespace per hour per table. namespace is the tenant
  boundary: it subsets by key prefix, and it is the unit for access
  control or cost attribution later.
- logs sorted by (pod, time) inside the file, row groups ~100k rows, so
  parquet statistics answer `WHERE pod = X` by skipping row groups within
  the one object a query opens.
- metrics sorted by (name, pod, time), the order rate queries read in.
- The manifest is the index the previous datalake never had: one row per
  (namespace, pod, pod_uid, container, restart) with first/last time,
  lines, bytes, node and the file it landed in. It serves pod
  autocomplete, "which pods existed when", the restart timeline, and
  query planning: read the manifest first (1 object), then open exactly
  the files it names, as an explicit list.

Read cost after compaction, same scenario: 6 hourly manifests + 6
namespace files = 12 objects, ~40 requests, sub-second. 1,000 queries/day
= ~40k requests = under $0.02/day. That is ~300x fewer requests than
uncompacted.

## The job

One process per cluster (a CronJob, or a loop in the consumer backend; it
only needs DuckDB and the bucket credentials):

1. Nodes: LIST `{cluster}/` with delimiter (1 request).
2. Hour H: with the predictable keys ($SEQ, the PoC's patch 0006, since
   removed) the 60 windows
   are enumerated by GETs from sequence 0 until 404, no LIST needed; one
   LIST per node prefix (1 request per 1000 keys) still works in bulk.
3. `read_parquet([explicit list])` for each table, `SELECT DISTINCT`:
   upload retries after an unacknowledged success are the one at-least-
   once edge in shipping, and duplicate objects contain identical rows,
   so DISTINCT restores exactly-once.
4. `COPY (... ORDER BY pod, time) TO ... PARTITION_BY (namespace)` with
   ZSTD; then the manifest from the same scan.
5. Idempotent by construction: re-running hour H rewrites the same keys
   deterministically. No coordination, no state outside the bucket.
6. Per-minute objects are deleted by a bucket lifecycle rule at age 2
   days, leaving a re-run window. The compactor never deletes.

The window key contract makes hour H complete once H+1 has begun plus the
upload grace (~10 s): everything the agents shipped for H sits under H's
prefixes. An agent that was down ships nothing; its data does not arrive
late under old keys, because keys carry the shipping window, not record
time. The only late writes are store-recovery uploads after a node
restart, which land under the recovery minute's key; a re-run of the
affected hour (or accepting that those rows live in the next hour's
compacted files) covers it.

The podindex table folds smallest of all: its rows are events, so the
hourly pass is the same DISTINCT concatenation with `ORDER BY pod_uid,
time` and no partitioning (a day of a 50-node cluster is a few MB). The
read-time dedup semantics (latest row per pod_uid, min(time) per metric
name) stay with the consumer in both tiers, and that per-name min(time)
over compacted hours is the metric catalog the y-prometheus autocomplete
story reads.

## The hot hour

The current hour is per-minute objects only, and it is what "tail this
pod" hits. Options, in the order they should be tried:

1. Accept it with narrow prefixes: a pod-now query over the last 10
   minutes of a 10-node cluster is <= 100 objects, ~350 requests, a few
   seconds. With `SET enable_object_cache = true` a session's repeated
   queries stop re-fetching footers.
2. Rolling compaction: run the same job for the current hour every 5
   minutes, overwriting `compacted/.../{HH}/`. Consumers then never read
   minute objects at all. Extra cost: the hour's inputs are re-read ~6.5x
   on average (sum of 12 growing re-reads).
3. If even 5 minutes is too stale for the tail case, that is the point
   where a serving layer (an existing logs API reading minute objects
   with a cache, as today) beats more compaction.

Recommendation: build 2 from the start; it is the same code as the hourly
job with a different prefix and schedule.

## Compactor cost, 50-node cluster, 2 tables

- Hourly pass: 50 x 60 x 2 = 6,000 objects read, ~20k class B = $0.008;
  writes ~2 x namespaces + 1 class A, negligible. Per day ~$0.20.
- Rolling pass on top: ~6.5x the hour's reads = ~130k class B per hour,
  $0.05/hour, ~$1.25/day.
- Compute: one pod. An hour of a 50-node cluster at the itest's ~1 MB
  parquet per node-hour is ~50 MB in, well under a GiB of working set;
  size for bursts (a stress-level hour is ~60x that) with 4 GiB and
  DuckDB's spill-to-disk on ORDER BY.
- Total: order of $1.5/day per 50-node cluster, against ~$5/day saved on
  the developer-query scenario alone, before counting latency and before
  the lake has any non-developer readers.

Egress is zero when the job runs in the bucket's region. The numbers
scale linearly in nodes and tables and not at all in bytes, which is the
point: request count, not volume, is the cost driver at this shape.

## What this repo owns

The agent's contract with the compactor is the key layout, the window
completeness rule, and the fixed schemas; those are asserted by test.sh
today. The compactor itself belongs with the consumer side,
like the signals detections belong with production config. What could
move here later is a reference implementation of the hourly pass as a
DuckDB script, exercised in the itest the way CONSUMING.md queries are.
