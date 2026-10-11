#!/usr/bin/env bash
# The host phase of the itest (HOST_MODE.md, "What the itest verifies"): the
# host agent installed from the release tarball as a systemd service on the
# itest node (the qemu VM that also runs k3s), writing to its own versitygw
# bucket through a recording proxy. test.sh runs it after the node agent's
# assertions; it also runs alone against a running cluster:
#
#   TARBALL=target/host/observability-to-blobs-host-dev-linux-amd64.tar.gz \
#   CONTEXT=node-agent VM_SSH_PORT=2232 itest/host/run.sh
#
# Env:
#   TARBALL        the tarball, with its .sha256 next to it (required)
#   CONTEXT        the kubectl context of the itest cluster (default node-agent)
#   KUBECONFIG     as for kubectl
#   VM_SSH_PORT    the node's sshd on 127.0.0.1 (default 2232, itest/cluster/qemu)
#   VM_SSH_KEY     its key (default ~/.cache/y-cluster-qemu/node-agent-ssh)
#   VM_SSH_USER    ystack, passwordless sudo (y-cluster's guest user)
#   CLUSTER        the cluster name in object keys (default itest)
#   LOCAL_PORT     where the gateway is forwarded for DuckDB (default 17071)
#   GAP_SECONDS    how long the service is stopped in the gap scenario (default 130)
#
# What runs where: the agent, the recorder (itest/host/s3-recorder.py, which
# forwards to the gateway's NodePort and keeps one JSON line per request) and
# auditd are on the node; the gateway is in the cluster; the assertions read
# the bucket with DuckDB over a port-forward, as test.sh does. Credentials
# are made up here and stay in this process, the node's root-only credential
# file and the gateway's IAM directory. An assertion that fails does not stop
# the phase; the exit status says how many did.
# the jq and awk programs and the remote scripts are single-quoted on purpose
# shellcheck disable=SC2016
set -euo pipefail
cd "$(dirname "$0")/../.."

CONTEXT=${CONTEXT:-node-agent}
CLUSTER=${CLUSTER:-itest}
VM_SSH_PORT=${VM_SSH_PORT:-2232}
VM_SSH_KEY=${VM_SSH_KEY:-$HOME/.cache/y-cluster-qemu/node-agent-ssh}
VM_SSH_USER=${VM_SSH_USER:-ystack}
LOCAL_PORT=${LOCAL_PORT:-17071}
GAP_SECONDS=${GAP_SECONDS:-130}
TARBALL=${TARBALL:?TARBALL: the host tarball to install}
BUCKET=host-agent
KEY_ROOT=host1
RUN=$(date +%s)
KUBECTL="kubectl --context=$CONTEXT"

phase() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing tool: $1 ($2)"; }
need ssh "openssh-client"
need jq "https://jqlang.github.io/jq/"
need duckdb "https://duckdb.org/docs/installation/"
need kubectl "https://kubernetes.io/docs/tasks/tools/"
need openssl "openssl"
[ -f "$TARBALL" ] || fail "no tarball at $TARBALL (images/fluentbit/host/build.sh)"
[ -f "$TARBALL.sha256" ] || fail "no $TARBALL.sha256"
[ -f "$VM_SSH_KEY" ] || fail "no ssh key at $VM_SSH_KEY (VM_SSH_KEY)"

vm() {
  ssh -p "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o BatchMode=yes -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10 \
    "$VM_SSH_USER@127.0.0.1" "$@"
}
vmroot() { vm sudo "$@"; }
# the node's clock for every time in this phase: epoch seconds with fractions
vmnow() { vm date +%s.%N; }

TMP=$(mktemp -d "${TMPDIR:-/tmp}/host-itest.XXXXXX")
PF_PID=""
cleanup() {
  [ -z "$PF_PID" ] || kill "$PF_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

# ------------------------------------------------------------- gateway --

phase "host: the gateway, the bucket and the agent's key"
# an agent left by an earlier run holds the old key and would be refused
# (a denied request in the gateway's log) once the key is replaced below
vm "sudo systemctl stop observability-host 2>/dev/null || true"
$KUBECTL apply -k itest/host >/dev/null
$KUBECTL rollout status statefulset/versitygw-host --timeout=180s
vgw_admin() {
  $KUBECTL exec versitygw-host-0 -- versitygw admin --access demoaccess --secret demosecret \
    --endpoint-url http://127.0.0.1:7080 "$@"
}
# the gateway's access log outlives a run: only what is written from here on is this run's
LOG_SKIP=$($KUBECTL exec versitygw-host-0 -- sh -c 'wc -l < /logs/access.log 2>/dev/null || echo 0' | tr -d '[:space:]')
# two users with one policy: the agent's, whose requests the gateway's log
# must show only as PutObject, and one that the checks below may abuse
AGENT_SECRET=$(openssl rand -hex 16)
PROBE_SECRET=$(openssl rand -hex 16)
for u in hostagent hostprobe; do
  vgw_admin delete-user --access "$u" >/dev/null 2>&1 || true
done
vgw_admin create-user --access hostagent --secret "$AGENT_SECRET" --role user >/dev/null
vgw_admin create-user --access hostprobe --secret "$PROBE_SECRET" --role user >/dev/null

# the root account: bucket, policy and a clean prefix
POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["hostagent","hostprobe"]},"Action":["s3:PutObject"],"Resource":["arn:aws:s3:::'$BUCKET'/'$KEY_ROOT'/*"]}]}'
aws_pod() { # aws_pod <access> <secret> <shell script>: a one-off aws-cli pod; its log on stdout, its failure as ours
  local name=aws-host-itest phase deadline=$((SECONDS + 180))
  $KUBECTL delete pod "$name" --ignore-not-found --grace-period=1 >/dev/null 2>&1
  $KUBECTL run "$name" --restart=Never --image=amazon/aws-cli:2.22.35 \
    --env="AWS_ACCESS_KEY_ID=$1" --env="AWS_SECRET_ACCESS_KEY=$2" --env=AWS_DEFAULT_REGION=us-east-1 \
    --env=AWS_PAGER= --env="POLICY=$POLICY" --env="BUCKET=$BUCKET" --env="ROOT=$KEY_ROOT" --env="CLUSTER=$CLUSTER" \
    --command -- sh -c "$3" >/dev/null
  while :; do
    phase=$($KUBECTL get pod "$name" -o jsonpath='{.status.phase}')
    case "$phase" in (Succeeded|Failed) break;; esac
    [ $SECONDS -lt $deadline ] || fail "$name still $phase after 180 s"
    sleep 2
  done
  $KUBECTL logs "$name"
  $KUBECTL delete pod "$name" --grace-period=1 >/dev/null
  [ "$phase" = Succeeded ] || fail "$name $phase"
}
GW=http://versitygw-host:7070
aws_pod demoaccess demosecret "
  set -e
  aws --endpoint-url $GW s3api head-bucket --bucket \$BUCKET 2>/dev/null || aws --endpoint-url $GW s3api create-bucket --bucket \$BUCKET >/dev/null
  aws --endpoint-url $GW s3api put-bucket-policy --bucket \$BUCKET --policy \"\$POLICY\"
  aws --endpoint-url $GW s3 rm --recursive --quiet s3://\$BUCKET/\$ROOT/\$CLUSTER
" >/dev/null

# the key is what the phase claims it is: PutObject under the root, and
# nothing else, not even under the root
probe=$(aws_pod hostprobe "$PROBE_SECRET" "
  echo probe > /tmp/probe
  aws --endpoint-url $GW s3api put-object --bucket \$BUCKET --key \$ROOT/probe --body /tmp/probe >/dev/null 2>&1 && echo put-root=allowed || echo put-root=denied
  aws --endpoint-url $GW s3api put-object --bucket \$BUCKET --key other/probe --body /tmp/probe >/dev/null 2>&1 && echo put-elsewhere=allowed || echo put-elsewhere=denied
  aws --endpoint-url $GW s3api get-object --bucket \$BUCKET --key \$ROOT/probe /tmp/out >/dev/null 2>&1 && echo get=allowed || echo get=denied
  aws --endpoint-url $GW s3api head-object --bucket \$BUCKET --key \$ROOT/probe >/dev/null 2>&1 && echo head=allowed || echo head=denied
  aws --endpoint-url $GW s3api list-objects-v2 --bucket \$BUCKET --prefix \$ROOT/ >/dev/null 2>&1 && echo list=allowed || echo list=denied
  aws --endpoint-url $GW s3api delete-object --bucket \$BUCKET --key \$ROOT/probe >/dev/null 2>&1 && echo delete=allowed || echo delete=denied
")
probe=$(echo "$probe" | tr -d '\r' | grep -E '^[a-z-]+=(allowed|denied)$')
echo "$probe" | paste -sd' ' | sed 's/^/  the policy, as the probe user sees it: /'
[ "$(echo "$probe" | paste -sd' ')" = "put-root=allowed put-elsewhere=denied get=denied head=denied list=denied delete=denied" ] \
  || fail "the agent's key is not put-only (above)"

# DuckDB reads through a port-forward as root
pf_start() {
  # the first forward needs the port free: one that already answers is
  # another process's, perhaps another itest's gateway
  if [ -z "$PF_PID" ] && (exec 3<>"/dev/tcp/127.0.0.1/$LOCAL_PORT") 2>/dev/null; then
    fail "127.0.0.1:$LOCAL_PORT is in use (another itest?); set LOCAL_PORT to a free port"
  fi
  [ -z "$PF_PID" ] || kill "$PF_PID" 2>/dev/null || true
  $KUBECTL port-forward svc/versitygw-host "$LOCAL_PORT:7070" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 30); do
    curl -s -o /dev/null "http://127.0.0.1:$LOCAL_PORT/" && return 0
    sleep 1
  done
  fail "could not forward the gateway to 127.0.0.1:$LOCAL_PORT"
}
pf_ensure() { { [ -n "$PF_PID" ] && kill -0 "$PF_PID" 2>/dev/null && curl -s -o /dev/null "http://127.0.0.1:$LOCAL_PORT/"; } || pf_start; }
pf_start

INIT=$TMP/init.sql
cat >"$INIT" <<SQL
.output /dev/null
INSTALL httpfs; LOAD httpfs; SET TimeZone='UTC';
SET s3_region='us-east-1'; SET s3_endpoint='127.0.0.1:$LOCAL_PORT';
SET s3_access_key_id='demoaccess'; SET s3_secret_access_key='demosecret';
SET s3_use_ssl=false; SET s3_url_style='path';
SET VARIABLE raw_files = 's3://$BUCKET/$KEY_ROOT/$CLUSTER/*/*/*/*.jsonl.zst';
.read $PWD/compactor/sql/raw.sql
.read $PWD/compactor/sql/tables.sql
.output
SQL
RAW_OBJECTS="read_json_objects('s3://$BUCKET/$KEY_ROOT/$CLUSTER/*/*/*/*.jsonl.zst', format='newline_delimited', compression='zstd')"
# q runs in command substitutions (a subshell), so the forward is renewed
# by the callers, in this shell, where PF_PID is remembered and stopped at exit
q() { duckdb -noheader -csv -c ".read $INIT" -c "$1" 2>&1; }
ERRORS=0
assert() { # assert <description> <sql returning a single truthy value>
  local out
  pf_ensure
  out=$(q "$2" | tr -d '[:space:]' || true)
  if [ "$out" = true ] || [ "$out" = 1 ]; then
    echo "  PASS: $1"
  else
    echo "  FAIL: $1 (got: $out)" >&2
    ERRORS=$((ERRORS + 1))
  fi
}
check() { # check <description> <command...>: passes when the command succeeds
  local d=$1; shift
  if "$@" >/dev/null 2>&1; then echo "  PASS: $d"; else echo "  FAIL: $d" >&2; ERRORS=$((ERRORS + 1)); fi
}
wait_for() { # wait_for <description> <seconds> <sql returning true when it is time>
  local deadline=$((SECONDS + $2)) out
  while :; do
    pf_ensure
    out=$(q "$3" | tr -d '[:space:]' || true)
    case "$out" in (true|1) return 0;; esac
    [ $SECONDS -lt $deadline ] || { echo "  FAIL: gave up waiting for $1 after $2 s (got: $out)" >&2; ERRORS=$((ERRORS + 1)); return 1; }
    sleep 10
  done
}
# the agent's own logs and the recorder's log, for a failure
agent_logs() { vm sudo journalctl -u observability-host --no-pager -n "${1:-60}" || true; }
rec_log() { vm sudo cat /var/lib/itest-recorder/log.jsonl; }

# ---------------------------------------------------------------- node --

phase "host: install the tarball on the node as a systemd service"
TB=$(basename "$TARBALL")
# the pipeline as the binary reads it, where the binary runs (it is built for
# the node's distribution, which is not always this machine's)
mkdir "$TMP/pkg"
tar xzf "$TARBALL" -C "$TMP/pkg"
if "$TMP/pkg/bin/fluent-bit" --version >/dev/null 2>&1; then
  check "the tarball's pipeline passes fluent-bit's dry run" \
    env CLUSTER_NAME=c NODE_NAME=n S3_BUCKET=b S3_ENDPOINT=http://127.0.0.1:1 "$TMP/pkg/bin/fluent-bit" --dry-run -c "$TMP/pkg/etc/host.yaml"
else
  echo "  (the binary does not run on this machine, no dry run here)"
fi

vm "rm -rf /tmp/host-itest && mkdir /tmp/host-itest"
scp -q -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o BatchMode=yes -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  "$TARBALL" "$TARBALL.sha256" itest/host/s3-recorder.py "$VM_SSH_USER@127.0.0.1:/tmp/host-itest/"
# the bucket key goes in as the root-only file the unit loads, from stdin
printf '[default]\naws_access_key_id = hostagent\naws_secret_access_key = %s\n' "$AGENT_SECRET" \
  | vm "sudo sh -c 'umask 077; mkdir -p /etc/observability-host; cat > /etc/observability-host/s3-credentials'"
vm "sudo bash -s" <<SETUP
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
cd /tmp/host-itest
sha256sum -c $TB.sha256
# what an installer does (HOST_MODE.md, Packaging)
mkdir -p /opt/observability-host
tar xzf $TB -C /opt/observability-host
/opt/observability-host/bin/fluent-bit --version
id -u observability-host >/dev/null 2>&1 || useradd --system --user-group --no-create-home \
  --home-dir /nonexistent --shell /usr/sbin/nologin observability-host
install -m 0644 /opt/observability-host/lib/systemd/system/observability-host.service /etc/systemd/system/
# auditd, with the audit log readable for the group the service is in
if ! command -v auditctl >/dev/null; then
  for i in 1 2 3; do apt-get update -qq && apt-get install -y -qq auditd >/dev/null 2>&1 && break; sleep 5; done
fi
sed -i 's/^log_group *=.*/log_group = adm/' /etc/audit/auditd.conf
systemctl kill --signal=SIGHUP auditd || true
# a journal that survives the reboot below
mkdir -p /var/log/journal
systemd-tmpfiles --create --prefix /var/log/journal
systemctl restart systemd-journald
# a further file to tail, as the configuration lists them
install -m 0640 -o root -g adm /dev/null /var/log/itest-host.log
# the environment file, not the credential (that is loaded from its own file)
cat > /etc/observability-host/env <<ENV
CLUSTER_NAME=$CLUSTER
S3_BUCKET=$BUCKET
S3_ENDPOINT=http://127.0.0.1:30072
S3_TLS=off
KEY_ROOT=$KEY_ROOT
UPLOAD_TIMEOUT=30s
HOST_EXTRA_FILES=/var/log/itest-host.log
# the node runs the node agent, which exports node_*
HOST_NODE_COLLECTORS=textfile
ENV
# the recorder between the agent and the gateway's NodePort
install -m 0755 s3-recorder.py /usr/local/bin/s3-recorder.py
mkdir -p /var/lib/itest-recorder/control
rm -f /var/lib/itest-recorder/log.jsonl /var/lib/itest-recorder/control/*
cat > /etc/systemd/system/itest-s3-recorder.service <<UNIT
[Unit]
Description=itest S3 recorder
After=network.target
[Service]
ExecStart=/usr/bin/python3 /usr/local/bin/s3-recorder.py 127.0.0.1:30072 127.0.0.1:30071 /var/lib/itest-recorder/log.jsonl /var/lib/itest-recorder/control
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
# a clean start: no position, no buffer from an earlier run
systemctl stop observability-host 2>/dev/null || true
rm -rf /var/lib/observability-host
systemctl daemon-reload
systemctl enable --now itest-s3-recorder observability-host
SETUP
# the audit log must be readable for the service
vm "sudo ls -ld /var/log/audit; sudo ls -l /var/log/audit/audit.log" | sed 's/^/  /'
sleep 5
check "the unit is active" vm systemctl is-active --quiet observability-host

check "the recorder is active" vm systemctl is-active --quiet itest-s3-recorder

# ------------------------------------------------------------ coverage --

phase "scenario: a login, a failed login, an account change, a privileged command, a configuration file change, a service stopped, a package installed"
COVER_START=$(vmnow)
# a failed login is an ssh attempt as a user that does not exist
ssh -p "$VM_SSH_PORT" -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o LogLevel=ERROR -o ConnectTimeout=10 "itest-fail-$RUN@127.0.0.1" true >/dev/null 2>&1 || true
vm "bash -s" <<EVENTS
set -euo pipefail
sudo useradd --no-create-home --no-user-group --shell /usr/sbin/nologin itest-acct-$RUN
sudo -n /bin/echo itest-sudo-$RUN >/dev/null
sudo touch /etc/itest-conf-$RUN
sudo auditctl -w /etc/itest-conf-$RUN -p wa -k itest_conf_$RUN
sudo sh -c 'echo changed > /etc/itest-conf-$RUN'
sudo systemd-run --quiet --unit itest-svc-$RUN --collect sleep 300
sleep 1
sudo systemctl stop itest-svc-$RUN
d=\$(mktemp -d)
mkdir -p "\$d/pkg/DEBIAN"
printf 'Package: itest-pkg-$RUN\nVersion: 1.0\nArchitecture: all\nMaintainer: itest <itest@example.invalid>\nDescription: marker package of the host phase\n' > "\$d/pkg/DEBIAN/control"
dpkg-deb --build "\$d/pkg" "\$d/itest-pkg-$RUN.deb" >/dev/null
sudo env DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l apt-get install -y -qq "\$d/itest-pkg-$RUN.deb" >/dev/null 2>&1
rm -rf "\$d"
EVENTS
echo "  events created, waiting for the next upload"

# --------------------------------------------------------------- survival --

phase "scenario: an authentication event whose local logs are deleted by root after the agent has read them"
SURV_T0=$(vmnow)
ssh -p "$VM_SSH_PORT" -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o LogLevel=ERROR -o ConnectTimeout=10 "itest-surv-$RUN@127.0.0.1" true >/dev/null 2>&1 || true
sleep 10
# root deletes what the node recorded: the audit log's content, the
# rotated journal files, the syslog file if there is one
vm "sudo bash -s" <<'DELETE'
truncate -s 0 /var/log/audit/audit.log
rm -f /var/log/auth.log /var/log/syslog
journalctl --rotate
journalctl --vacuum-size=1K >/dev/null 2>&1 || true
DELETE
# sshd records the name of a user that does not exist in the journal, and in
# the audit log only that a login failed (the name is hidden)
SURV_SQL="SELECT (SELECT count(*) FROM logs WHERE stream = 'journal' AND message LIKE '%itest-surv-$RUN%') >= 1 AND (SELECT count(*) FROM logs WHERE unit = '/var/log/audit/audit.log' AND message LIKE 'type=USER_LOGIN%res=failed%' AND time > to_timestamp($SURV_T0)::TIMESTAMP) >= 1"
wait_for "the failed login in the bucket" 180 "$SURV_SQL" || true
pf_ensure
SURV_KEY=$(q "SELECT any_value(regexp_replace(filename, '^s3://$BUCKET', '')) FROM logs WHERE message LIKE '%itest-surv-$RUN%' AND stream = 'journal'" | tr -d '[:space:]')
SURV_T1=$(rec_log | jq -rs --arg key "/$BUCKET$SURV_KEY" '[.[] | select(.path == $key and .status == 200) | .t] | first // empty')
if [ -n "$SURV_T1" ]; then
  SURV_SECS=$(awk -v a="$SURV_T0" -v b="$SURV_T1" 'BEGIN { printf "%.1f", b - a }')
  echo "  event to object: $SURV_SECS s (upload_timeout 30 s; the node's clock, the recorder's PUT time)"
  check "the object landed within 90 s of the event (30 s upload window, and the 10 s the logs were left alone)" \
    awk -v s="$SURV_SECS" 'BEGIN { exit !(s < 90) }'
else
  echo "  FAIL: no successful upload of $SURV_KEY in the recorder's log" >&2
  ERRORS=$((ERRORS + 1))
fi
assert "the event is in the bucket as a journal row and as an audit log row (a failed USER_LOGIN), after the local copies were deleted" "$SURV_SQL"

# ------------------------------------------------------------------- gap --

phase "scenario: the service stopped for $GAP_SECONDS s"
HEARTBEAT_COUNT="SELECT count(*) FROM metrics WHERE name = 'observability_host_heartbeat'"
wait_for "three heartbeats from this start" 300 "SELECT ($HEARTBEAT_COUNT) >= 3" || true
vm "sudo systemctl stop observability-host"
GAP_T0=$(vmnow)
STOP_MARK="itest-stopped-$RUN"
vm "logger -t itest-host -p user.notice $STOP_MARK"
sleep "$GAP_SECONDS"
vm "sudo systemctl start observability-host"
GAP_T1=$(vmnow)
echo "  stopped at $GAP_T0, started at $GAP_T1 (the node's clock)"
# the heartbeat resumes one interval after the start
wait_for "two heartbeats after the start" 300 "SELECT ($HEARTBEAT_COUNT) >= 5" || true

# ------------------------------------------------- restart and reboot --

phase "scenario: delivery stopped, events created, the service restarted, the node rebooted"
vm "sudo touch /var/lib/itest-recorder/control/fail-all"
BLOCK_T=$(vmnow)
mark() { # mark <label>: five journal lines and five file lines
  vm "bash -s" <<MARKS
for i in 1 2 3 4 5; do
  logger -t itest-host -p user.notice "itest-once-$RUN-$1-j\$i"
  echo "itest-once-$RUN-$1-f\$i" | sudo tee -a /var/log/itest-host.log >/dev/null
done
MARKS
}
mark A
sleep 10
vm "sudo systemctl restart observability-host"
sleep 5
mark B
sleep 10
BOOT1=$(vm cat /proc/sys/kernel/random/boot_id)
echo "  rebooting the node (boot $BOOT1)"
vm "sudo systemctl reboot" >/dev/null 2>&1 || true
deadline=$((SECONDS + 420))
while :; do
  sleep 10
  BOOT2=$(vm cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)
  [ -n "$BOOT2" ] && [ "$BOOT2" != "$BOOT1" ] && break
  [ $SECONDS -lt $deadline ] || fail "the node did not come back within 420 s"
done
echo "  node is back (boot $BOOT2)"
for _ in $(seq 60); do
  vm systemctl is-active --quiet observability-host && break
  sleep 3
done
check "the service is active after the reboot" vm systemctl is-active --quiet observability-host
mark C
sleep 5
# delivery is still stopped: the control file survived the reboot too
rec_log >"$TMP/rec-blocked.jsonl"
check "no object was delivered while delivery was stopped, across the restart and the reboot" \
  jq -es --argjson t "$BLOCK_T" '[.[] | select(.t > $t + 2 and .status == 200)] | length == 0' "$TMP/rec-blocked.jsonl"
# the gateway is back when the node's NodePort answers
for _ in $(seq 90); do
  vm "curl -s -o /dev/null http://127.0.0.1:30071/" && break
  sleep 5
done
vm "sudo rm -f /var/lib/itest-recorder/control/fail-all"
echo "  delivery restored"
wait_for "all thirty events" 300 \
  "SELECT count(DISTINCT message) = 30 FROM logs WHERE message LIKE 'itest-once-$RUN-%'" || true
assert "every event arrived, none twice (journal lines and file lines, before the restart, after it and after the reboot)" \
  "SELECT count(*) = 30 AND count(DISTINCT message) = 30 FROM logs WHERE message LIKE 'itest-once-$RUN-%'"
assert "journal events carry stream journal and file events stream file with the file's path as unit" \
  "SELECT bool_and(CASE WHEN message LIKE '%-j_' THEN stream = 'journal' ELSE stream = 'file' AND unit = '/var/log/itest-host.log' END) FROM logs WHERE message LIKE 'itest-once-$RUN-%'"

# ----------------------------------------------------------- fresh keys --

phase "scenario: uploads that fail and are retried"
vm "echo 3 | sudo tee /var/lib/itest-recorder/control/fail-next >/dev/null"
FRESH_MARK="itest-fresh-$RUN"
vm "logger -t itest-host -p user.notice $FRESH_MARK"
wait_for "the event to arrive after the failed attempts" 240 \
  "SELECT count(*) = 1 FROM logs WHERE message = '$FRESH_MARK'" || true

# ---------------------------------------------------------------- assert --

phase "assert: the bucket"
pf_ensure
q "SELECT filename, \"table\", count(*) AS lines FROM raw GROUP BY ALL ORDER BY filename, \"table\"" | tail -n 12 | sed 's/^/  /'
show_rec() { rec_log | jq -r '[.t, .method, .status, .md5_header, .md5_ok, .path] | @tsv' | tail -n 12 | sed 's/^/  /'; }
show_rec

echo "  -- coverage"
assert "a login: a journal row of an accepted publickey login" \
  "SELECT count(*) >= 1 FROM logs WHERE stream = 'journal' AND message LIKE 'Accepted publickey for $VM_SSH_USER%'"
assert "a failed login: the user that does not exist in the journal, and a failed USER_LOGIN in the audit log" \
  "SELECT (SELECT count(*) FROM logs WHERE stream = 'journal' AND message LIKE '%itest-fail-$RUN%') >= 1 AND (SELECT count(*) FROM logs WHERE unit = '/var/log/audit/audit.log' AND message LIKE 'type=USER_LOGIN%res=failed%' AND time > to_timestamp($COVER_START)::TIMESTAMP) >= 1"
assert "an account change: useradd's row in the journal, and an ADD_USER in the audit log" \
  "SELECT (SELECT count(*) FROM logs WHERE stream = 'journal' AND message LIKE 'new user: name=itest-acct-$RUN,%') >= 1 AND (SELECT count(*) FROM logs WHERE unit = '/var/log/audit/audit.log' AND message LIKE 'type=ADD_USER%' AND time > to_timestamp($COVER_START)::TIMESTAMP) >= 1"
assert "a privileged command: sudo's journal row with the command, and the audit log's USER_CMD" \
  "SELECT (SELECT count(*) FROM logs WHERE stream = 'journal' AND message LIKE '%COMMAND=%itest-sudo-$RUN%') >= 1 AND (SELECT count(*) FROM logs WHERE unit = '/var/log/audit/audit.log' AND message LIKE 'type=USER_CMD%' AND time > to_timestamp($COVER_START)::TIMESTAMP) >= 1"
assert "a configuration file change: the audit log's records of the watch on the file" \
  "SELECT count(*) >= 1 FROM logs WHERE unit = '/var/log/audit/audit.log' AND message LIKE '%itest_conf_$RUN%'"
assert "a service stopped: the journal's Stopped line of the unit" \
  "SELECT count(*) >= 1 FROM logs WHERE stream = 'journal' AND message LIKE 'Stopped itest-svc-$RUN.service%'"
assert "a package installed: dpkg's log and apt's history" \
  "SELECT count(*) FILTER (unit = '/var/log/dpkg.log' AND message LIKE '% install itest-pkg-$RUN:%') >= 1 AND count(*) FILTER (unit = '/var/log/apt/history.log' AND message LIKE 'Install: itest-pkg-$RUN:%') >= 1 FROM logs"
assert "the service's own stop is a record in the bucket, shipped before or after the restart" \
  "SELECT count(*) >= 1 FROM logs WHERE stream = 'journal' AND message LIKE 'Stopped observability-host.service%' AND time > to_timestamp($GAP_T0 - 5)::TIMESTAMP"

echo "  -- gap"
assert "a line logged while the service was stopped arrives, once, after the start" \
  "SELECT count(*) = 1 FROM logs WHERE message = '$STOP_MARK'"
HEARTBEATS="SELECT time AS ts, labels['boot_id'] AS boot, CAST(value AS BIGINT) AS n FROM metrics WHERE name = 'observability_host_heartbeat'"
STEPS="SELECT boot, n, ts, lag(n) OVER w AS pn, lag(ts) OVER w AS pts FROM ($HEARTBEATS) WINDOW w AS (PARTITION BY boot ORDER BY ts)"
assert "heartbeats arrive about every 60 s" \
  "SELECT count(*) >= 5 AND count(*) FILTER (date_diff('millisecond', pts, ts) BETWEEN 50000 AND 70000) >= 3 FROM ($STEPS)"
assert "the service's restart shows as a gap in the time of the heartbeats while the counter continues" \
  "SELECT count(*) >= 1 FROM ($STEPS) WHERE n = pn + 1 AND date_diff('second', pts, ts) >= $GAP_SECONDS"
assert "within a boot the counter goes up by one, never repeats and never skips" \
  "SELECT bool_and(n = pn + 1) AND count(*) = count(DISTINCT (boot, n)) FROM ($STEPS) WHERE pn IS NOT NULL"
assert "the node's reboot starts a new boot id, and the counter starts at 1 again" \
  "SELECT count(DISTINCT boot) >= 2 AND bool_and(first_n = 1) FROM (SELECT boot, min(n) AS first_n FROM ($HEARTBEATS) GROUP BY boot)"
assert "the heartbeat row is job node, with the node's name and port in instance" \
  "SELECT count(*) >= 1 AND bool_and(job = 'node' AND instance LIKE '%:2023' AND cluster = '$CLUSTER') FROM metrics WHERE name = 'observability_host_heartbeat'"
echo "  restart flag as the node serves it:"
vm "curl -s http://127.0.0.1:2023/metrics | grep -E '^(node_reboot_required|node_(cpu|load|memory)|fluentbit_output_proc_records_total)' | cut -c1-120 | head -8" | sed 's/^/    /' || true
check "the node serves the restart flag and the agent's own metrics, and no node_* of the machine (HOST_NODE_COLLECTORS=textfile)" \
  vm "curl -s http://127.0.0.1:2023/metrics | grep -q '^node_reboot_required ' && curl -s http://127.0.0.1:2023/metrics | grep -q '^fluentbit_' && ! curl -s http://127.0.0.1:2023/metrics | grep -q '^node_cpu'"

echo "  -- put only"
ACCESS_LOG=$TMP/access.log
$KUBECTL exec versitygw-host-0 -- cat /logs/access.log | tail -n "+$((LOG_SKIP + 1))" >"$ACCESS_LOG"
# fields of the gateway's log line: owner bucket [time zone] remote requester id operation key uri status
awk '$6 == "hostagent" { print $8 " " $11 }' "$ACCESS_LOG" | sort | uniq -c | sed 's/^/  gateway log, requests of the agent: /'
check "the gateway's log has requests of the agent, and every one is a PutObject answered 200: no list, read, head, delete, multipart or denied request" \
  test "$(awk '$6 == "hostagent" { print $8 " " $11 }' "$ACCESS_LOG" | sort -u)" = "s3_PutObject 200"
check "the agent put under its root only ($KEY_ROOT/)" \
  awk -v root="$KEY_ROOT/" '$6 == "hostagent" && index($9, root) != 1 { bad = 1 } END { exit bad }' "$ACCESS_LOG"
REC=$TMP/rec.jsonl
rec_log >"$REC"
check "the recorder saw PUT and nothing else" \
  jq -es 'length > 0 and all(.[]; .method == "PUT")' "$REC"

echo "  -- content-md5"
check "every upload carries a Content-MD5 header that matches its body (refused attempts included)" \
  jq -es 'length > 0 and all(.[]; .md5_header == true and .md5_ok == true)' "$REC"

echo "  -- fresh keys"
check "uploads were refused and retried (the recorder refused attempts)" \
  jq -es '[.[] | select(.injected == true)] | length >= 3' "$REC"
check "no key was written twice: every attempt, refused or not, was under a key of its own" \
  jq -es '[.[].path] | length == (unique | length)' "$REC"
check "a refused upload was retried under a new key (same hour and MMSS, another suffix)" \
  jq -es 'map(. + {prefix: (.path | sub("-[^-/]+\\.jsonl\\.zst$"; ""))}) | group_by(.prefix) | map(select(any(.[]; .injected == true) and any(.[]; .status == 200) and ((map(.path) | unique | length) > 1))) | length >= 1' "$REC"
check "and the gateway's log has no key twice among the agent's accepted uploads" \
  test -z "$(awk '$6 == "hostagent" && $11 == 200 { print $9 }' "$ACCESS_LOG" | sort | uniq -d)"

echo "  -- schema"
assert "keys are /$KEY_ROOT/cluster/date/HH/node/MMSS-suffix.jsonl.zst" \
  "SELECT bool_and(regexp_matches(filename, '^s3://$BUCKET/$KEY_ROOT/$CLUSTER/\d{4}-\d{2}-\d{2}/\d{2}/[^/]+/\d{4}-[0-9A-Za-z]+\.jsonl\.zst\$')) FROM raw"
assert "every field a host line carries is a pinned column of compactor/sql/raw.sql (no extra field)" \
  "WITH seen AS (SELECT list_distinct(flatten(list(json_keys(json)))) AS keys FROM $RAW_OBJECTS),
        pinned AS (SELECT list(column_name) AS cols FROM (DESCRIBE raw))
   SELECT list_filter(seen.keys, lambda k: NOT list_contains(pinned.cols, k)) = [] FROM seen, pinned"
assert "journal lines carry exactly table, cluster, node, stream, unit, priority, message and ts" \
  "SELECT list_sort(list_distinct(flatten(list(json_keys(json))))) = ['cluster', 'message', 'node', 'priority', 'stream', 'table', 'ts', 'unit']
   FROM $RAW_OBJECTS WHERE json_extract_string(json, 'stream') = 'journal'"
assert "file lines carry exactly table, cluster, node, stream, unit, message and ts" \
  "SELECT list_sort(list_distinct(flatten(list(json_keys(json))))) = ['cluster', 'message', 'node', 'stream', 'table', 'ts', 'unit']
   FROM $RAW_OBJECTS WHERE json_extract_string(json, 'stream') = 'file'"
assert "heartbeat lines carry exactly table, cluster, node, job, instance, samples, time and ts" \
  "SELECT list_sort(list_distinct(flatten(list(json_keys(json))))) = ['cluster', 'instance', 'job', 'node', 'samples', 'table', 'time', 'ts']
   FROM $RAW_OBJECTS WHERE json_extract_string(json, 'table') = 'metrics'"
assert "a sample carries exactly name, type, labels and value" \
  "SELECT list_sort(list_distinct(flatten(list(json_keys(json_extract(json, '\$.samples[0]')))))) = ['labels', 'name', 'type', 'value']
   FROM $RAW_OBJECTS WHERE json_extract_string(json, 'table') = 'metrics'"
assert "table is logs or metrics, never podindex" \
  "SELECT bool_and(\"table\" IN ('logs', 'metrics')) FROM raw"
assert "stream is journal or file, and a file row's unit is the path of a file the agent reads" \
  "SELECT bool_and(stream IN ('journal', 'file')) AND bool_and(stream <> 'file' OR unit IN ('/var/log/audit/audit.log', '/var/log/dpkg.log', '/var/log/apt/history.log', '/var/log/itest-host.log') OR unit LIKE '/var/log/unattended-upgrades/%.log') FROM logs"
assert "ts is the event time at microseconds, within a microsecond of the nanosecond time where a line has one" \
  "SELECT bool_and(ts IS NOT NULL) AND bool_and(time IS NULL OR abs(epoch_ns(time) - epoch_ns(ts)) <= 1000) FROM raw"
assert "ts, cluster and node are on every line" \
  "SELECT bool_and(ts IS NOT NULL AND cluster = '$CLUSTER' AND node IS NOT NULL AND node <> '') FROM raw"
assert "priority is an integer on journal rows only, and there are no namespace, pod or container columns on any row" \
  "SELECT bool_and((stream = 'journal') = (priority IS NOT NULL)) AND bool_and(namespace IS NULL AND pod IS NULL AND container IS NULL) FROM raw WHERE \"table\" = 'logs'"

echo
if [ "$ERRORS" -gt 0 ]; then
  agent_logs 60 >&2
  echo "recorder:" >&2
  show_rec >&2 || true
  fail "$ERRORS host assertion(s) failed"
fi
echo "PASS: all host assertions passed"
