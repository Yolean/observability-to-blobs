#!/bin/bash
# All three processes in one instance; if any dies the container exits and
# the next request (or the watchdog) starts a fresh one, which backfills
# from the bucket -- cold start is the design, not a failure. The tailer
# needs no start ordering: a failed remote write is retried next poll.
set -euo pipefail
echo "alerts-cf: S3_ENDPOINT=${S3_ENDPOINT:-} S3_BUCKET=${S3_BUCKET:-} CLUSTER_NAME=${CLUSTER_NAME:-} creds=$([ -n "${AWS_ACCESS_KEY_ID:-}" ] && echo present || echo MISSING)"
/bin/alertmanager --config.file=/etc/alerts/alertmanager.yml \
  --storage.path=/tmp/alertmanager &
/bin/prometheus --config.file=/etc/alerts/prometheus.yml \
  --storage.tsdb.path=/tmp/prometheus \
  --web.enable-remote-write-receiver \
  --web.listen-address=0.0.0.0:9090 &
/bin/blobs-to-prometheus &
wait -n
exit 1
