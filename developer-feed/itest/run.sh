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
# cmux: a journal with cmux's schema, with rows before the start, which must
# not ship, and after dev-feed-cmux's first run, which must, once each.
#
# claude: the fixture is a ~/.claude with lines written before the agent's
# first start, which must not ship (live only), lines appended after it, a
# session and a subagent transcript that appear after it, a line longer than
# tail's default buffer, and lines written while the agent is restarted,
# which must ship once.
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
# dev-feed-git for the container: no cgo, so a cross-compile from here
mkdir -p "$TMP/bin"
(cd developer-feed/git && CGO_ENABLED=0 GOOS=linux GOARCH="$(docker version -f '{{.Server.Arch}}')" go build -o "$TMP/bin/dev-feed-git" .)
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
    mkdir -p /opt/observability-host "$HOME/.claude/projects/$PROJ/memory"
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
    # launchd and systemd do
    while :; do /opt/observability-host/bin/observability-host-launch || true; sleep 1; done' >/dev/null
in_agent() { docker exec "$AGENT" bash -c "$1"; }
for _ in $(seq 30); do docker logs "$AGENT" 2>&1 | grep -q 'collectors: claude claude-memory cmux git heartbeat$' && break; sleep 1; done
check "the feed's directory's defaults, claude, claude-memory, cmux and git, run beside the heartbeat" sh -c "docker logs $AGENT 2>&1 | grep -q 'collectors: claude claude-memory cmux git heartbeat\$'"
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

phase "after the first start: a commit pushed and fetched"
in_agent 'git -C /root/repo commit -q --allow-empty -F - <<MSG
feature-after-start-$RUN: what was asked for

The requirement, in the words of the session.

Co-Authored-By: Claude <noreply@anthropic.com>
MSG
git -C /root/repo push -q origin main'

phase "after the first start: lines appended, a new session, a subagent, a long line"
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
for _ in $(seq 20); do [ "$(docker logs "$AGENT" 2>&1 | grep -c 'collectors: claude claude-memory cmux git heartbeat$')" -ge 2 ] && break; sleep 1; done
check "the launcher started fluent-bit again" sh -c "[ \$(docker logs $AGENT 2>&1 | grep -c 'collectors: claude claude-memory cmux git heartbeat\$') -ge 2 ]"
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
X="FROM feed WHERE \"table\" = 'cmux'"
assert "cmux: the journal's rows from after the first run, once each, in order, with their workspace and pane" \
  "SELECT count(*) = 2 AND list(event_id ORDER BY sequence) = ['after-1-$RUN', 'after-2-$RUN'] AND list(surface_id ORDER BY sequence) = ['sf-1', 'sf-2'] AND bool_and(session_id = '$S1' AND workspace_id = 'ws-1') $X"
assert "cmux: nothing from before the start: live only" "SELECT count(*) = 0 $X AND event_id = 'before-$RUN'"
assert "cmux: the times and the subagent flag as the journal has them" \
  "SELECT bool_and(occurred_at >= TIMESTAMP '2026-10-20') AND count(*) FILTER (WHERE is_subagent) = 1 $X"
assert "cmux: no detail column: metadata only" \
  "SELECT count(*) = 0 FROM read_json_objects('s3://$BUCKET/$KEY_ROOT/$CLUSTER/*/*/*/*.jsonl.zst', format='newline_delimited', compression='zstd') WHERE json->>'table' = 'cmux' AND (json->>'detail' IS NOT NULL OR json::VARCHAR LIKE '%must not ship%')"
q "SELECT \"table\", list(DISTINCT kind) AS kinds, count(*) AS rows FROM feed WHERE \"table\" IN ('claude', 'git', 'cmux') GROUP BY ALL ORDER BY 1" | sed 's/^/  (/; s/$/)/'

if [ "$FAILED" -gt 0 ]; then echo "--- agent"; docker logs --tail 30 "$AGENT" 2>&1 | grep -v 'unattended-upgrades' || true; fi
echo
echo "developer-feed itest: $FAILED failed"
exit "$FAILED"
