#!/usr/bin/env bash
# Creates a developer feed's R2 bucket in the EU jurisdiction, or finds it
# there (README.md, Setup), and writes what it established to the user-level
# host agent's config (developer-feed/lib/config.sh): the account, bucket and
# jurisdiction, S3_BUCKET, S3_ENDPOINT and S3_REGION. Run it again at any
# time: an existing bucket is left as it is, and a second run needs no flags.
#
#   developer-feed/cloudflare/setup-bucket.sh --account <account id> --bucket <name>
#
# The Cloudflare API token needs "Workers R2 Storage: Edit" on the account
# (Workers R2 Storage Write); a short expiry and an IP filter are good
# practice, it is needed for this script only. It is read from
# CLOUDFLARE_API_TOKEN, or from the terminal with echo off, and handed to
# curl on stdin, never in a command line.
#
# The flags' defaults are DEV_FEED_ACCOUNT and DEV_FEED_BUCKET from the
# config. CLOUDFLARE_API_BASE (default https://api.cloudflare.com/client/v4)
# is for tests.
set -euo pipefail
# shellcheck source=../lib/config.sh
. "$(cd "$(dirname "$0")/.." && pwd)/lib/config.sh"

account=$(cfg_get DEV_FEED_ACCOUNT)
bucket=$(cfg_get DEV_FEED_BUCKET)
jurisdiction=eu
api=${CLOUDFLARE_API_BASE:-https://api.cloudflare.com/client/v4}
while [ $# -gt 0 ]; do
  case $1 in
    --account) account=$2; shift 2;;
    --bucket) bucket=$2; shift 2;;
    *) echo "setup-bucket.sh: unknown argument $1" >&2; exit 2;;
  esac
done
[ -n "$account" ] && [ -n "$bucket" ] || { echo "usage: setup-bucket.sh --account <account id> --bucket <name>" >&2; exit 2; }
# R2's bucket names: 3 to 63 of a-z, 0-9 and -, not starting or ending with -
[[ $bucket =~ ^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$ ]] || { echo "setup-bucket.sh: $bucket is not an R2 bucket name" >&2; exit 2; }
for t in curl jq; do command -v $t >/dev/null || { echo "setup-bucket.sh: needs $t" >&2; exit 2; }; done

token=${CLOUDFLARE_API_TOKEN:-}
if [ -z "$token" ]; then
  [ -t 0 ] || { echo "setup-bucket.sh: set CLOUDFLARE_API_TOKEN, or run it in a terminal" >&2; exit 2; }
  printf 'Cloudflare API token (Workers R2 Storage: Edit): ' >&2
  stty -echo
  IFS= read -r token || true
  stty echo
  echo >&2
fi
[ -n "$token" ] || { echo "setup-bucket.sh: no token" >&2; exit 2; }

# cf <method> <path> [json body]: the API's JSON on stdout; the token and
# the jurisdiction go to curl as a config on stdin
cf() {
  local method=$1 path=$2 body=${3:-}
  {
    printf 'header = "Authorization: Bearer %s"\n' "$token"
    printf 'header = "cf-r2-jurisdiction: %s"\n' "$jurisdiction"
    printf 'header = "Content-Type: application/json"\n'
  } | curl -sS -X "$method" --config - ${body:+--data "$body"} "$api$path"
}
errors() { jq -r '[.errors[]? | "\(.code): \(.message)"] | join("; ")'; }

echo "== the token"
r=$(cf GET "/accounts/$account/tokens/verify")
[ "$(jq -r '.success' <<<"$r")" = true ] && [ "$(jq -r '.result.status' <<<"$r")" = active ] \
  || { echo "the token does not verify for account $account: $(errors <<<"$r")" >&2; exit 1; }
echo "active, expires $(jq -r '.result.expires_on // "never"' <<<"$r")"

echo "== the bucket $bucket, jurisdiction $jurisdiction"
r=$(cf GET "/accounts/$account/r2/buckets/$bucket")
if [ "$(jq -r '.success' <<<"$r")" = true ]; then
  echo "exists"
else
  echo "not found ($(errors <<<"$r")); creating"
  r=$(cf POST "/accounts/$account/r2/buckets" "$(jq -cn --arg n "$bucket" '{name: $n}')")
  [ "$(jq -r '.success' <<<"$r")" = true ] || { echo "creating $bucket failed: $(errors <<<"$r")" >&2; exit 1; }
  r=$(cf GET "/accounts/$account/r2/buckets/$bucket")
  [ "$(jq -r '.success' <<<"$r")" = true ] || { echo "$bucket was created but cannot be read back: $(errors <<<"$r")" >&2; exit 1; }
  echo "created"
fi
# a GET with the jurisdiction header finds only that jurisdiction's
# buckets; the response's own field is optional, and checked when present
got=$(jq -r '.result.jurisdiction // empty' <<<"$r")
[ -z "$got" ] || [ "$got" = "$jurisdiction" ] || { echo "$bucket is in jurisdiction $got, not $jurisdiction" >&2; exit 1; }
jq -r '.result | "  name \(.name), jurisdiction \(.jurisdiction // "default"), location \(.location // "?"), created \(.creation_date // "?"), storage class \(.storage_class // "?")"' <<<"$r"

echo "== config $cfg_file"
cfg_set DEV_FEED_ACCOUNT "$account"
cfg_set DEV_FEED_BUCKET "$bucket"
cfg_set DEV_FEED_JURISDICTION "$jurisdiction"
cfg_set S3_BUCKET "$bucket"
cfg_set S3_ENDPOINT "https://$account.$jurisdiction.r2.cloudflarestorage.com"
cfg_set S3_REGION auto

cat <<NEXT

== next (developer-feed/macos/README.md, Setup)
For this machine, a parent token: in the dashboard, R2, Manage API tokens,
Create Account API token, Object Read & Write, applied to this bucket only
(com.cloudflare.edge.r2.bucket.${account}_${jurisdiction}_$bucket), no expiry, no IP filter.
NEXT
