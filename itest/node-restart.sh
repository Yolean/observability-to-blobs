#!/usr/bin/env bash
# Node termination test against the running qemu itest cluster: write a
# marker, stop the VM (systemd shuts k3s down, pods get SIGTERM), start it
# again, and require the marker to show up exactly once. Whether the exit
# flush managed to upload before versitygw went away or the buffer file was
# recovered from /var/lib/node-agent on the next start does not matter to
# the reader; both must end in one copy.
#
#   itest/node-restart.sh
set -euo pipefail
cd "$(dirname "$0")/.."

CONTEXT=node-agent
export KUBECONFIG=$PWD/itest/cluster/kubeconfig
KUBECTL="kubectl --context=$CONTEXT"

S3_SQL="INSTALL httpfs; LOAD httpfs;
SET s3_region='us-east-1'; SET s3_endpoint='127.0.0.1:30070';
SET s3_access_key_id='demoaccess'; SET s3_secret_access_key='demosecret';
SET s3_use_ssl=false; SET s3_url_style='path';"
LOGS="read_parquet('s3://node-agent/itest/*/*/*/*/logs-*.parquet', filename=true)"
q() { duckdb -noheader -csv -c "$S3_SQL $1" 2>&1; }

MARKER="node-restart-marker-$(date +%s)"
echo "==> marker $MARKER"
$KUBECTL exec deploy/log-generator -- sh -c "echo '$MARKER' > /proc/1/fd/1"
sleep 8

echo "==> stopping the VM $(date -u +%T)"
y-cluster stop --context "$CONTEXT"
echo "==> starting the VM $(date -u +%T)"
y-cluster start --context "$CONTEXT"
# the API server answers ServiceUnavailable for a while after start
deadline=$((SECONDS + 300))
until $KUBECTL get node/node-agent >/dev/null 2>&1; do
  [ $SECONDS -lt $deadline ] || { echo "FAIL: API server not back after 300s" >&2; exit 1; }
  sleep 3
done
$KUBECTL wait node/node-agent --for=condition=Ready --timeout=300s
$KUBECTL rollout status statefulset/versitygw --timeout=300s
$KUBECTL rollout status daemonset/node-agent --timeout=300s
echo "==> cluster back $(date -u +%T)"

deadline=$((SECONDS + 240))
while :; do
  n=$(q "SELECT count(*) FROM $LOGS WHERE message = '$MARKER'" | tr -d '[:space:]' || true)
  case "$n" in (''|*[!0-9]*) n=0;; esac
  echo "    marker copies: $n"
  [ "$n" -ge 1 ] && break
  if [ $SECONDS -ge $deadline ]; then
    $KUBECTL logs ds/node-agent --tail=40 || true
    echo "FAIL: marker not shipped within 240s of the restart" >&2
    exit 1
  fi
  sleep 10
done
# one more window to make sure no second copy follows
sleep 70
n=$(q "SELECT count(*) FROM $LOGS WHERE message = '$MARKER'" | tr -d '[:space:]')
if [ "$n" = 1 ]; then
  echo "PASS: marker shipped exactly once across the node restart"
else
  echo "FAIL: marker shipped $n times" >&2
  exit 1
fi
duckdb -c "$S3_SQL SELECT filename, count(*) AS rows FROM $LOGS WHERE message = '$MARKER' GROUP BY filename;"
echo "==> agent log around the restart:"
$KUBECTL logs ds/node-agent --previous --tail=8 2>/dev/null | cut -c1-160 || echo "(previous container log not retrievable)"
$KUBECTL logs ds/node-agent --tail=400 | grep -E 'fstore|recover|Successfully uploaded|upload_timeout' | head -8 | cut -c1-160
