# Consuming the blobs

> PoC-era document (iteration 1, branch `alerts-cf`). Kept for the
> measurements and reasoning of the first iteration; where it differs from
> `README.md` or `compactor/README.md`, those are current.

What a reader (DuckDB today, the hourly compaction job later) can rely on,
and queries that were run against real objects from the itest cluster.

## Keys

```
/{cluster}/{node}/{YYYY-MM-DD}/{HH}/{MM}/logs-{uuid}.parquet
/{cluster}/{node}/{YYYY-MM-DD}/{HH}/{MM}/metrics-{uuid}.parquet
```

- `{MM}` is the start of the `upload_window` (1 minute by default) during
  which the agent *received* the rows. Rows carry their own `time`; a few
  may be older than the window (late reads, restarts) and, rarely, a few
  newer (received in the seconds between the boundary and the upload).
- Objects of one table and window are numbered from 0: `logs-0.parquet`,
  then `logs-1.parquet` when the window overflowed `total_file_size` or the
  agent restarted mid-window. A consumer that wants one window GETs
  sequence 0 upward until 404 and never needs a LIST; a consumer that
  wants many windows can still glob.
- Once a window's minute has passed plus ~10 seconds, the prefix is
  complete except for agent restarts, which flush whatever was buffered
  under the window in which it was buffered. A compaction job that runs at
  H+1 for hour H sees everything shipped for H.

## Schemas

Fixed by `schema` in `agent/fluent-bit.yaml`, identical in every object.

logs:

```
time       TIMESTAMP_NS   CRI timestamp (container rows) / journal realtime
cluster    VARCHAR
node       VARCHAR
namespace  VARCHAR        NULL for journal and file rows
pod        VARCHAR        NULL for journal and file rows
pod_uid    VARCHAR
container  VARCHAR
restart    SMALLINT       kubelet restart count, from the log file name
stream     VARCHAR        stdout | stderr | journal | file (host mode)
unit       VARCHAR        systemd unit (or syslog identifier) for journal rows, the path for file rows
priority   TINYINT        syslog priority for journal rows
message    VARCHAR
```

metrics:

```
time       TIMESTAMP_NS   scrape start time
cluster    VARCHAR
node       VARCHAR
scrape     VARCHAR        pod | node | host: the scrape path
instance   VARCHAR        ip:port
namespace  VARCHAR        '' for scrape=node
pod        VARCHAR
container  VARCHAR
k8s_deployment_name  VARCHAR   the pod's controllers, NULL when none of the kind
k8s_replicaset_name  VARCHAR   (README.md, "Workload names")
k8s_statefulset_name VARCHAR
k8s_daemonset_name   VARCHAR
k8s_job_name         VARCHAR
k8s_cronjob_name     VARCHAR
name       VARCHAR        metric name as exposed
type       VARCHAR        counter | gauge | histogram | summary | untyped
labels     MAP(VARCHAR, VARCHAR)
value      DOUBLE
```

podindex (append-only events; deduplicate at read time: latest row per
`pod_uid` for state, `min(time)` per (`pod_uid`, `container`, `name`) for a
metric's first appearance):

```
time        TIMESTAMP_NS   event time (container-exit: the CRI finished-at)
cluster     VARCHAR
node        VARCHAR
kind        VARCHAR        pod-observed | pod-checkpoint | pod-deleted |
                           container | container-exit | metric-first-seen
namespace   VARCHAR
pod         VARCHAR
pod_uid     VARCHAR
container   VARCHAR        container/-exit/metric rows
image       VARCHAR        container rows, as written in the pod spec
name        VARCHAR        metric-first-seen rows: the metric name
labels      MAP            pod labels, on pod-* rows
started     TIMESTAMP_NS   pod-* rows, nullable
restart     INTEGER        container rows
exit_code   INTEGER        container-exit rows, nullable
exit_reason VARCHAR        container-exit rows
```

podenrich (companion table written by the API-side enricher, not the
agent; keys use `enricher` as the node
segment; same append-only dedup-latest-per-`pod_uid` reading as podindex):

```
time            TIMESTAMP_NS   observation time (checkpoint: attestation time)
cluster         VARCHAR
kind            VARCHAR        pod-enriched | enrichment-checkpoint
namespace       VARCHAR
pod             VARCHAR
pod_uid         VARCHAR
owner_kind      VARCHAR        pod-enriched: Deployment | StatefulSet | ...
owner_name      VARCHAR
qos_class       VARCHAR
config_map_refs VARCHAR[]      names the pod spec references
secret_refs     VARCHAR[]
pods_covered    INTEGER        enrichment-checkpoint rows
```

signals: the logs columns (without `unit`/`priority`) plus

```
detection      VARCHAR   error-js | error-envoy | error-java | error-turbo | error-turbo-summary | oversized
turbo_package  VARCHAR   for error-turbo
turbo_task     VARCHAR   for error-turbo
```

## Session setup

```sql
INSTALL httpfs; LOAD httpfs;
SET s3_region='us-east-1';
SET s3_endpoint='127.0.0.1:30070';   -- versitygw NodePort in the itest cluster
SET s3_use_ssl=false; SET s3_url_style='path';
SET s3_access_key_id='demoaccess'; SET s3_secret_access_key='demosecret';

CREATE VIEW logs AS
  SELECT * FROM read_parquet('s3://node-agent/itest/*/*/*/*/logs-*.parquet');
CREATE VIEW metrics AS
  SELECT * FROM read_parquet('s3://node-agent/itest/*/*/*/*/metrics-*.parquet');
CREATE VIEW signals AS
  SELECT * FROM read_parquet('s3://node-agent/itest/*/*/*/*/signals-*.parquet');
```

Against GCS the endpoint is `storage.googleapis.com` with HMAC keys and
`s3_url_style='path'`; the prefix under the bucket is the same.

For the R2 bucket (`R2=1` runs) `itest/r2/duckdb.sh` did this setup from
`itest/r2/.env.r2`, and took a key prefix so the views cover only what
you mean to read -- the one lever that changes cost, per the table below.
It was removed with the parquet blob; `local/blobs.sh` is its successor
over the raw tier, with other views (`local/README.md`).

```
itest/r2/duckdb.sh                                      # whole bucket
itest/r2/duckdb.sh itest/<node>/2026-08-27/14/27   # one window
itest/r2/duckdb.sh itest/<node>/2026-08-27/14      # one hour
itest/r2/duckdb.sh <prefix> -c "SELECT ..."             # one query, pipeable
itest/r2/duckdb.sh <prefix> -ui                         # sidebar catalog
```

It defined the same view names, so every query below ran unchanged, plus
a `filename` column on each for tracing a row back to its object. Note the
EU jurisdiction endpoint (`<account>.eu.r2.cloudflarestorage.com`): the
default one 403s on a jurisdiction-bound bucket.

## Queries

Measured on the itest corpus after five days of agent output (23,738
objects), single node:

| query | objects touched | requests | wall time |
|---|---|---|---|
| `count(*)`, one hour glob | 60 | 201 | 9.8 s |
| same hour, `WHERE pod = ...` | 60 | 257 | 7.7 s |
| one day glob | 1,440 | 4,239 | 112 s |
| explicit list of 5 files | 5 | 39 | 1.6 s |

Predicates do not reduce requests; only touching fewer objects does. In
practice:

- Put every known bound into the key glob (date, hour, node). A `WHERE`
  on `time` alone still opens every object the glob matches.
- Prefer explicit file lists when an index knows the files:

```sql
SET VARIABLE files = (SELECT list(object) FROM manifest
                      WHERE pod = 'x' AND hour BETWEEN ...);
SELECT * FROM read_parquet(getvariable('files'));
```

- `SET enable_object_cache = true` keeps footers cached within a DuckDB
  session, which pays off for interactive, repeated queries.
- The per-minute objects are the shipping format. Sustained consumption
  belongs on the compacted hour objects; see COMPACTING.md for the job
  and the measured cost model.

Last lines of a pod, all containers interleaved:

```sql
SELECT time, container, stream, message
FROM logs
WHERE namespace = 'default' AND pod LIKE 'log-generator-%'
ORDER BY time DESC LIMIT 50;
```

What happened around a restart:

```sql
SELECT restart, min(time) AS first, max(time) AS last, count(*) AS lines
FROM logs WHERE pod = 'restarter'
GROUP BY restart ORDER BY restart;
```

Node and container logs for one minute, one timeline:

```sql
SELECT time, coalesce(pod, unit) AS source, message
FROM logs
WHERE time BETWEEN '2026-08-20 21:30:00' AND '2026-08-20 21:31:00'
ORDER BY time;
```

Which pods log the most (bytes, not lines):

```sql
SELECT namespace, pod, sum(length(message)) AS bytes, count(*) AS lines
FROM logs WHERE pod IS NOT NULL
GROUP BY 1, 2 ORDER BY bytes DESC LIMIT 10;
```

Counter rate per target over the scraped period (Prometheus `rate()` by
hand; resets are not handled here):

```sql
SELECT pod, container, labels['code'] AS code,
       (max(value) - min(value)) / epoch(max(time) - min(time)) AS per_second
FROM metrics
WHERE name = 'app_requests_total'
GROUP BY 1, 2, 3;
```

Histogram: p90 from cumulative buckets at the latest scrape:

```sql
WITH latest AS (
  SELECT * FROM metrics
  WHERE name = 'app_latency_seconds_bucket' AND pod = 'metrics-multi'
    AND time = (SELECT max(time) FROM metrics WHERE name = 'app_latency_seconds_bucket')
), total AS (
  SELECT value AS n FROM latest WHERE labels['le'] = '+Inf'
)
SELECT min(try_cast(labels['le'] AS DOUBLE)) AS p90_upper_bound
FROM latest, total
WHERE labels['le'] <> '+Inf' AND value >= 0.9 * total.n;
```

Targets that are down right now, and for how long:

```sql
SELECT namespace, pod, container, instance,
       min(time) AS down_since, count(*) AS failed_scrapes
FROM metrics
WHERE name = 'up' AND value = 0
  AND time > now() - INTERVAL 10 MINUTE
GROUP BY 1, 2, 3, 4;
```

Errors per pod in the last hour, without touching the logs table:

```sql
SELECT namespace, pod, detection, count(*) AS n, max(time) AS last
FROM signals
WHERE time > now() - INTERVAL 1 HOUR
GROUP BY 1, 2, 3 ORDER BY n DESC;
```

Node memory (node_exporter via the static target):

```sql
SELECT time, value / 1024 / 1024 AS available_mib
FROM metrics
WHERE scrape = 'node' AND name = 'node_memory_MemAvailable_bytes'
ORDER BY time DESC LIMIT 5;
```

Agent self-observation (how many records each input produced):

```sql
SELECT time, labels['name'] AS input, value
FROM metrics
WHERE name = 'fluentbit_input_records_total'
ORDER BY time DESC, input LIMIT 10;
```

Enrichment state per pod (the PoC's four-state contract; the
PoC itest asserted exactly this query against crafted rows, `test.sh`
no longer does):

```sql
WITH last AS (SELECT pod_uid, max(time) AS t FROM podindex
              WHERE kind IN ('pod-observed','pod-checkpoint','pod-deleted')
              GROUP BY pod_uid),
     ck   AS (SELECT max(time) AS t FROM podenrich
              WHERE kind = 'enrichment-checkpoint'),
     enr  AS (SELECT DISTINCT pod_uid FROM podenrich
              WHERE kind = 'pod-enriched')
SELECT pod_uid,
       CASE WHEN pod_uid IN (SELECT pod_uid FROM enr) THEN 'enriched'
            WHEN (SELECT t FROM ck) IS NULL          THEN 'unenriched'
            WHEN last.t > (SELECT t FROM ck)          THEN 'pending'
            ELSE 'gap' END AS state
FROM last;
```

## Compaction sketch

An hourly job for hour H, per cluster:

1. `read_parquet('s3://bucket/{cluster}/*/{date}/{HH}/*/logs-*.parquet')`
   (all nodes, all minutes).
2. `COPY (... ORDER BY namespace, pod, time) TO 's3://bucket-compacted/{cluster}/{date}/{HH}/logs.parquet'`
   with `ROW_GROUP_SIZE` tuned so that a pod's rows share few row groups;
   parquet column statistics then let `WHERE pod = ...` skip most of the
   file.
3. Optionally `PARTITION_BY (namespace)` when a tenant boundary matters for
   access control or cost attribution.
4. Write a small index table (namespace, pod, pod_uid, container, restart,
   first_time, last_time, lines, bytes, object) from the same scan. That is
   the table a "which pods existed, when" UI reads, and it answers the
   restart question without touching log rows.

Metrics compact the same way with `ORDER BY name, pod, time`, which is the
order the rate queries above read in.

The per-minute objects can then be deleted by a lifecycle rule (age 2 days
is enough headroom for a compaction job to be re-run).
