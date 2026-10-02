#!/usr/bin/env bash
# Purge every object in the test bucket so experiments restart from afresh:
# the schemas are not final, and leftovers poison globs and the tailer's
# backfill. Works without the cluster (aws-cli in docker against the EU
# endpoint); deletes are free on R2. The 2.22.35 pin predates aws-cli
# 2.23's default integrity-protection change that breaks S3-compatibles.
set -euo pipefail
cd "$(dirname "$0")"
[ -f .env.r2 ] || { echo "missing itest/r2/.env.r2 (copy .env.r2.example)" >&2; exit 1; }
BUCKET=node-agent-itest
ENDPOINT=$(sed -n 's/^S3_ENDPOINT=//p' .env.r2)
[ -n "$ENDPOINT" ] || { echo "itest/r2/.env.r2 has no S3_ENDPOINT" >&2; exit 1; }
docker run --rm --env-file .env.r2 -e AWS_DEFAULT_REGION=auto \
  amazon/aws-cli:2.22.35 s3 rm --recursive --quiet "s3://$BUCKET/" --endpoint-url "$ENDPOINT"
echo "purged s3://$BUCKET/"
