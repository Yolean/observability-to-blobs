#!/usr/bin/env bash
# Load test against the running itest cluster: N pods each writing a fixed
# number of lines per second for a while, then a look at what the agent
# did with it. Not part of test.sh; run it after test.sh has passed.
#
#   itest/stress.sh [pods] [lines_per_second_per_pod] [seconds]
set -euo pipefail
cd "$(dirname "$0")/.."

PODS=${1:-20}
RATE=${2:-200}
SECONDS_RUN=${3:-150}
CONTEXT=node-agent
export KUBECONFIG=$PWD/itest/cluster/kubeconfig
KUBECTL="kubectl --context=$CONTEXT"

S3_SQL="INSTALL httpfs; LOAD httpfs;
SET s3_region='us-east-1'; SET s3_endpoint='127.0.0.1:30070';
SET s3_access_key_id='demoaccess'; SET s3_secret_access_key='demosecret';
SET s3_use_ssl=false; SET s3_url_style='path';"
LOGS="read_parquet('s3://node-agent/itest/*/*/*/*/logs-*.parquet', filename=true)"

echo "==> $PODS pods x $RATE lines/s for ${SECONDS_RUN}s ($((PODS * RATE)) lines/s, ~$((PODS * RATE * 120 / 1024)) KiB/s)"
START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
for i in $(seq 1 "$PODS"); do
  $KUBECTL run "stress-$i" --restart=Never --labels=app=stress --image=busybox:1.37 \
    --command -- sh -c "i=0; end=\$((\$(date +%s) + $SECONDS_RUN)); while [ \$(date +%s) -lt \$end ]; do n=0; while [ \$n -lt $RATE ]; do i=\$((i+1)); n=\$((n+1)); echo \"{\\\"seq\\\":\$i,\\\"pod\\\":\\\"stress-$i\\\",\\\"msg\\\":\\\"stress line with some padding to look like a real log message 0123456789\\\"}\"; done; sleep 1; done; echo done" \
    >/dev/null &
done
wait
$KUBECTL wait pod -l app=stress --for=condition=Ready --timeout=120s >/dev/null
echo "    started at $START"

for _ in $(seq 1 $((SECONDS_RUN / 30 + 2))); do
  sleep 30
  printf '    %s  ' "$(date -u +%T)"
  $KUBECTL top pod -l app=node-agent --no-headers 2>/dev/null || echo "(metrics-server not ready)"
done

echo "==> waiting for the last window to land"
sleep 75

echo "==> lines shipped per stress pod (expect $((RATE * SECONDS_RUN)) each, plus 'done')"
duckdb -c "$S3_SQL
  SELECT pod, count(*) AS lines, max(CAST(json_extract(message, '\$.seq') AS INTEGER)) AS max_seq,
         count(DISTINCT CAST(json_extract(message, '\$.seq') AS INTEGER)) AS distinct_seq
  FROM $LOGS WHERE pod LIKE 'stress-%' AND message LIKE '{%'
  GROUP BY pod ORDER BY pod;"

echo "==> objects per window during the run"
duckdb -c "$S3_SQL
  SELECT regexp_extract(filename, '([0-9]{2}/[0-9]{2})/logs-', 1) AS w,
         count(DISTINCT filename) AS objects, count(*) AS rows
  FROM $LOGS WHERE pod LIKE 'stress-%'
  GROUP BY w ORDER BY w;"

echo "==> agent CPU and memory from its own metrics (fluentbit_* are in the metrics table; process stats via node job)"
duckdb -c "$S3_SQL
  SELECT time, labels['name'] AS input, value
  FROM read_parquet('s3://node-agent/itest/*/*/*/*/metrics-*.parquet')
  WHERE name = 'fluentbit_input_records_total' AND labels['name'] = 'pods'
  ORDER BY time DESC LIMIT 5;"

$KUBECTL delete pod -l app=stress --grace-period=1 >/dev/null 2>&1 || true
echo "==> stress pods deleted"
