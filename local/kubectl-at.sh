#!/usr/bin/env bash
# kubectl against a cluster as it was at a time, from the bucket alone
# (K8SAGENT.md, "Reading"): local/apiserver_at.sql over the cluster agent's
# objects, served read-only by tools/apiserver-at.
#
#   local/kubectl-at.sh itest '2026-10-07 10:12:00' get pods -A    # one command
#   local/kubectl-at.sh itest '2026-10-07 10:12:00' describe pod -n app web-1
#   local/kubectl-at.sh itest '2026-10-07 10:12:00'                # serve until ^C
#
# The time is UTC. The objects come from the hourly tier (the hour of the
# time and the one before, which holds the base snapshot of a time just
# after a full hour), else the daily tier (the day and the day before);
# SOURCE=raw reads the raw tier's two hours instead. The bucket is
# local/blobs.sh's: S3_BUCKET, S3_ENDPOINT, S3_TLS, S3_REGION,
# AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, PARQUET_KEYS_DIR.
#
# apiserver-at runs kube-apiserver and etcd of the cluster's release at
# that time, from its Nodes' kubelet version, downloaded by setup-envtest
# unless KUBEBUILDER_ASSETS names them. Needs go, duckdb and kubectl.
set -euo pipefail
DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$DIR/..

[ $# -ge 2 ] || { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
CLUSTER=$1 AT=$2
shift 2

BUCKET=${S3_BUCKET:-node-agent}
ENDPOINT=${S3_ENDPOINT:-127.0.0.1:30070}
ENDPOINT=${ENDPOINT#http://}; ENDPOINT=${ENDPOINT#https://}
USE_SSL=$([ "${S3_TLS:-off}" = on ] && echo true || echo false)
REGION=${S3_REGION:-us-east-1}
KEY_ID=${AWS_ACCESS_KEY_ID:-demoaccess}
SECRET=${AWS_SECRET_ACCESS_KEY:-demosecret}
SETUP_ENVTEST=v0.25.2

umask 077  # $WORK holds the secret, the keys and the state
WORK=$(mktemp -d "${TMPDIR:-/tmp}/kubectl-at.XXXXXX")
AT_PID=""
cleanup() {
  [ -z "$AT_PID" ] || { kill "$AT_PID" 2>/dev/null || true; wait "$AT_PID" 2>/dev/null || true; }
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

cat >"$WORK/init.sql" <<SQL
.output /dev/null
INSTALL httpfs; LOAD httpfs;
SET TimeZone = 'UTC';
CREATE OR REPLACE SECRET blobs (
  TYPE s3, PROVIDER config,
  KEY_ID '$KEY_ID', SECRET '$SECRET',
  ENDPOINT '$ENDPOINT', REGION '$REGION', URL_STYLE 'path', USE_SSL $USE_SSL,
  SCOPE 's3://$BUCKET'
);
SQL
if [ -n "${PARQUET_KEYS_DIR:-}" ]; then
  for f in "$PARQUET_KEYS_DIR"/*; do
    [ -f "$f" ] || continue
    echo "PRAGMA add_parquet_key('${f##*/}', '$(tr -d '[:space:]' < "$f")');" >>"$WORK/init.sql"
  done
fi
q() { duckdb -noheader -csv -c ".read $WORK/init.sql" -c ".output" -c "$1"; }

hours=$(q "SELECT strftime(h, '%Y-%m-%d/%H') FROM (SELECT unnest([date_trunc('hour', TIMESTAMP '$AT') - INTERVAL 1 HOUR, date_trunc('hour', TIMESTAMP '$AT')]) AS h)")
days=$(q "SELECT strftime(d, '%Y-%m-%d') FROM (SELECT unnest([date_trunc('day', TIMESTAMP '$AT') - INTERVAL 1 DAY, date_trunc('day', TIMESTAMP '$AT')]) AS d)")

# tier <stem...>: a FROM clause over the tier objects of the stems, each
# read with the parquet key its name carries (compactor/README.md,
# "Encryption"); empty when none exists
tier() {
  local from="" file id
  for stem in "$@"; do
    for file in $(q "SELECT file FROM glob('s3://$BUCKET/$stem*.parquet')"); do
      id=$(sed -n 's/.*\.k-\([a-z0-9-]*\)\.parquet$/\1/p' <<<"$file")
      if [ -n "$id" ]; then
        from="${from:+$from UNION ALL BY NAME }SELECT * FROM read_parquet('$file', encryption_config = {footer_key: '$id'})"
      else
        from="${from:+$from UNION ALL BY NAME }SELECT * FROM read_parquet('$file')"
      fi
    done
  done
  echo "$from"
}

case "${SOURCE:-tier}" in
  (raw)
    files=""
    for h in $hours; do files="${files:+$files, }'s3://$BUCKET/raw1/$CLUSTER/$h/*/*.jsonl.zst'"; done
    {
      echo "SET VARIABLE raw_files = [$files];"
      echo ".read $ROOT/compactor/sql/raw.sql"
      echo ".read $ROOT/compactor/sql/tables.sql"
    } >>"$WORK/init.sql"
    ;;
  (tier)
    stems=()
    for h in $hours; do stems+=("hourly/$CLUSTER/apiserver/$h"); done
    from=$(tier "${stems[@]}")
    if [ -z "$from" ]; then
      stems=()
      for d in $days; do stems+=("daily/$CLUSTER/apiserver/$d"); done
      from=$(tier "${stems[@]}")
    fi
    [ -n "$from" ] || { echo "no objects of $CLUSTER around $AT in the hourly or daily tier of s3://$BUCKET (SOURCE=raw reads raw1/)" >&2; exit 1; }
    echo "CREATE OR REPLACE TEMP VIEW apiserver AS $from;" >>"$WORK/init.sql"
    ;;
  (*) echo "SOURCE: raw, or unset for the tiers" >&2; exit 2 ;;
esac
echo ".read $ROOT/local/apiserver_at.sql" >>"$WORK/init.sql"

q "COPY (SELECT api_version, resource, namespace, name, apijson FROM apiserver_at(TIMESTAMP_NS '$AT')) TO '$WORK/state.jsonl' (FORMAT json)" >/dev/null
[ -s "$WORK/state.jsonl" ] || { echo "no complete snapshot of $CLUSTER at or before $AT" >&2; exit 1; }

if [ -z "${KUBEBUILDER_ASSETS:-}" ]; then
  # the release the cluster's kubelets reported at that time: v1.37.0+k3s1 -> 1.37.x
  minor=$(q "SELECT regexp_extract(max(json_extract_string(apijson, '\$.status.nodeInfo.kubeletVersion')), '^v([0-9]+\.[0-9]+)', 1)
             FROM apiserver_at(TIMESTAMP_NS '$AT') WHERE api_version = 'v1' AND resource = 'nodes'")
  [ -n "$minor" ] || { echo "no Node with a kubelet version at $AT; set KUBEBUILDER_ASSETS" >&2; exit 1; }
  KUBEBUILDER_ASSETS=$(GOFLAGS='' go run "sigs.k8s.io/controller-runtime/tools/setup-envtest@$SETUP_ENVTEST" use "$minor.x" -p path)
  export KUBEBUILDER_ASSETS
fi
BIN=$ROOT/target/bin/apiserver-at
(cd "$ROOT/tools/apiserver-at" && go build -o "$BIN" .)

"$BIN" -objects "$WORK/state.jsonl" -kubeconfig "$WORK/kubeconfig" -context "$CLUSTER-at" -label "$AT UTC" >"$WORK/serve.log" 2>&1 &
AT_PID=$!
until grep -q '^serving ' "$WORK/serve.log"; do
  kill -0 "$AT_PID" 2>/dev/null || { cat "$WORK/serve.log" >&2; exit 1; }
  sleep 0.5
done

if [ $# -gt 0 ]; then
  kubectl --kubeconfig "$WORK/kubeconfig" "$@"
  exit
fi
cp "$WORK/kubeconfig" "$WORK.kubeconfig"
grep '^serving ' "$WORK/serve.log" >&2
echo "  export KUBECONFIG=$WORK.kubeconfig    # $CLUSTER at $AT UTC, read-only; ^C stops it" >&2
trap 'rm -f "$WORK.kubeconfig"; cleanup' EXIT INT TERM
wait "$AT_PID"
