#!/usr/bin/env bash
# The developer feed's collectors end to end, in docker: the host tarball's
# launcher and fluent-bit in an Ubuntu container, with developer-feed/
# collectors/ as HOST_COLLECTOR_DIRS, writing to versitygw as a user that a
# bucket policy allows PutObject under the feed's root only, as the minted
# R2 credential does on a real bucket (developer-feed/cloudflare/). The
# tools whose files the collectors read are not needed: fixtures stand in
# for them. Never a real bucket (developer-feed/CLAUDE.md).
#
#   TARBALL=target/host/observability-to-blobs-host-dev-linux-arm64.tar.gz developer-feed/itest/run.sh
#
# Env:
#   TARBALL     the host tarball for this machine's architecture (required;
#               images/fluentbit/host/build.sh target/host builds it)
#   LOCAL_PORT  this machine's port for the gateway (default 17172)
#   KEEP=1      leave the containers and the network for a look around
#
# git: a repository with a commit pushed before the start, which must not
# ship, and one pushed after dev-feed-git's first run, which must, with its
# whole message and the push in the remote-tracking branch's reflog.
#
# claude-memory: memory files there before the start, shipped on the first
# run (a memory's content is state), then edited, deleted and added, each
# change shipped once.
#
# hammerspoon: a line in the activity log before the start, which must not
# ship, and one after, which must.
#
# cmux: a journal with cmux's schema, with rows before the start, which must
# not ship, and after dev-feed-cmux's first run, which must, once each.
#
# claude: the fixture is a ~/.claude with lines written before the agent's
# first start, which must not ship (live only), lines appended after it, a
# session and a subagent transcript that appear after it, a line longer than
# tail's default buffer, and lines written while the agent is restarted,
# which must ship once.
#
# backfill: dev-feed-backfill then ships what was written before the start,
# the claude categories in one run and the rest in another, as the agent's
# PutObject-only user: each line written once arrives once once readers
# drop the lines shipped by both (explore/sql/001-ingest.sql), with the
# fields the agent gives it, and nothing is shipped twice by the backfill.
# the scripts run in the containers are single-quoted on purpose
# shellcheck disable=SC2016
set -euo pipefail
cd "$(dirname "$0")/../.."

: "${TARBALL:?TARBALL: the host tarball, from images/fluentbit/host/build.sh}"
[ -f "$TARBALL" ] || { echo "no $TARBALL" >&2; exit 2; }
LOCAL_PORT=${LOCAL_PORT:-17172}
RUN=$(date +%s)
P=o2b-devfeed
NET=$P-net GW=$P-gw AGENT=$P-agent
BUCKET=dev-feed KEY_ROOT=dev1 CLUSTER=itest NODE=laptop
VGW_IMAGE=docker.io/versity/versitygw:v1.7.0
AWS_IMAGE=amazon/aws-cli:2.22.35
AGENT_IMAGE=$P-agent:ubuntu-24.04
AGENT_SECRET=$(openssl rand -hex 16)
TMP=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/dev-feed-itest.XXXXXX")" && pwd)
FAILED=0
S1=11111111-1111-4111-8111-111111111111
S2=22222222-2222-4222-8222-222222222222
PROJ=-Users-dev-repo

phase() { echo; echo "== $*"; }
pass() { echo "  PASS: $*"; }
failed() { echo "  FAIL: $*"; FAILED=$((FAILED + 1)); }
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else failed "$d"; fi; }

remove() {
  docker ps -aq --filter "name=^$P-" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
# shellcheck disable=SC2329 # invoked by the trap below
cleanup() {
  if [ "${KEEP:-}" = 1 ]; then echo "kept: docker ps --filter name=$P, and $TMP"; return; fi
  rm -rf "$TMP"
  remove
}
remove
trap cleanup EXIT

phase "gateway: versitygw, a bucket, and a user allowed PutObject under $KEY_ROOT/$CLUSTER/ only"
docker network create "$NET" >/dev/null
docker run -d --name "$GW" --network "$NET" -p "127.0.0.1:$LOCAL_PORT:7070" \
  -e ROOT_ACCESS_KEY=demoaccess -e ROOT_SECRET_KEY=demosecret \
  -e VGW_IAM_DIR=/iam -e VGW_ADMIN_PORT=:7080 --tmpfs /data --tmpfs /iam \
  "$VGW_IMAGE" posix /data >/dev/null
for _ in $(seq 30); do
  docker exec "$GW" versitygw admin --access demoaccess --secret demosecret --endpoint-url http://127.0.0.1:7080 list-users >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$GW" versitygw admin --access demoaccess --secret demosecret --endpoint-url http://127.0.0.1:7080 \
  create-user --access feedagent --secret "$AGENT_SECRET" --role user >/dev/null
aws_root() {
  docker run --rm --network "$NET" -e AWS_ACCESS_KEY_ID=demoaccess -e AWS_SECRET_ACCESS_KEY=demosecret \
    -e AWS_DEFAULT_REGION=us-east-1 -e AWS_PAGER= "$AWS_IMAGE" --endpoint-url "http://$GW:7070" "$@"
}
POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["feedagent"]},"Action":["s3:PutObject"],"Resource":["arn:aws:s3:::'$BUCKET'/'$KEY_ROOT'/'$CLUSTER'/*"]}]}'
aws_root s3api create-bucket --bucket "$BUCKET" >/dev/null
aws_root s3api put-bucket-policy --bucket "$BUCKET" --policy "$POLICY"

phase "agent: the host tarball, the feed's collectors, and a ~/.claude written before its first start"
docker build -q -t "$AGENT_IMAGE" - >/dev/null <<'DOCKERFILE'
FROM ubuntu:24.04
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
    libyaml-0-2 libsystemd0 ca-certificates git sqlite3 jq >/dev/null && rm -rf /var/lib/apt/lists/*
DOCKERFILE
# dev-feed-git and dev-feed-backfill for the container: no cgo, so a
# cross-compile from here
mkdir -p "$TMP/bin"
ARCH=$(docker version -f '{{.Server.Arch}}')
(cd developer-feed/git && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -o "$TMP/bin/dev-feed-git" .)
(cd developer-feed/backfill && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -o "$TMP/bin/dev-feed-backfill" .)
printf '[default]\naws_access_key_id = feedagent\naws_secret_access_key = %s\n' "$AGENT_SECRET" >"$TMP/s3-credentials"
# the feed's machine runs the feed's collectors, on by default from their
# own directory, and the heartbeat: the host's own defaults are turned off
printf -- '--no-collector.journal --no-collector.files\n--no-collector.node --no-collector.self\n' >"$TMP/args"
docker run -d --name "$AGENT" --network "$NET" \
  -v "$PWD/$TARBALL:/tmp/host.tar.gz:ro" -v "$PWD/developer-feed/collectors:/opt/developer-feed/collectors:ro" \
  -v "$TMP/s3-credentials:/etc/observability-host/s3-credentials:ro" -v "$TMP/args:/etc/observability-host/args:ro" \
  -v "$TMP/bin:/opt/developer-feed/built:ro" -v "$PWD/developer-feed/bin:/opt/developer-feed/bin:ro" \
  -e PATH=/opt/developer-feed/built:/opt/developer-feed/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e DEV_FEED_GIT_INTERVAL=3 -e DEV_FEED_CMUX_INTERVAL=3 -e DEV_FEED_MEMORY_INTERVAL=3 -e DEV_FEED_CMUX_JOURNAL=/root/cmux/journal.sqlite3 \
  -e CLUSTER_NAME=$CLUSTER -e NODE_NAME=$NODE -e "S3_BUCKET=$BUCKET" -e "S3_ENDPOINT=http://$GW:7070" -e S3_TLS=off \
  -e KEY_ROOT=$KEY_ROOT -e UPLOAD_TIMEOUT=5s -e HOST_COLLECTOR_DIRS=/opt/developer-feed/collectors \
  -e AWS_SHARED_CREDENTIALS_FILE=/etc/observability-host/s3-credentials \
  -e HEARTBEAT_COMMAND=/opt/observability-host/bin/observability-host-heartbeat \
  -e "S1=$S1" -e "S2=$S2" -e "PROJ=$PROJ" -e "RUN=$RUN" \
  "$AGENT_IMAGE" sh -c '
    set -e
    mkdir -p /opt/observability-host "$HOME/.claude/projects/$PROJ/memory" "$HOME/Reposoft/productivity-logs"
    echo "{\"ts\":\"2026-10-24T18:00:00.000Z\",\"event\":\"window_focused\",\"app\":\"before-start-$RUN\"}" >"$HOME/Reposoft/productivity-logs/hammerspoon-activity-log.jsonl"
    printf -- "---\nname: kept\n---\nmemory-before-start-$RUN\n" >"$HOME/.claude/projects/$PROJ/memory/kept.md"
    echo "memory-to-delete-$RUN" >"$HOME/.claude/projects/$PROJ/memory/gone.md"
    tar xzf /tmp/host.tar.gz -C /opt/observability-host
    echo "{\"type\":\"user\",\"sessionId\":\"$S1\",\"m\":\"before-start-$RUN\"}" >"$HOME/.claude/projects/$PROJ/$S1.jsonl"
    echo "{\"display\":\"history-before-start-$RUN\",\"sessionId\":\"$S1\",\"project\":\"/root/repo\"}" >"$HOME/.claude/history.jsonl"
    # a repository the history names, with a commit pushed before the start
    git config --global user.name Dev; git config --global user.email dev@example.com; git config --global init.defaultBranch main
    git init -q --bare /root/origin.git; git clone -q /root/origin.git /root/repo 2>/dev/null
    git -C /root/repo commit -q --allow-empty -m "before-start-$RUN"; git -C /root/repo push -q origin main
    # a cmux agent journal, its schema as cmux 0.65.2 creates it, with a row
    mkdir -p /root/cmux
    sqlite3 /root/cmux/journal.sqlite3 "PRAGMA journal_mode = wal;
      CREATE TABLE agent_journal (sequence INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT UNIQUE NOT NULL,
        schema_version INTEGER NOT NULL, kind TEXT NOT NULL, occurred_at_ms INTEGER NOT NULL, committed_at_ms INTEGER NOT NULL,
        source TEXT NOT NULL, agent_key TEXT NOT NULL, session_id TEXT, workspace_id TEXT, surface_id TEXT,
        unattributed_reason TEXT, is_subagent INTEGER NOT NULL, pending_work INTEGER NOT NULL,
        native_event TEXT, declared_phase TEXT, detail TEXT);
      INSERT INTO agent_journal VALUES (NULL, \"before-$RUN\", 1, \"agent.session.started\", 1792800000000, 1792800000000,
        \"claude\", \"k\", \"$S1\", \"ws-1\", \"sf-1\", NULL, 0, 0, \"SessionStart\", NULL, NULL);" >/dev/null
    # the launcher in a loop, so that killing fluent-bit is a restart, as
    # launchd and systemd do; each start recorded as dev-feed-agent does
    mkdir -p /var/lib/observability-host
    while :; do date -u +%s >>/var/lib/observability-host/agent-starts
      /opt/observability-host/bin/observability-host-launch || true; sleep 1; done' >/dev/null
in_agent() { docker exec "$AGENT" bash -c "$1"; }
for _ in $(seq 30); do docker logs "$AGENT" 2>&1 | grep -q 'collectors: claude claude-memory cmux git hammerspoon heartbeat$' && break; sleep 1; done
check "the feed's directory's defaults, claude, claude-memory, cmux, git and hammerspoon, run beside the heartbeat" sh -c "docker logs $AGENT 2>&1 | grep -q 'collectors: claude claude-memory cmux git hammerspoon heartbeat\$'"
sleep 6
for _ in $(seq 20); do in_agent 'ls /var/lib/observability-host/git/*.json' >/dev/null 2>&1 && break; sleep 1; done
check "dev-feed-git's first run recorded the repository" in_agent 'ls /var/lib/observability-host/git/*.json'

for _ in $(seq 20); do in_agent 'test -f /var/lib/observability-host/cmux-journal.seq' && break; sleep 1; done
check "dev-feed-cmux's first run recorded the journal's sequence" in_agent 'test "$(cat /var/lib/observability-host/cmux-journal.seq)" = 1'
in_agent 'sqlite3 /root/cmux/journal.sqlite3 "INSERT INTO agent_journal VALUES
  (NULL, \"after-1-$RUN\", 1, \"agent.turn.started\", 1792800001000, 1792800001000, \"claude\", \"k\", \"$S1\", \"ws-1\", \"sf-1\", NULL, 0, 0, \"UserPromptSubmit\", NULL, \"a detail that must not ship\"),
  (NULL, \"after-2-$RUN\", 1, \"agent.turn.completed\", 1792800002000, 1792800002000, \"claude\", \"k\", \"$S1\", \"ws-1\", \"sf-2\", NULL, 1, 0, \"Stop\", \"done\", NULL)"'

for _ in $(seq 20); do in_agent 'test -s /var/lib/observability-host/snapshot-claude-memory.tsv' && break; sleep 1; done
in_agent 'd=$HOME/.claude/projects/$PROJ/memory
  echo "memory-edited-$RUN" >>"$d/kept.md"
  rm "$d/gone.md"
  echo "memory-new-$RUN" >"$d/new.md"'

in_agent 'echo "{\"ts\":\"2026-10-24T18:01:00.123Z\",\"event\":\"window_focused\",\"app\":\"Code\",\"bundle_id\":\"com.microsoft.VSCode\",\"title\":\"after-start-$RUN\"}" >>"$HOME/Reposoft/productivity-logs/hammerspoon-activity-log.jsonl"'

phase "after the first start: a commit pushed and fetched"
in_agent 'git -C /root/repo commit -q --allow-empty -F - <<MSG
feature-after-start-$RUN: what was asked for

The requirement, in the words of the session.

Co-Authored-By: Claude <noreply@anthropic.com>
MSG
git -C /root/repo push -q origin main'

phase "after the first start: lines appended, a new session, a subagent, a long line"
# past the backfill's window after a start, so that the agent reads the new
# session whole and the backfill leaves it to the agent
for _ in $(seq 30); do [ $(( $(date -u +%s) - $(in_agent 'head -n 1 /var/lib/observability-host/agent-starts') )) -gt 16 ] && break; sleep 1; done
in_agent '
  d=$HOME/.claude/projects/$PROJ
  echo "{\"type\":\"assistant\",\"sessionId\":\"$S1\",\"m\":\"after-start-$RUN\"}" >>"$d/$S1.jsonl"
  printf "%s\n" "{\"type\":\"user\",\"sessionId\":\"$S2\",\"m\":\"new-session-1-$RUN\"}" "{\"type\":\"user\",\"sessionId\":\"$S2\",\"m\":\"new-session-2-$RUN\"}" >"$d/$S2.jsonl"
  mkdir -p "$d/$S1/subagents"
  echo "{\"type\":\"user\",\"isSidechain\":true,\"m\":\"subagent-$RUN\"}" >"$d/$S1/subagents/agent-a1.jsonl"
  # 3 MB in one line, beyond tail'"'"'s default 32 KB buffer
  printf "{\"type\":\"user\",\"sessionId\":\"$S2\",\"m\":\"long-$RUN\",\"pad\":\"%s\"}\n" "$(head -c 3000000 /dev/zero | tr "\0" x)" >>"$d/$S2.jsonl"
  echo "{\"display\":\"history-after-start-$RUN\",\"sessionId\":\"$S2\"}" >>"$HOME/.claude/history.jsonl"'
sleep 12

phase "restart: fluent-bit killed, lines written while it is down, then it starts again"
in_agent 'pkill -x fluent-bit'
in_agent 'echo "{\"type\":\"user\",\"sessionId\":\"$S1\",\"m\":\"during-restart-$RUN\"}" >>"$HOME/.claude/projects/$PROJ/$S1.jsonl"'
for _ in $(seq 20); do [ "$(docker logs "$AGENT" 2>&1 | grep -c 'collectors: claude claude-memory cmux git hammerspoon heartbeat$')" -ge 2 ] && break; sleep 1; done
check "the launcher started fluent-bit again" sh -c "[ \$(docker logs $AGENT 2>&1 | grep -c 'collectors: claude claude-memory cmux git hammerspoon heartbeat\$') -ge 2 ]"
in_agent 'echo "{\"type\":\"user\",\"sessionId\":\"$S1\",\"m\":\"after-restart-$RUN\"}" >>"$HOME/.claude/projects/$PROJ/$S1.jsonl"'
sleep 15

phase "bucket: the feed's rows, through developer-feed/sql/feed.sql"
cat >"$TMP/init.sql" <<SQL
.output /dev/null
INSTALL httpfs; LOAD httpfs; SET TimeZone='UTC';
CREATE OR REPLACE SECRET dev_feed_itest (TYPE s3, KEY_ID 'demoaccess', SECRET 'demosecret',
  REGION 'us-east-1', ENDPOINT '127.0.0.1:$LOCAL_PORT', USE_SSL false, URL_STYLE 'path',
  SCOPE 's3://$BUCKET');
SET VARIABLE raw_files = 's3://$BUCKET/$KEY_ROOT/$CLUSTER/*/*/*/*.jsonl.zst';
.read $PWD/developer-feed/sql/feed.sql
.output
SQL
q() { duckdb -noheader -csv -c ".read $TMP/init.sql" -c "$1" 2>&1; }
assert() { # assert <description> <SQL that yields true>
  local got
  got=$(q "$2") || true
  if [ "$got" = true ]; then pass "$1"; else failed "$1 (got: $got)"; fi
}
C="FROM feed WHERE \"table\" = 'claude'"
assert "nothing written before the first start is shipped: live only" \
  "SELECT count(*) = 0 $C AND json LIKE '%before-start-$RUN%'"
assert "a line appended to a transcript after the start, once, with its project and session" \
  "SELECT count(*) = 1 AND bool_and(kind = 'transcript' AND project = '$PROJ' AND session_id = '$S1') $C AND json LIKE '%\"after-start-$RUN\"%'"
assert "a session that appears after the start, from its first line" \
  "SELECT count(*) = 2 AND bool_and(session_id = '$S2') $C AND json LIKE '%new-session-%-$RUN%'"
assert "a subagent transcript, with its parent's session and project" \
  "SELECT count(*) = 1 AND bool_and(kind = 'subagent' AND session_id = '$S1' AND project = '$PROJ') $C AND json LIKE '%subagent-$RUN%'"
assert "a 3 MB line, whole" \
  "SELECT count(*) = 1 AND bool_and(length(json) > 3000000) $C AND json LIKE '%long-$RUN%'"
assert "a prompt appended to the history, as kind history" \
  "SELECT count(*) = 1 AND bool_and(kind = 'history' AND path LIKE '%/.claude/history.jsonl') $C AND json LIKE '%history-after-start-$RUN%'"
assert "the line written while fluent-bit was down, and the one after, once each" \
  "SELECT count(*) = 2 AND count(DISTINCT json) = 2 $C AND (json LIKE '%during-restart-$RUN%' OR json LIKE '%after-restart-$RUN%')"
assert "no line is shipped twice across the restart" \
  "SELECT count(*) = count(DISTINCT json) $C AND kind <> 'memory'"
assert "the line is verbatim: it parses as the JSON that was written" \
  "SELECT bool_and(json_extract_string(json, '\$.m') = 'after-start-$RUN') $C AND json LIKE '%\"after-start-$RUN\"%'"
assert "a claude line in the objects has the contract's fields and no other" \
  "SELECT bool_and(list_has_all(['table','kind','cluster','node','path','project','session_id','json','ts'], json_keys(json))) FROM read_json_objects('s3://$BUCKET/$KEY_ROOT/$CLUSTER/*/*/*/*.jsonl.zst', format='newline_delimited', compression='zstd') WHERE json->>'table' = 'claude' AND json->>'kind' <> 'memory'"
G="FROM feed WHERE \"table\" = 'git'"
assert "git: the remote, as the repository's" \
  "SELECT count(*) >= 1 AND bool_and(repo = '/root/repo' AND fetch_url = '/root/origin.git') $G AND kind = 'remote' AND remote = 'origin'"
assert "git: the commit pushed after the start, once, with its whole message and its parent" \
  "SELECT count(*) = 1 AND bool_and(message LIKE 'feature-after-start-$RUN: what was asked for%Co-Authored-By: Claude <noreply@anthropic.com>' AND len(parents) = 1 AND author_email = 'dev@example.com') $G AND kind = 'commit' AND message LIKE 'feature-after-start-$RUN%'"
assert "git: nothing from before the start: live only" \
  "SELECT count(*) = 0 $G AND kind = 'commit' AND message = 'before-start-$RUN'"
assert "git: the push, in the remote-tracking branch's reflog, at the commit" \
  "SELECT count(*) = 1 $G AND kind = 'ref-update' AND ref = 'refs/remotes/origin/main' AND message = 'update by push' AND sha = (SELECT sha $G AND kind = 'commit' AND message LIKE 'feature-after-start-$RUN%')"
M="FROM feed WHERE \"table\" = 'claude' AND kind = 'memory'"
assert "memory: a file there before the start, on the first run, whole, with its project" \
  "SELECT count(*) = 1 AND bool_and(project = '$PROJ' AND NOT deleted AND content LIKE '---%memory-before-start-$RUN%' AND content NOT LIKE '%memory-edited%') $M AND path LIKE '%/kept.md' AND content NOT LIKE '%memory-edited%'"
assert "memory: the same file after an edit, whole again, with another hash" \
  "SELECT count(*) = 1 AND bool_and(content LIKE '%memory-before-start-$RUN%memory-edited-$RUN%') AND (SELECT count(DISTINCT sha256) $M AND path LIKE '%/kept.md') = 2 $M AND path LIKE '%/kept.md' AND content LIKE '%memory-edited%'"
assert "memory: a file deleted, as a deleted row without content" \
  "SELECT count(*) = 1 AND bool_and(content IS NULL) $M AND path LIKE '%/gone.md' AND deleted"
assert "memory: a file that appeared after the start" \
  "SELECT count(*) = 1 $M AND path LIKE '%/new.md' AND content = 'memory-new-$RUN' || chr(10)"
H="FROM feed WHERE \"table\" = 'hammerspoon'"
assert "hammerspoon: the line logged after the start, verbatim, and not the one before" \
  "SELECT count(*) = 1 AND bool_and(json_extract_string(json, '\$.bundle_id') = 'com.microsoft.VSCode' AND path LIKE '%/hammerspoon-activity-log.jsonl') $H AND json LIKE '%after-start-$RUN%' AND (SELECT count(*) $H AND json LIKE '%before-start-$RUN%') = 0"
X="FROM feed WHERE \"table\" = 'cmux'"
assert "cmux: the journal's rows from after the first run, once each, in order, with their workspace and pane" \
  "SELECT count(*) = 2 AND list(event_id ORDER BY sequence) = ['after-1-$RUN', 'after-2-$RUN'] AND list(surface_id ORDER BY sequence) = ['sf-1', 'sf-2'] AND bool_and(session_id = '$S1' AND workspace_id = 'ws-1') $X"
assert "cmux: nothing from before the start: live only" "SELECT count(*) = 0 $X AND event_id = 'before-$RUN'"
assert "cmux: the times and the subagent flag as the journal has them" \
  "SELECT bool_and(occurred_at >= TIMESTAMP '2026-10-20') AND count(*) FILTER (WHERE is_subagent) = 1 $X"
assert "cmux: no detail column: metadata only" \
  "SELECT count(*) = 0 FROM read_json_objects('s3://$BUCKET/$KEY_ROOT/$CLUSTER/*/*/*/*.jsonl.zst', format='newline_delimited', compression='zstd') WHERE json->>'table' = 'cmux' AND (json->>'detail' IS NOT NULL OR json::VARCHAR LIKE '%must not ship%')"
q "SELECT \"table\", list(DISTINCT kind) AS kinds, count(*) AS rows FROM feed WHERE \"table\" IN ('claude', 'git', 'cmux') GROUP BY ALL ORDER BY 1" | sed 's/^/  (/; s/$/)/'

phase "backfill: what was written before the start, the claude categories first, then the rest"
backfill() {
  docker exec -e AWS_ACCESS_KEY_ID=feedagent -e "AWS_SECRET_ACCESS_KEY=$AGENT_SECRET" -e HOST_STATE_DIR=/var/lib/observability-host \
    "$AGENT" /opt/developer-feed/built/dev-feed-backfill "$@"
}
run_id() { sed -n 's/^dev-feed-backfill: run \([0-9TZ]*\),.*/\1/p' "$1"; }
CLAUDE_CATEGORIES=claude-transcript,claude-subagent,claude-history
backfill --category "$CLAUDE_CATEGORIES" >"$TMP/backfill-plan.txt"
sleep 1
backfill --ship --category "$CLAUDE_CATEGORIES" >"$TMP/backfill-1.txt"
sleep 1
backfill --ship >"$TMP/backfill-2.txt"
sleep 1
backfill --ship >"$TMP/backfill-3.txt"
B0=$(run_id "$TMP/backfill-plan.txt") B1=$(run_id "$TMP/backfill-1.txt") B2=$(run_id "$TMP/backfill-2.txt")
sed 's/^/  | /' "$TMP/backfill-1.txt" "$TMP/backfill-2.txt"
check "a third run has nothing to ship: every category is handed off to its collector" grep -qx 'nothing to ship' "$TMP/backfill-3.txt"
assert "without --ship, nothing is shipped" "SELECT count(*) = 0 FROM feed WHERE backfill = '$B0'"
BF="FROM feed WHERE backfill IS NOT NULL"
assert "backfill: the lines written before the start, once each, by the run of their category" \
  "SELECT list(\"table\" || '/' || coalesce(kind, '') || '@' || backfill ORDER BY \"table\", kind) = ['claude/history@$B1', 'claude/transcript@$B1', 'hammerspoon/@$B2'] $BF AND json LIKE '%before-start-$RUN%'"
assert "backfill: the second run shipped no claude line: those were backfilled" \
  "SELECT count(*) = 0 FROM feed WHERE backfill = '$B2' AND \"table\" = 'claude'"
assert "backfill: the session and the subagent that appeared after the start are left to the agent, which read them whole" \
  "SELECT count(*) = 0 $BF AND (json LIKE '%new-session-%' OR json LIKE '%subagent-$RUN%')"
assert "backfill: no line twice" \
  "SELECT count(*) = count(DISTINCT (kind, path, json)) $BF AND json IS NOT NULL"
assert "backfill: a line both shipped has the fields the agent gives it" \
  "SELECT count(*) >= 4 AND bool_and(b.\"table\" = a.\"table\" AND b.kind IS NOT DISTINCT FROM a.kind AND b.cluster = a.cluster AND b.node = a.node AND b.project IS NOT DISTINCT FROM a.project AND b.session_id IS NOT DISTINCT FROM a.session_id) FROM feed b JOIN feed a ON a.json = b.json AND a.path = b.path AND a.backfill IS NULL AND b.backfill IS NOT NULL"
assert "backfill: a claude line in the objects has the contract's fields and backfill, and no other" \
  "SELECT count(*) > 0 AND bool_and(list_has_all(['table','kind','cluster','node','path','project','session_id','json','ts','backfill'], json_keys(json))) FROM read_json_objects('s3://$BUCKET/$KEY_ROOT/$CLUSTER/*/*/*/*.jsonl.zst', format='newline_delimited', compression='zstd') WHERE json->>'table' = 'claude' AND json->>'backfill' IS NOT NULL"
assert "backfill: the objects are under the hour of their lines' time, the source's" \
  "SELECT count(*) > 0 AND bool_and(filename LIKE '%/$KEY_ROOT/$CLUSTER/' || strftime(ts::TIMESTAMP, '%Y-%m-%d/%H') || '/$NODE/%') $BF"
assert "backfill: a transcript line at the time in it" \
  "SELECT count(*) = 1 $BF AND json LIKE '%\"after-start-$RUN\"%' AND kind = 'transcript'"
assert "backfill cmux: the row before the start, and the collector's rows again, as the collector ships them" \
  "SELECT list(event_id ORDER BY sequence) = ['before-$RUN', 'after-1-$RUN', 'after-2-$RUN'] AND (SELECT count(*) = 2 AND bool_and(b.kind = a.kind AND b.sequence = a.sequence AND b.occurred_at = a.occurred_at AND b.committed_at = a.committed_at AND b.session_id = a.session_id AND b.workspace_id = a.workspace_id AND b.surface_id = a.surface_id AND b.is_subagent = a.is_subagent AND b.pending_work = a.pending_work AND b.native_event = a.native_event AND b.declared_phase IS NOT DISTINCT FROM a.declared_phase) FROM feed b JOIN feed a USING (event_id) WHERE b.backfill IS NOT NULL AND a.backfill IS NULL AND a.\"table\" = 'cmux') $BF AND \"table\" = 'cmux'"
assert "backfill git: the commit pushed before the start, with its push, and none the agent's first run made a remote of again" \
  "SELECT (SELECT count(*) = 1 $BF AND \"table\" = 'git' AND kind = 'commit' AND message = 'before-start-$RUN') AND (SELECT count(*) = 1 $BF AND \"table\" = 'git' AND kind = 'ref-update' AND ref = 'refs/remotes/origin/main' AND message = 'update by push' AND sha = (SELECT sha FROM feed WHERE kind = 'commit' AND message = 'before-start-$RUN' LIMIT 1)) AND (SELECT count(*) = 0 $BF AND \"table\" = 'git' AND kind = 'remote')"
assert "backfill git: a commit both shipped, with the agent's message, parents and author" \
  "SELECT count(*) = 1 AND bool_and(b.message = a.message AND b.parents = a.parents AND b.author_email = a.author_email AND b.author_time = a.author_time AND b.committer_time = a.committer_time) FROM feed b JOIN feed a ON a.sha = b.sha AND a.kind = 'commit' AND b.kind = 'commit' AND a.backfill IS NULL AND b.backfill IS NOT NULL"
assert "backfill memory: none: the collector's first run shipped every memory" "SELECT count(*) = 0 $BF AND kind = 'memory'"

# explore's view of every line once (explore/sql/001-ingest.sql)
got=$(duckdb -noheader -csv -c ".read $TMP/init.sql" -c ".read $PWD/developer-feed/explore/sql/001-ingest.sql" -c ".output /dev/null" \
  -c "INSERT INTO ingest.lines SELECT filename, json FROM read_json_objects('s3://$BUCKET/$KEY_ROOT/$CLUSTER/*/*/*/*.jsonl.zst', format='newline_delimited', compression='zstd', filename=true)" \
  -c ".output" -c "SELECT
    (SELECT count(*) FROM ingest.all_rows WHERE \"table\" IN ('claude', 'hammerspoon', 'cmux')) > (SELECT count(*) FROM ingest.rows WHERE \"table\" IN ('claude', 'hammerspoon', 'cmux'))
    AND (SELECT count(*) = count(DISTINCT (kind, line->>'path', line->>'json')) FROM ingest.rows WHERE \"table\" IN ('claude', 'hammerspoon') AND kind IS DISTINCT FROM 'memory')
    AND (SELECT count(*) = count(DISTINCT line->>'event_id') FROM ingest.rows WHERE \"table\" = 'cmux')
    AND (SELECT count(*) = 1 FROM ingest.rows WHERE line->>'json' LIKE '%\"after-start-$RUN\"%' AND kind = 'transcript')
    AND (SELECT count(*) = 2 FROM ingest.rows WHERE \"table\" = 'claude' AND kind = 'memory' AND line->>'path' LIKE '%/kept.md')" 2>&1) || true
if [ "$got" = true ]; then pass "explore: a line shipped by both is there once, and memory versions are kept"; else failed "explore: a line shipped by both is there once (got: $got)"; fi

if [ "$FAILED" -gt 0 ]; then echo "--- agent"; docker logs --tail 30 "$AGENT" 2>&1 | grep -v 'unattended-upgrades' || true; fi
echo
echo "developer-feed itest: $FAILED failed"
exit "$FAILED"
