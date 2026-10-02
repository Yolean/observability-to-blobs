#!/usr/bin/env bash
# Compacts one hour of raw blobs into the hourly tier (compactor/README.md):
#
#   compact.sh <cluster> <YYYY-MM-DD> <HH>
#
# Reads  <IN>/raw1/<cluster>/<date>/<HH>/*/*.jsonl.zst
# Writes <OUT>/hourly/<cluster>/logs/<date>/<HH>.parquet
#        <OUT>/hourly/<cluster>/metrics/<date>/<HH>.parquet
#        <OUT>/hourly/<cluster>/podindex/<date>/<HH>.parquet
#        <OUT>/hourly/<cluster>/pods/<date>/<HH>.parquet
# (one object per table per hour, namespace-sorted, 20k-row row groups:
# compactor/README.md amendment 1; the daily tier partitions by namespace)
#
# Stateless and idempotent: the output keys are a function of the
# arguments, a re-run overwrites. IN and OUT default to the same bucket
# (s3://$S3_BUCKET); either can be a local directory for tests.
#
# Env: S3_BUCKET S3_ENDPOINT (host:port) S3_TLS (on|off) S3_REGION
#      AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY, or IN and OUT as paths.
set -euo pipefail
DIR=$(cd "$(dirname "$0")" && pwd)
CLUSTER=$1; DATE=$2; HH=$3
IN=${IN:-s3://$S3_BUCKET}
OUT=${OUT:-s3://$S3_BUCKET}
SRC="$IN/raw1/$CLUSTER/$DATE/$HH/*/*.jsonl.zst"
case "$OUT" in (s3://*) ;; (*)
  for t in logs metrics podindex pods; do mkdir -p "$OUT/hourly/$CLUSTER/$t/$DATE"; done;;
esac

S3_SETUP=""
case "$IN$OUT" in (*s3://*)
  S3_SETUP="INSTALL httpfs; LOAD httpfs;
    SET s3_region='${S3_REGION:-us-east-1}'; SET s3_endpoint='${S3_ENDPOINT:?}';
    SET s3_use_ssl=$([ "${S3_TLS:-on}" = on ] && echo true || echo false); SET s3_url_style='path';
    SET s3_access_key_id='${AWS_ACCESS_KEY_ID:?}'; SET s3_secret_access_key='${AWS_SECRET_ACCESS_KEY:?}';"
esac

# -bail: the first failed statement ends the session, nonzero. Without it
# the CLI runs on, and a COPY after a failed CREATE TEMP TABLE writes the
# view's rows, not deduplicated.
duckdb -bail <<SQL
$S3_SETUP
SET TimeZone='UTC';
SET preserve_insertion_order = false;

-- The contract and the row shapes, the same files local/blobs.sh and the
-- itest read (sql/raw.sql, sql/tables.sql, sql/pods.sql).
SET VARIABLE raw_files = '$SRC';
.read $DIR/sql/raw.sql
.read $DIR/sql/tables.sql
.read $DIR/sql/pods.sql

-- The hour's objects, read once: from here on a temp table named raw
-- takes precedence over the view (sql/tables.sql), so the table views read
-- it instead of the bucket. Each CREATE TEMP TABLE <name> below still reads
-- the view <name>, because the temp table exists only once it completes.
CREATE TEMP TABLE raw AS SELECT * FROM raw;

-- The tier's rows. DISTINCT folds the at-least-once edge of an upload
-- retried after an unacknowledged success. The temp tables take the views'
-- names, so pods is computed from the deduplicated rows.
CREATE TEMP TABLE logs AS SELECT DISTINCT * EXCLUDE (filename) FROM logs;
CREATE TEMP TABLE metrics AS SELECT DISTINCT * EXCLUDE (filename) FROM metrics;
CREATE TEMP TABLE podindex AS SELECT DISTINCT * EXCLUDE (filename) FROM podindex;

-- logs: namespace first, so row-group statistics prune "pod X in hour H"
-- to a few row groups of the one object
COPY (SELECT * FROM logs ORDER BY namespace, pod, pod_uid, container, restart, time)
  TO '$OUT/hourly/$CLUSTER/logs/$DATE/$HH.parquet'
  (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 20000);

-- metrics: one row per sample, sorted the way rate queries read
COPY (SELECT * FROM metrics ORDER BY namespace, name, pod, pod_uid, container, restart, time)
  TO '$OUT/hourly/$CLUSTER/metrics/$DATE/$HH.parquet'
  (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 20000);

-- podindex: the events pods is derived from, kept so that a later fold of
-- this hour, and the rollup, can recompute pods rather than merge it
COPY (SELECT * FROM podindex ORDER BY pod_uid, time)
  TO '$OUT/hourly/$CLUSTER/podindex/$DATE/$HH.parquet' (FORMAT parquet, COMPRESSION zstd);

COPY (SELECT * FROM pods ORDER BY namespace, pod, pod_started, pod_uid, container, restart)
  TO '$OUT/hourly/$CLUSTER/pods/$DATE/$HH.parquet' (FORMAT parquet, COMPRESSION zstd);

SELECT 'raw lines' AS what, count(*) AS n FROM raw
UNION ALL SELECT 'logs rows', count(*) FROM logs
UNION ALL SELECT 'metric samples', count(*) FROM metrics
UNION ALL SELECT 'podindex events', count(*) FROM podindex
UNION ALL SELECT 'pods rows', count(*) FROM pods;
SQL
