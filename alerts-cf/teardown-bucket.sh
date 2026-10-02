#!/usr/bin/env bash
# Purge and delete the test- R2 bucket. This is the irreversible half of the
# teardown: R2 has no undelete, so the only copy of the itest corpus
# afterwards is the local mirror.
#
#   alerts-cf/teardown-bucket.sh              # check the mirror, change nothing
#   alerts-cf/teardown-bucket.sh --yes        # purge, then delete the bucket
#
# The guard is the point: it refuses unless a local mirror already holds at
# least as many objects as the bucket does. Make one with, from the repo
# root,
#
#   docker run --rm --env-file itest/r2/.env.r2 -e AWS_DEFAULT_REGION=auto \
#     -v "$PWD/local/mirror:/mirror" amazon/aws-cli:2.22.35 \
#     s3 sync s3://node-agent-itest/ /mirror --endpoint-url "$ENDPOINT"
#
# Stop the agent before running this (kubectl -n observability scale
# ds/node-agent --replicas=0, or tear the cluster down). R2 refuses to
# delete a non-empty bucket, so a writer that is still shipping re-creates
# objects between the purge and the delete and the delete fails.
#
# Credentials: the bucket-scoped pair in itest/r2/.env.r2 can purge but not
# delete the bucket itself; the account token in alerts-cf/.env does that.
set -euo pipefail
cd "$(dirname "$0")"

BUCKET=node-agent-itest
MIRROR=${MIRROR:-../local/mirror}
ENV_R2=../itest/r2/.env.r2

[ -f "$ENV_R2" ] || { echo "missing itest/r2/.env.r2" >&2; exit 1; }
ENDPOINT=$(sed -n 's/^S3_ENDPOINT=//p' "$ENV_R2")
[ -n "$ENDPOINT" ] || { echo "itest/r2/.env.r2 has no S3_ENDPOINT" >&2; exit 1; }
[ -f .env ] || { echo "missing alerts-cf/.env (copy .env.example)" >&2; exit 1; }

aws() { # the 2.22.35 pin predates aws-cli 2.23's integrity default, per clean.sh
  docker run --rm --env-file "$ENV_R2" -e AWS_DEFAULT_REGION=auto \
    amazon/aws-cli:2.22.35 "$@" --endpoint-url "$ENDPOINT"
}

# Every object counts, whatever its key: the agent writes raw1/.../*.jsonl.zst,
# the compactor hourly/ and daily/ parquet and csv, the PoC parquet under the
# cluster name. Counting one suffix would call a bucket of the others empty.
remote=$(aws s3 ls "s3://$BUCKET/" --recursive | grep -c . || true)
local_count=0
[ -d "$MIRROR" ] && local_count=$(find "$MIRROR" -type f | wc -l | tr -d ' ')

echo "bucket s3://$BUCKET/ : $remote objects"
echo "mirror $MIRROR : $local_count files"

if [ "$remote" -eq 0 ]; then
  echo "bucket already empty"
elif [ "$local_count" -lt "$remote" ]; then
  echo >&2
  echo "REFUSING: the mirror is missing $((remote - local_count)) object(s)." >&2
  echo "Re-run the s3 sync in this script's header, then try again." >&2
  exit 1
fi

if [ "${1:-}" != --yes ]; then
  echo
  echo "mirror covers the bucket. Re-run with --yes to purge and delete it."
  exit 0
fi

echo
echo "== purging =="
aws s3 rm --recursive --quiet "s3://$BUCKET/"

echo "== deleting bucket =="
set -a
# gitignored, so there is nothing to follow at lint time
# shellcheck disable=SC1091
. ./.env
set +a
: "${CLOUDFLARE_API_TOKEN:?not set in alerts-cf/.env}"
# EU-jurisdiction buckets need -J on every wrangler r2 call, list included.
pnpm exec wrangler r2 bucket delete "$BUCKET" -J eu

echo
echo "deleted s3://$BUCKET/ -- the mirror at $MIRROR is now the only copy."
