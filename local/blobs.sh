#!/usr/bin/env bash
# A DuckDB session over the bucket, with a view per table. Defaults to the
# itest cluster's versitygw on NodePort 30070; export S3_ENDPOINT, S3_TLS,
# S3_REGION, AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for any other
# bucket (for R2: the EU jurisdiction endpoint, REGION auto). For a bucket
# whose tiers are encrypted, export PARQUET_KEYS_DIR, a directory of key
# files as the compactor mounts it (compactor/README.md, "Encryption").
#
#   local/blobs.sh                                    # interactive, whole cluster
#   local/blobs.sh itest/2026-09-22/14                # raw views scoped to one hour
#   local/blobs.sh itest/2026-09-22/14/<node>         # ... of one node
#   local/blobs.sh -c "SELECT ..."                    # one query, pipeable
#   local/blobs.sh -ui                                # the DuckDB UI, views in the sidebar
#
# The views are the .sql files this script reads, which are also what the
# itest and the compactor use:
#   compactor/sql/raw.sql     raw: every line, columns pinned (the contract)
#   compactor/sql/tables.sql  logs, metrics (samples unnested), podindex
# and over the hourly tier, for the tables the compactor has written:
#   hourly_logs, hourly_metrics, hourly_podindex, pods
# The argument scopes the raw views; a node-hour prefix is the cheap shape,
# the whole cluster LISTs everything.
set -euo pipefail
DIR=$(cd "$(dirname "$0")" && pwd)

BUCKET=${S3_BUCKET:-node-agent}
ENDPOINT=${S3_ENDPOINT:-127.0.0.1:30070}
ENDPOINT=${ENDPOINT#http://}; ENDPOINT=${ENDPOINT#https://}
USE_SSL=$([ "${S3_TLS:-off}" = on ] && echo true || echo false)
REGION=${S3_REGION:-us-east-1}
KEY_ID=${AWS_ACCESS_KEY_ID:-demoaccess}
SECRET=${AWS_SECRET_ACCESS_KEY:-demosecret}

# First argument is the raw prefix (cluster[/date[/HH[/node]]]) unless it is
# already a duckdb flag.
PREFIX=itest
case "${1:-}" in
  ''|-*) ;;
  *) PREFIX=${1%/}; PREFIX=${PREFIX#/}; shift ;;
esac
CLUSTER=${PREFIX%%/*}
# pad the prefix with wildcards down to the object level
depth=$(tr -cd '/' <<< "$PREFIX" | wc -c)
GLOB="$PREFIX"
while [ "$depth" -lt 3 ]; do GLOB="$GLOB/*"; depth=$((depth + 1)); done
GLOB="$GLOB/*.jsonl.zst"

INIT=$(mktemp "${TMPDIR:-/tmp}/blobs.XXXXXX")  # 0600; the only place the secret and the keys are written
trap 'rm -f "$INIT" "$INIT.probe" "$INIT.visible"' EXIT INT TERM

# The secret and the variables; the views come from the .sql files.
cat >"$INIT.probe" <<SQL
INSTALL httpfs; LOAD httpfs;
SET TimeZone = 'UTC';
CREATE OR REPLACE SECRET blobs (
  TYPE s3, PROVIDER config,
  KEY_ID '$KEY_ID', SECRET '$SECRET',
  ENDPOINT '$ENDPOINT', REGION '$REGION', URL_STYLE 'path', USE_SSL $USE_SSL,
  SCOPE 's3://$BUCKET'
);
SET enable_object_cache = true;
SET VARIABLE raw_files = 's3://$BUCKET/raw1/$GLOB';
SET VARIABLE hourly_prefix = 's3://$BUCKET/hourly/$CLUSTER';
SQL
KEY_IDS=" "
if [ -n "${PARQUET_KEYS_DIR:-}" ]; then
  for f in "$PARQUET_KEYS_DIR"/*; do  # not the dot entries of a Secret's mount
    [ -f "$f" ] || continue
    id=${f##*/}
    [[ $id =~ ^[a-z0-9][a-z0-9-]{0,31}$ ]] || { echo "PARQUET_KEYS_DIR: $id is not a key id" >&2; exit 1; }
    echo "PRAGMA add_parquet_key('$id', '$(tr -d '[:space:]' < "$f")');" >>"$INIT.probe"
    KEY_IDS="$KEY_IDS$id "
  done
fi
{
  echo ".output /dev/null"
  cat "$INIT.probe"
  echo ".read $DIR/../compactor/sql/raw.sql"
  echo ".read $DIR/../compactor/sql/tables.sql"
} >"$INIT"

# The hourly views: one object per table per hour, namespace-sorted, and
# the pods index. An object is plain, {HH}.parquet, or encrypted with the
# key its name carries, {HH}.k-{id}.parquet. One read takes one key, so a
# view unites by name a read of the plain objects and one per key id;
# union_by_name aligns an older hour's columns (raw1 changes are additive),
# which DuckDB 1.5 cannot do on an encrypted read, so within one key the
# first object's columns win. Creating a view binds its globs: a view has
# only the reads whose glob matches objects, and a table without objects
# has no view.
sources=$(duckdb -noheader -csv -c ".output /dev/null" -c ".read $INIT.probe" -c ".output" \
  -c "SELECT DISTINCT regexp_extract(file, '/hourly/[^/]+/([^/]+)/', 1), regexp_extract(file, '\.k-([a-z0-9-]+)\.parquet\$', 1)
      FROM glob(getvariable('hourly_prefix') || '/*/*/*.parquet') ORDER BY ALL" 2>/dev/null) || sources=
for t in logs metrics podindex pods; do
  view=
  while IFS=, read -r table id; do
    [ "$table" = "$t" ] || continue
    from="read_parquet(getvariable('hourly_prefix') || '/$t/*/[0-9][0-9].parquet', filename = true, union_by_name = true)"
    if [ -n "$id" ]; then
      case "$KEY_IDS" in
        *" $id "*) from="read_parquet(getvariable('hourly_prefix') || '/$t/*/*.k-$id.parquet', filename = true, encryption_config = {footer_key: '$id'})" ;;
        *) echo "blobs.sh: hourly $t objects under key $id, which PARQUET_KEYS_DIR lacks, are not in the view" >&2; continue ;;
      esac
    fi
    view="${view:+$view UNION ALL BY NAME }SELECT * FROM $from"
  done <<< "$sources"
  name=hourly_$t; [ "$t" != pods ] || name=pods
  [ -z "$view" ] || echo "CREATE OR REPLACE VIEW $name AS $view;" >>"$INIT"
done
echo ".output" >>"$INIT"

# Not exec: the EXIT trap has to survive to unlink the init file. Errors
# inside the init file go to its .output redirection, so on failure the
# view definitions are replayed once with output visible.
rc=0
case "${1:-}" in
  # -init prints a banner with an ANSI reset in front of the first result
  # line; for one-shot queries load the init through -c so output pipes
  -c) duckdb -c ".read $INIT" "$@" || rc=$? ;;
  *) duckdb -init "$INIT" "$@" || rc=$? ;;
esac
if [ "$rc" -ne 0 ]; then
  grep -v '^\.output' "$INIT" > "$INIT.visible"
  duckdb -init "$INIT.visible" -c "SELECT 1" 2>&1 | grep -iv '^│\|^┌\|^├\|^└\|Loading resources' >&2 || true
fi
exit $rc
