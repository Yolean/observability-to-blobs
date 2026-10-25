#!/usr/bin/env bash
# Explore the feed in DuckDB (developer-feed/explore/README.md): ingests the
# bucket's objects from <since> back, then again every <every> minutes,
# into a database in the user-level state, with the schemas of sql/ and then
# the config's explore/*.sql, and serves it with quack on localhost only,
# for duckdb -ui and other clients.
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
#   --clusters  the clusters to read, CLUSTER_NAMEs (default: this
#               machine's); the machines of one cluster are its nodes, told
#               apart by node, a column of every row
#
# The read credential is minted with dev-feed-r2 from a read-only parent
# token (the script says how to make one) and goes to DuckDB as a temporary
# secret on stdin: never a file or a command line. So does quack's token,
# which is new every run and printed once. Ctrl-C stops it.
#
# Ingests list the bucket a UTC day at a time, a prefix per cluster and day,
# and commit each day with the objects read: a stopped first ingest keeps
# the days it finished, and says how far it got. Later ingests list today
# and yesterday. A notebook triggers one at once by writing the file the
# script names (<state>/explore/refresh), with COPY.
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

utc() { date -u -r "$1" "$2" 2>/dev/null || date -u -d "@$1" "$2"; }

# the read credential as DuckDB's temporary secret, for the bucket only
secret_sql() {
  echo "LOAD httpfs;"
  printf "CREATE OR REPLACE TEMPORARY SECRET feed_read (TYPE s3, KEY_ID %s, SECRET %s, SESSION_TOKEN %s, REGION %s, ENDPOINT %s, URL_STYLE 'path', USE_SSL %s, SCOPE %s);\n" \
    "$(q "$key_id")" "$(q "$secret")" "$(q "$session")" "$(q "$region")" "$(q "$host")" "$ssl" "$(q "s3://$bucket/")"
}

# day_globs <YYYY-MM-DD>: the day's objects of every cluster, a listing of
# <root>/<cluster>/<day>/ each: two requests and one more per 1000 objects,
# where a listing per hour is two requests for each of its 24 hours
day_globs() {
  local c first=1
  for c in ${clusters//,/ }; do
    [ $first = 1 ] || printf ' UNION ALL '
    printf "SELECT file FROM glob(%s)" "$(q "s3://$bucket/$root/$c/$1/*/*/*.jsonl.zst")"
    first=0
  done
}

# ingest <from epoch>: the objects from that hour on, not read before, a
# UTC day at a time: each day's in one transaction with the record of them,
# and a line for each day that had any. Then a row in ingest.runs.
ingest() {
  local from_hour t now day
  from_hour=$(utc "$1" +%Y-%m-%d/%H)
  now=$(date -u +%s)
  echo "SET VARIABLE run_started = now(); SET VARIABLE run_new = 0; SET VARIABLE run_days = 0;"
  t=$(( $1 / 86400 * 86400 ))
  while [ "$t" -le "$now" ]; do
    day=$(utc "$t" +%Y-%m-%d)
    cat <<SQL
BEGIN;
SET VARIABLE new_files = (SELECT coalesce(list(file), []::VARCHAR[]) FROM ($(day_globs "$day"))
  WHERE regexp_extract(file, '/([0-9]{4}-[0-9]{2}-[0-9]{2}/[0-9]{2})/[^/]+/[^/]+\$', 1) >= $(q "$from_hour")
    AND file NOT IN (SELECT filename FROM ingest.files));
INSERT INTO ingest.lines SELECT filename, json FROM read_json_objects(list_concat([$(q "$empty")], getvariable('new_files')),
  format = 'newline_delimited', compression = 'auto_detect', filename = true) WHERE filename <> $(q "$empty");
INSERT INTO ingest.files SELECT unnest(getvariable('new_files')), now();
COMMIT;
SET VARIABLE run_new = getvariable('run_new') + len(getvariable('new_files'));
SET VARIABLE run_days = getvariable('run_days') + 1;
SELECT format('{}: {} new object{}', $(q "$day"), len(getvariable('new_files')), CASE len(getvariable('new_files')) WHEN 1 THEN '' ELSE 's' END)
  WHERE len(getvariable('new_files')) > 0;
SQL
    t=$(( t + 86400 ))
  done
  cat <<SQL
INSERT INTO ingest.runs VALUES (getvariable('run_started'), now(), $(q "$from_hour"), $(q "$clusters"), getvariable('run_days'), getvariable('run_new'));
SELECT format('ingested at {}: {} new, from {} days; {} objects, {} lines in all',
  strftime(now(), '%H:%M:%S'), getvariable('run_new'), getvariable('run_days'),
  (SELECT count(*) FROM ingest.files), (SELECT count(*) FROM ingest.lines));
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
  # the setup's results are no news; errors still show
  echo ".output /dev/null"
  secret_sql
  for f in "$here"/sql/*.sql; do
    cat "$f"
    printf "INSERT OR REPLACE INTO ingest.migrations VALUES (%s, now());\n" "$(q "$(basename "$f")")"
  done
  # the developer's own schemas, after the feed's (macos/README.md, Your own sources)
  for f in "$OBSERVABILITY_HOST_CONFIG"/explore/*.sql; do
    [ -f "$f" ] || continue
    cat "$f"
    printf "INSERT OR REPLACE INTO ingest.migrations VALUES (%s, now());\n" "$(q "config/$(basename "$f")")"
  done
  # the ingests' lines, one a line
  echo ".mode list"
  echo ".headers off"
  printf "CALL quack_serve(%s, token = %s);\n" "$(q "quack:localhost:$port")" "$(q "$token")"
  echo ".output"
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
    # yesterday and today, whole: an object lands in its first line's hour,
    # and may arrive after that hour is over; a listing per day costs the
    # same for the whole day
    ingest "$(( $(date -u +%s) / 86400 * 86400 - 86400 ))"
  done
}

# every cluster named must have objects: a LIMIT 1 listing each, two
# requests; a node's name for a cluster's would otherwise list nothing for
# every day of <since>
echo "== clusters: $clusters"
found=$( { secret_sql; for c in ${clusters//,/ }; do
    printf "SELECT %s, count(*) FROM (SELECT 1 FROM glob(%s) LIMIT 1);\n" "$(q "$c")" "$(q "s3://$bucket/$root/$c/*/*/*/*.jsonl.zst")"
  done; } | y-duckdb -noheader -csv -no-agent 2>&1) || { echo "explore: listing $bucket failed: $found" >&2; exit 1; }
present='' missing=''
for c in ${clusters//,/ }; do
  if grep -qx "$c,1" <<<"$found"; then present=${present:+$present,}$c
  elif grep -qx "$c,0" <<<"$found"; then missing="$missing $c"
  else echo "explore: listing $root/$c/ in $bucket failed: $found" >&2; exit 1
  fi
done
if [ -n "$missing" ]; then
  echo "explore: no objects under$(for c in $missing; do printf ' %s' "$root/$c/"; done) in $bucket." >&2
  echo "  --clusters takes CLUSTER_NAMEs, the first part of a key: $root/<cluster>/<date>/<hour>/<node>/." >&2
  echo "  Machines that share a cluster, such as a backfill's with NODE_NAME set, are its nodes:" >&2
  echo "  read the cluster, and tell them apart by node, a column of every row." >&2
  [ -n "$present" ] || exit 2
  echo "  Going on with $present." >&2
  clusters=$present
fi

echo "== $db, from $since back, every $every min, clusters: $clusters"
feed | y-duckdb "$db"
