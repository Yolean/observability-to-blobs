#!/usr/bin/env bash
# The host agent's collectors end to end, in docker (HOST_MODE.md,
# Collectors): the release tarball's launcher and fluent-bit in Ubuntu
# containers, one per scenario, writing to a versitygw container as a user
# that a bucket policy allows PutObject under the key root and nothing else,
# as the host phase's agent (itest/host/run.sh). No systemd and no qemu, so
# it runs where the host phase cannot: it covers what the collectors' flags
# put into the process, while the journal, the unit and the scenarios of a
# real host stay the host phase's.
#
#   TARBALL=target/host/observability-to-blobs-host-dev-linux-arm64.tar.gz itest/host-docker/run.sh
#
# Env:
#   TARBALL     the tarball for this machine's architecture (required;
#               images/fluentbit/host/build.sh target/host builds it)
#   LOCAL_PORT  this machine's port for the gateway, read by DuckDB (default 17171)
#   KEEP=1      leave the containers and the network for a look around
#
# Credentials are made up here and stay in this process and the containers.
# An assertion that fails does not stop the run; the exit status says how
# many did.
set -euo pipefail
cd "$(dirname "$0")/../.."

: "${TARBALL:?TARBALL: the host tarball, from images/fluentbit/host/build.sh}"
[ -f "$TARBALL" ] || { echo "no $TARBALL" >&2; exit 2; }
LOCAL_PORT=${LOCAL_PORT:-17171}
RUN=$(date +%s)
P=o2b-hostdocker
NET=$P-net GW=$P-gw
BUCKET=host-docker KEY_ROOT=host1 CLUSTER=itest
VGW_IMAGE=docker.io/versity/versitygw:v1.7.0
AWS_IMAGE=amazon/aws-cli:2.22.35
AGENT_IMAGE=$P-agent:ubuntu-24.04
AGENT_SECRET=$(openssl rand -hex 16)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/host-docker.XXXXXX")
FAILED=0

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
  rm -rf "$TMP"
  if [ "${KEEP:-}" = 1 ]; then echo "kept: docker ps --filter name=$P"; return; fi
  remove
}
# a previous run's containers, kept or left by an interrupted run
remove
trap cleanup EXIT

phase "gateway: versitygw, a bucket, and a user allowed PutObject under $KEY_ROOT/ only"
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
  create-user --access hostagent --secret "$AGENT_SECRET" --role user >/dev/null
aws_as() { # aws_as <access> <secret> <args...>: the aws cli on the network, against the gateway
  local a=$1 s=$2; shift 2
  docker run --rm --network "$NET" -e "AWS_ACCESS_KEY_ID=$a" -e "AWS_SECRET_ACCESS_KEY=$s" \
    -e AWS_DEFAULT_REGION=us-east-1 -e AWS_PAGER= "$AWS_IMAGE" --endpoint-url "http://$GW:7070" "$@"
}
POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["hostagent"]},"Action":["s3:PutObject"],"Resource":["arn:aws:s3:::'$BUCKET'/'$KEY_ROOT'/*"]}]}'
aws_as demoaccess demosecret s3api create-bucket --bucket "$BUCKET" >/dev/null
aws_as demoaccess demosecret s3api put-bucket-policy --bucket "$BUCKET" --policy "$POLICY"
echo probe >"$TMP/probe"
check "the agent's key may PutObject under $KEY_ROOT/" \
  docker run --rm --network "$NET" -v "$TMP/probe:/probe:ro" -e AWS_ACCESS_KEY_ID=hostagent -e "AWS_SECRET_ACCESS_KEY=$AGENT_SECRET" \
  -e AWS_DEFAULT_REGION=us-east-1 "$AWS_IMAGE" --endpoint-url "http://$GW:7070" s3api put-object --bucket "$BUCKET" --key "$KEY_ROOT/probe/$RUN" --body /probe
if aws_as hostagent "$AGENT_SECRET" s3api list-objects-v2 --bucket "$BUCKET" >/dev/null 2>&1; then
  failed "the agent's key may not list the bucket"
else
  pass "the agent's key may not list the bucket"
fi

phase "agent image: Ubuntu 24.04 with the packages HOST_MODE.md names"
docker build -q -t "$AGENT_IMAGE" - >/dev/null <<'DOCKERFILE'
FROM ubuntu:24.04
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
    libyaml-0-2 libsystemd0 ca-certificates >/dev/null && rm -rf /var/lib/apt/lists/*
DOCKERFILE
printf '[default]\naws_access_key_id = hostagent\naws_secret_access_key = %s\n' "$AGENT_SECRET" >"$TMP/s3-credentials"

# agent <scenario> <args file content> [docker run arg ...]: one agent
# container, the tarball at the install prefix, NODE_NAME the scenario, so
# that each scenario's objects are under their own node in the key;
# HOST_EXTRA_FILES the fixture, unset with NO_EXTRA_FILES=1
agent() {
  local name=$1 args=$2 extra=(-e "HOST_EXTRA_FILES=/var/log/itest/*.log")
  shift 2
  [ "${NO_EXTRA_FILES:-}" != 1 ] || extra=()
  printf '%s\n' "$args" >"$TMP/args-$name"
  docker run -d --name "$P-$name" --network "$NET" "$@" "${extra[@]}" \
    -v "$PWD/$TARBALL:/tmp/host.tar.gz:ro" -v "$TMP/s3-credentials:/etc/observability-host/s3-credentials:ro" \
    -v "$TMP/args-$name:/etc/observability-host/args:ro" \
    -e CLUSTER_NAME=$CLUSTER -e "NODE_NAME=$name" -e "S3_BUCKET=$BUCKET" -e "S3_ENDPOINT=http://$GW:7070" -e S3_TLS=off \
    -e KEY_ROOT=$KEY_ROOT -e UPLOAD_TIMEOUT=5s \
    -e AWS_SHARED_CREDENTIALS_FILE=/etc/observability-host/s3-credentials \
    -e HEARTBEAT_COMMAND=/opt/observability-host/bin/observability-host-heartbeat \
    "$AGENT_IMAGE" sh -c '
      set -e
      mkdir -p /opt/observability-host /var/lib/observability-host /var/log/itest
      tar xzf /tmp/host.tar.gz -C /opt/observability-host
      echo "itest-before-start" >/var/log/itest/a.log
      exec /opt/observability-host/bin/observability-host-launch' >/dev/null
}
in_agent() { docker exec "$P-$1" bash -c "$2"; }
listening() { in_agent "$1" 'exec 3<>/dev/tcp/127.0.0.1/2023' 2>/dev/null; }
# fluent-bit's server keeps an HTTP/1.0 connection open; 1.1 with close ends it
metrics_of() { in_agent "$1" 'exec 3<>/dev/tcp/127.0.0.1/2023; printf "GET /metrics HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n" >&3; timeout 5 cat <&3'; }

phase "scenarios: one agent each"
agent files-only '--collector.disable-defaults
--collector.files'
agent self-only '# the agent'"'"'s own metrics and nothing else
--collector.disable-defaults --collector.self'
agent defaults '# no flags: the defaults'
agent refused '--no-collector.heartbeat'
# the installer requires files and journal (HOST_MODE.md, Collectors), and
# the args file turns files off
printf '# the security log\nfiles journal\n' >"$TMP/required"
agent required-off '--no-collector.files' -v "$TMP/required:/etc/observability-host/required:ro"
# files with HOST_EXTRA_FILES unset: its path ends in an empty item
NO_EXTRA_FILES=1 agent no-extra '--collector.disable-defaults
--collector.files'
sleep 8
for s in files-only self-only defaults; do
  docker exec "$P-$s" sh -c "echo itest-after-start-$RUN >>/var/log/itest/a.log" || true
done

phase "launch: what each set of flags runs"
for s in files-only self-only defaults no-extra; do
  check "$s: the agent is running" sh -c "[ \"\$(docker inspect -f '{{.State.Running}}' $P-$s)\" = true ]"
done
check "files-only: the launcher logged heartbeat and files" sh -c "docker logs $P-files-only 2>&1 | grep -q 'collectors: files heartbeat\$'"
check "self-only: the launcher logged heartbeat and self" sh -c "docker logs $P-self-only 2>&1 | grep -q 'collectors: heartbeat self\$'"
check "defaults: the launcher logged the five defaults" sh -c "docker logs $P-defaults 2>&1 | grep -q 'collectors: files heartbeat journal node self\$'"
for _ in $(seq 20); do [ "$(docker inspect -f '{{.State.Running}}' "$P-refused")" = false ] && break; sleep 1; done
check "refused: --no-collector.heartbeat stops the launcher before fluent-bit, with exit 2" \
  sh -c "[ \"\$(docker inspect -f '{{.State.ExitCode}}' $P-refused)\" = 2 ] && docker logs $P-refused 2>&1 | grep -q 'cannot be turned off'"
for _ in $(seq 20); do [ "$(docker inspect -f '{{.State.Running}}' "$P-required-off")" = false ] && break; sleep 1; done
check "required-off: --no-collector.files, which /etc/observability-host/required requires, stops the launcher with exit 2" \
  sh -c "[ \"\$(docker inspect -f '{{.State.ExitCode}}' $P-required-off)\" = 2 ] && docker logs $P-required-off 2>&1 | grep -q 'the files collector is required on this host (/etc/observability-host/required)'"
check "no-extra: the launcher logged heartbeat and files" sh -c "docker logs $P-no-extra 2>&1 | grep -q 'collectors: files heartbeat\$'"

phase "waiting 70 s: the first heartbeat comes 60 s after the start, the first scrape 15 s"
sleep 70

phase "metrics port: open only for node and self"
if listening files-only; then failed "files-only: nothing listens on 2023"; else pass "files-only: nothing listens on 2023"; fi
m=$(metrics_of self-only 2>/dev/null || true)
check "self-only: 2023 serves the agent's own metrics" grep -q '^fluentbit_' <<<"$m"
if grep -q '^node_cpu' <<<"$m"; then failed "self-only: no node_* series"; else pass "self-only: no node_* series"; fi
m=$(metrics_of defaults 2>/dev/null || true)
# two here-strings: two greps on one stdin share it, and the first reads past what the second needs
# shellcheck disable=SC2016 # $1 is the inner bash's
check "defaults: 2023 serves node_* and the agent's own metrics" bash -c 'grep -q "^node_cpu" <<<"$1" && grep -q "^fluentbit_" <<<"$1"' _ "$m"

phase "bucket: the rows of each scenario"
cat >"$TMP/init.sql" <<SQL
.output /dev/null
INSTALL httpfs; LOAD httpfs; SET TimeZone='UTC';
-- a secret scoped to the bucket, which wins over any broader one this
-- machine keeps (a persistent secret for s3:// would otherwise be used)
CREATE OR REPLACE SECRET host_docker (TYPE s3, KEY_ID 'demoaccess', SECRET 'demosecret',
  REGION 'us-east-1', ENDPOINT '127.0.0.1:$LOCAL_PORT', USE_SSL false, URL_STYLE 'path',
  SCOPE 's3://$BUCKET');
SET VARIABLE raw_files = 's3://$BUCKET/$KEY_ROOT/$CLUSTER/*/*/*/*.jsonl.zst';
.read $PWD/compactor/sql/raw.sql
.output
SQL
q() { duckdb -noheader -csv -c ".read $TMP/init.sql" -c "$1" 2>&1; }
assert() { # assert <description> <SQL that yields true>
  local got
  got=$(q "$2") || true
  if [ "$got" = true ]; then pass "$1"; else failed "$1 (got: $got)"; fi
}
assert "files-only: the line written before the start and the one after, as file rows of the fixture" \
  "SELECT count(DISTINCT message) = 2 FROM raw WHERE node = 'files-only' AND \"table\" = 'logs' AND stream = 'file' AND unit = '/var/log/itest/a.log' AND message IN ('itest-before-start', 'itest-after-start-$RUN')"
assert "files-only: heartbeat rows" \
  "SELECT count(*) >= 1 FROM raw WHERE node = 'files-only' AND \"table\" = 'metrics' AND samples[1].name = 'observability_host_heartbeat'"
assert "self-only: heartbeat rows, and no file rows" \
  "SELECT count(*) FILTER (WHERE \"table\" = 'metrics') >= 1 AND count(*) FILTER (WHERE stream = 'file') = 0 FROM raw WHERE node = 'self-only'"
assert "defaults: heartbeat rows and the fixture's file rows" \
  "SELECT count(*) FILTER (WHERE \"table\" = 'metrics') >= 1 AND count(*) FILTER (WHERE unit = '/var/log/itest/a.log') >= 2 FROM raw WHERE node = 'defaults'"
assert "no scenario but the defaults has journal rows" \
  "SELECT count(*) = 0 FROM raw WHERE stream = 'journal' AND node <> 'defaults'"
assert "the refused scenarios wrote nothing" \
  "SELECT count(*) = 0 FROM raw WHERE node IN ('refused', 'required-off')"
assert "no-extra: with HOST_EXTRA_FILES unset, the package log's lines as file rows" \
  "SELECT count(*) > 0 FROM raw WHERE node = 'no-extra' AND stream = 'file' AND unit = '/var/log/dpkg.log'"
# the container has no /var/log/unattended-upgrades/: tail warns of that
# glob at every scan, with HOST_EXTRA_FILES set or not
check "no-extra: fluent-bit warned of no other path than files' own, and of nothing else but store_dir_limit_size" \
  sh -c "! docker logs $P-no-extra 2>&1 | grep -E '\\[ *(warn|error)\\]' | grep -v -e store_dir_limit_size -e ': /var/log/unattended-upgrades/\\*\\.log\$'"
for s in files-only:files,heartbeat self-only:heartbeat,self defaults:files,heartbeat,journal,node,self no-extra:files,heartbeat; do
  assert "${s%%:*}: the heartbeat says the collectors running, ${s#*:}" \
    "SELECT count(*) >= 1 AND bool_and(samples[1].labels['collectors'] = '${s#*:}') FROM raw WHERE node = '${s%%:*}' AND \"table\" = 'metrics' AND samples[1].name = 'observability_host_heartbeat'"
done
assert "every key is under $KEY_ROOT/$CLUSTER/{date}/{HH}/{node}/" \
  "SELECT bool_and(regexp_matches(filename, '/$KEY_ROOT/$CLUSTER/[0-9]{4}-[0-9]{2}-[0-9]{2}/[0-9]{2}/(files-only|self-only|defaults|no-extra)/[0-9]{4}-[A-Za-z0-9]{8}\\.jsonl\\.zst\$')) FROM raw"

if [ "$FAILED" -gt 0 ]; then
  for s in files-only self-only defaults refused required-off no-extra; do echo "--- $s"; docker logs --tail 15 "$P-$s" 2>&1 || true; done
fi
echo
echo "host-docker: $FAILED failed"
exit "$FAILED"
