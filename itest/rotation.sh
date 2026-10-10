#!/usr/bin/env bash
# Log rotation test against the running itest cluster: one pod writes well
# past the kubelet's containerLogMaxSize (10 MiB on k3s) in a few seconds,
# so /var/log/pods/.../0.log is rotated under the tail input while it is
# still catching up. Every sequence number must arrive exactly once.
#
#   itest/rotation.sh [lines]        default 200000 (~26 MB)
set -euo pipefail
cd "$(dirname "$0")/.."

LINES=${1:-200000}
CONTEXT=${CONTEXT:-node-agent}
export KUBECONFIG=$PWD/itest/cluster/kubeconfig
KUBECTL="kubectl --context=$CONTEXT"
S3_SQL="INSTALL httpfs; LOAD httpfs;
SET s3_region='us-east-1'; SET s3_endpoint='127.0.0.1:${S3_PORT:-30070}';
SET s3_access_key_id='demoaccess'; SET s3_secret_access_key='demosecret';
SET s3_use_ssl=false; SET s3_url_style='path';"
LOGS="read_parquet('s3://node-agent/itest/*/*/*/*/logs-*.parquet', filename=true)"

POD="rotation-$(date +%s)"
echo "==> $POD writes $LINES lines as fast as busybox can"
$KUBECTL run "$POD" --restart=Never --labels=app=rotation --image=busybox:1.37 \
  --command -- sh -c "seq 1 $LINES | sed 's/^/{\"seq\":/; s/\$/,\"pad\":\"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"}/'; echo done; sleep 600" >/dev/null
$KUBECTL wait pod "$POD" --for=condition=Ready --timeout=120s >/dev/null
sleep 15
echo "==> files on the node after the burst:"
ssh -p 2232 -i "$HOME/.cache/y-cluster-qemu/node-agent-ssh" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  ystack@localhost "sudo sh -c 'ls -la /var/log/pods/default_${POD}_*/*/'" 2>/dev/null | awk '{print "    " $5, $9}' || echo "    (not a qemu node, skipping)"

echo "==> waiting for the windows to land"
deadline=$((SECONDS + 300))
while :; do
  n=$(duckdb -noheader -csv -c "$S3_SQL SELECT count(*) FROM $LOGS WHERE pod = '$POD' AND message = 'done'" 2>/dev/null | tr -d '[:space:]' || true)
  [ "$n" = 1 ] && break
  [ $SECONDS -lt $deadline ] || { echo "FAIL: 'done' line not shipped within 300s" >&2; exit 1; }
  sleep 10
done
sleep 70
duckdb -c "$S3_SQL
  SELECT count(*) AS lines, count(DISTINCT CAST(json_extract(message, '\$.seq') AS INTEGER)) AS distinct_seq,
         min(CAST(json_extract(message, '\$.seq') AS INTEGER)) AS min_seq, max(CAST(json_extract(message, '\$.seq') AS INTEGER)) AS max_seq,
         round(sum(length(message)) / 1024 / 1024, 1) AS mib
  FROM $LOGS WHERE pod = '$POD' AND message LIKE '{%';"
result=$(duckdb -noheader -csv -c "$S3_SQL
  SELECT count(*) = $LINES AND count(DISTINCT CAST(json_extract(message, '\$.seq') AS INTEGER)) = $LINES
  FROM $LOGS WHERE pod = '$POD' AND message LIKE '{%';" | tr -d '[:space:]')
$KUBECTL delete pod "$POD" --grace-period=1 >/dev/null 2>&1 || true
if [ "$result" = true ]; then
  echo "PASS: all $LINES lines shipped exactly once across rotation"
else
  echo "FAIL: lines missing or duplicated (see counts above)" >&2
  exit 1
fi
