#!/usr/bin/env bash
# A DuckDB session over the bucket, with a view per table. Defaults to the
# itest cluster's versitygw on NodePort 30070; export S3_ENDPOINT, S3_TLS,
# S3_REGION, AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for any other
# bucket (for R2: the EU jurisdiction endpoint, REGION auto).
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
#   local/hourly.sql          hourly_logs, hourly_metrics, hourly_podindex, pods, if the compactor has written them
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

INIT=$(mktemp "${TMPDIR:-/tmp}/blobs.XXXXXX")  # 0600; the only place the secret is written
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
{
  echo ".output /dev/null"
  cat "$INIT.probe"
  echo ".read $DIR/../compactor/sql/raw.sql"
  echo ".read $DIR/../compactor/sql/tables.sql"
} >"$INIT"

# Creating a view binds its glob, so the hourly views exist only when the
# compactor has written every table of the tier; podindex is the one an
# older hourly tier lacks.
if duckdb -init "$INIT.probe" -c "SELECT * FROM read_parquet(getvariable('hourly_prefix') || '/podindex/*/*.parquet') LIMIT 0" >/dev/null 2>&1; then
  echo ".read $DIR/hourly.sql" >>"$INIT"
fi
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
