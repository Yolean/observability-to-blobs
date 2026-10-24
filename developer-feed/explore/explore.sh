#!/usr/bin/env bash
# Explore the feed in DuckDB (developer-feed/explore/README.md): ingests the
# bucket's objects from <since> back, then again every <every> minutes,
# into a database in the user-level state, with the schemas of sql/, and
# serves it with quack on localhost only, for duckdb -ui and other clients.
#
#   developer-feed/explore/explore.sh [<since>] [--every <minutes>] [--ttl <ttl>]
#                                     [--port <port>] [--clusters <a,b>]
#
#   <since>     how far back the first ingest reaches: N with m, h or d
#               (default 24h)
#   --every     minutes between ingests (default 5)
#   --ttl       the read credential's lifetime, and so the session's
#               (default 12h)
#   --port      quack's port on localhost (default 18338)
#   --clusters  the machines to read (default: this machine's CLUSTER_NAME)
#
# The read credential is minted with dev-feed-r2 from a read-only parent
# token (the script says how to make one) and goes to DuckDB as a temporary
# secret on stdin: never a file or a command line. So does quack's token,
# which is new every run and printed once. Ctrl-C stops it.
#
# A notebook triggers an ingest at once by writing the file the script names
# (<state>/explore/refresh), with COPY.
#
# Env: DEV_FEED_EXPLORE_CREDENTIAL_COMMAND, a command that prints the
# credential_process JSON instead of minting one, is for tests.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=../lib/config.sh
. "$here/../lib/config.sh"

since=24h every=5 ttl=12h port=18338 clusters=
while [ $# -gt 0 ]; do
  case $1 in
    --every) every=$2; shift 2;;
    --ttl) ttl=$2; shift 2;;
    --port) port=$2; shift 2;;
    --clusters) clusters=$2; shift 2;;
    -h|--help) sed -n '2,25p' "$0"; exit 0;;
    -*) echo "explore: unknown flag $1" >&2; exit 2;;
    *) since=$1; shift;;
  esac
done
case $since in
  *m) since_s=$(( ${since%m} * 60 ));;
  *h) since_s=$(( ${since%h} * 3600 ));;
  *d) since_s=$(( ${since%d} * 86400 ));;
  *) echo "explore: <since> is N with m, h or d, not $since" >&2; exit 2;;
esac
case $every in ''|*[!0-9]*|0) echo "explore: --every is whole minutes, not $every" >&2; exit 2;; esac

account=$(cfg_get DEV_FEED_ACCOUNT)
bucket=$(cfg_get S3_BUCKET)
endpoint=$(cfg_get S3_ENDPOINT)
root=$(cfg_get KEY_ROOT); root=${root:-dev1}
region=$(cfg_get S3_REGION); region=${region:-auto}
[ -n "$clusters" ] || clusters=$(cfg_get CLUSTER_NAME)
[ -n "$bucket" ] && [ -n "$endpoint" ] && [ -n "$clusters" ] \
  || { echo "explore: no S3_BUCKET, S3_ENDPOINT or CLUSTER_NAME in $cfg_file (developer-feed/macos/README.md)" >&2; exit 2; }
command -v y-duckdb >/dev/null || { echo "explore: needs y-duckdb (ystack/bin)" >&2; exit 2; }
r2=$OBSERVABILITY_HOST_PREFIX/bin/dev-feed-r2
# one explore database at a time: a process that has it open is another
# explore.sh (or a client of it opened directly); checked before minting
dir=$OBSERVABILITY_HOST_STATE/explore
db=$dir/feed.duckdb
if holders=$(lsof -t "$db" "$db.wal" 2>/dev/null | sort -u | tr '\n' ' ') && [ -n "$holders" ]; then
  echo "explore: $db is open in process $holders($(ps -o command= -p "${holders%% *}" | cut -c1-80)); stop that first" >&2
  exit 2
fi
if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "explore: port $port is in use; --port another" >&2; exit 2
fi

echo "== read credential"
if [ -n "${DEV_FEED_EXPLORE_CREDENTIAL_COMMAND:-}" ]; then
  cred=$($DEV_FEED_EXPLORE_CREDENTIAL_COMMAND)
else
  [ -x "$r2" ] || { echo "explore: no $r2 (developer-feed/macos/install.sh)" >&2; exit 2; }
  parent=$(cfg_get DEV_FEED_READ_PARENT_KEY_ID)
  if [ -z "$parent" ]; then
    # the R2 token form has no "current IP" button: this machine's public
    # addresses, as Cloudflare sees them
    ips=$(for v in 4 6; do curl -"$v" -fsS -m 5 https://cloudflare.com/cdn-cgi/trace 2>/dev/null | sed -n 's/^ip=//p'; done | sort -u | tr '\n' ' ')
    cat <<HOWTO
Reading the feed needs a read-only parent token, which this machine keeps
nothing of but its Access Key ID. Create one at
  https://dash.cloudflare.com/${account:-<account id>}/r2/api-tokens
1. Create Account API token.
2. Token name: dev-feed-read.
3. Permissions: Object Read only.
4. Specify bucket(s): Apply to specific buckets only, and select $bucket.
5. TTL: as long as you want to explore with it; delete it when done.
6. Client IP Address Filtering: Allow, the public IPs you explore from; this
   machine's now: ${ips:-unknown (no answer from cloudflare.com/cdn-cgi/trace)}
   A read credential reads everything; a refused read away from them is harmless.
7. Create Account API token, then copy the Access Key ID and the Secret Access Key.
HOWTO
    printf 'Access Key ID of the read token: ' >&2
    IFS= read -r parent
  fi
  echo "minting a read credential for $root/ from $parent, valid $ttl; paste the token's Secret Access Key" >&2
  cred=$("$r2" mint --role read --prefix "$root/" --ttl "$ttl" --parent-key-id "$parent")
fi
field() { jq -r --arg k "$1" '.[$k] // empty' <<<"$cred"; }
key_id=$(field AccessKeyId) secret=$(field SecretAccessKey) session=$(field SessionToken) expiration=$(field Expiration)
[ -n "$key_id" ] && [ -n "$secret" ] || { echo "explore: no credential" >&2; exit 1; }
expires_s=$(date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "$expiration" +%s 2>/dev/null || date -u -d "$expiration" +%s 2>/dev/null || echo 0)
echo "valid until ${expiration:-unknown}"

case $endpoint in
  https://*) host=${endpoint#https://} ssl=true;;
  http://*) host=${endpoint#http://} ssl=false;;
  *) echo "explore: S3_ENDPOINT $endpoint has no scheme" >&2; exit 2;;
esac
host=${host%/}
(umask 077 && mkdir -p "$dir")
empty=$dir/empty.jsonl
: >"$empty"
refresh=$dir/refresh
rm -f "$refresh"
token=$(openssl rand -hex 24)
q() { printf "'%s'" "${1//\'/\'\'}"; }

# hour_globs <from epoch>: one ListObjectsV2 prefix per cluster and hour,
# from that hour to this one, UTC as the keys are; listing the whole
# bucket every few minutes would cost R2 operations
hour_globs() {
  local t now c first=1
  now=$(date -u +%s)
  t=$(( $1 / 3600 * 3600 ))
  while [ "$t" -le "$now" ]; do
    for c in ${clusters//,/ }; do
      [ $first = 1 ] || printf ' UNION ALL '
      printf "SELECT file FROM glob(%s)" "$(q "s3://$bucket/$root/$c/$(date -u -r "$t" +%Y-%m-%d/%H 2>/dev/null || date -u -d "@$t" +%Y-%m-%d/%H)/*/*.jsonl.zst")"
      first=0
    done
    t=$(( t + 3600 ))
  done
}

# ingest <from epoch>: the objects of those hours not read before, in one
# transaction with the record of them
ingest() {
  cat <<SQL
BEGIN;
SET VARIABLE listed = (SELECT coalesce(list(file), []::VARCHAR[]) FROM ($(hour_globs "$1")));
SET VARIABLE new_files = (SELECT coalesce(list(f), []::VARCHAR[]) FROM unnest(coalesce(getvariable('listed'), []::VARCHAR[])) t(f)
  WHERE f NOT IN (SELECT filename FROM ingest.files));
INSERT INTO ingest.lines SELECT filename, json FROM read_json_objects(list_concat([$(q "$empty")], getvariable('new_files')),
  format = 'newline_delimited', compression = 'auto_detect', filename = true) WHERE filename <> $(q "$empty");
INSERT INTO ingest.files SELECT unnest(getvariable('new_files')), now();
COMMIT;
SELECT strftime(now(), '%H:%M:%S') AS ingested, len(getvariable('new_files')) AS new_objects,
  (SELECT count(*) FROM ingest.lines) AS lines, (SELECT count(*) FROM ingest.files) AS objects;
SQL
}

# only loopback may reach quack: anything else listening on the port stops it
loopback_only() {
  local addrs
  addrs=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | awk 'NR > 1 { print $9 }')
  [ -n "$addrs" ] || { echo "explore: nothing listens on $port" >&2; return 1; }
  if echo "$addrs" | grep -vqE '^(127\.0\.0\.1|\[::1\]):'; then
    echo "explore: quack listens beyond loopback ($(echo "$addrs" | tr '\n' ' ')); stopping" >&2
    return 1
  fi
}

feed() {
  echo ".bail off"
  echo "LOAD httpfs;"
  printf "CREATE OR REPLACE TEMPORARY SECRET feed_read (TYPE s3, KEY_ID %s, SECRET %s, SESSION_TOKEN %s, REGION %s, ENDPOINT %s, URL_STYLE 'path', USE_SSL %s, SCOPE %s);\n" \
    "$(q "$key_id")" "$(q "$secret")" "$(q "$session")" "$(q "$region")" "$(q "$host")" "$ssl" "$(q "s3://$bucket")"
  for f in "$here"/sql/*.sql; do
    cat "$f"
    printf "INSERT OR REPLACE INTO ingest.migrations VALUES (%s, now());\n" "$(q "$(basename "$f")")"
  done
  printf "CALL quack_serve(%s, token = %s);\n" "$(q "quack:localhost:$port")" "$(q "$token")"
  sleep 3
  loopback_only || return 1
  cat >&2 <<HELLO

== serving $db with quack on localhost:$port, until Ctrl-C
open it in the DuckDB UI:
  y-duckdb -cmd "ATTACH 'quack:localhost:$port' AS feed (TOKEN '$token');" -ui
then, for example:
  FROM feed.claude.prompts ORDER BY occurred_at DESC;
ingest now, from a notebook, rather than in $every min:
  COPY (SELECT 1) TO '$refresh' (FORMAT csv);
an agent that analyzes the feed ($here/ANALYZE.md), in another terminal:
  DEV_FEED_QUACK_TOKEN=$token DEV_FEED_QUACK_PORT=$port claude --allowedTools "Bash($here/query.sh:*)" "Read $here/ANALYZE.md, then: what did I work on in the last 4 hours?"

HELLO
  ingest "$(( $(date -u +%s) - since_s ))"
  while :; do
    # every --every minutes, or at once when a notebook writes the refresh file
    waited=0
    while [ "$waited" -lt $(( every * 60 )) ] && [ ! -e "$refresh" ]; do
      sleep 2
      waited=$(( waited + 2 ))
    done
    rm -f "$refresh"
    if [ "$expires_s" -gt 0 ] && [ "$(date -u +%s)" -ge "$expires_s" ]; then
      echo "explore: the read credential expired at $expiration; still serving, no more ingests; run explore.sh again to go on" >&2
      while :; do sleep 3600; done
    fi
    # the last three hours: an object lands in its first line's hour, and
    # may arrive after that hour is over
    ingest "$(( $(date -u +%s) - 3 * 3600 ))"
  done
}

echo "== $db, from $since back, every $every min, machines: $clusters"
feed | y-duckdb "$db"
