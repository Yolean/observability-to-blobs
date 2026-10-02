#!/usr/bin/env bash
# Undeploy the Cloudflare half of the alerts stack: the Worker, its Durable
# Objects and the container instances it owns, then the images left in the
# managed registry.
#
# This is the whole recurring cost. The container is always-on by design --
# src/index.mjs overrides onActivityExpired() with an empty body (the
# documented never-sleep) and the Watchdog re-arms a 60 s alarm that renews
# activity -- so sleepAfter never applies and memory and disk bill as
# provisioned for the full month, ~$12-14 at instance_type basic. Nothing
# scales it down short of deleting it: max_instances only caps the ceiling,
# and the watchdog alarm survives a redeploy because it lives in DO storage,
# which only `wrangler delete` clears.
#
# Deleting the Worker also drops its secrets (R2_ACCESS_KEY_ID and
# R2_SECRET_ACCESS_KEY), so a later redeploy repeats the `wrangler secret
# put` steps in README.md.
#
# The R2 bucket is a separate half, and separately destructive:
# teardown-bucket.sh.
set -euo pipefail
cd "$(dirname "$0")"

WORKER=y-alerts

[ -f .env ] || { echo "missing alerts-cf/.env (copy .env.example)" >&2; exit 1; }
set -a
# gitignored, so there is nothing to follow at lint time
# shellcheck disable=SC1091
. ./.env
set +a
: "${CLOUDFLARE_API_TOKEN:?not set in alerts-cf/.env}"

echo "== before =="
pnpm exec wrangler containers list || true

# Removes the Worker and its Durable Objects, which is what finally stops
# the watchdog alarm loop.
echo
echo "== deleting worker $WORKER =="
pnpm exec wrangler delete --force

# `wrangler delete` does NOT take the container application with it, whatever
# the docs imply: measured 2026-08-28, the application stayed `active` with a
# `running` instance after the Worker was gone, still billing. Deleting it is
# a separate call, and it is the one that actually stops the meter.
echo
echo "== deleting container applications =="
pnpm exec wrangler containers list 2>/dev/null |
  grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' |
  while read -r id; do
    echo "-- $id"
    pnpm exec wrangler containers delete "$id" || true
  done

# Image layers keep billing registry storage after the application is gone,
# and they are not removed with the Worker either. Tags are listed as
# "REPOSITORY  TAG" and deleted as IMAGE:TAG.
echo
echo "== deleting images =="
pnpm exec wrangler containers images list 2>/dev/null |
  awk 'NR > 1 && NF == 2 { print $1 ":" $2 }' |
  while read -r image; do
    echo "-- $image"
    pnpm exec wrangler containers images delete "$image" --skip-confirmation || true
  done

echo
echo "== after =="
pnpm exec wrangler containers list || true
pnpm exec wrangler containers images list || true
