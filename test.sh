#!/usr/bin/env bash
# The one command: checks, unit tests, image build, and the itest against a
# local k3s node. CI runs this same script; the only difference is how the
# tools below got installed (see .github/workflows).
#
#   ./test.sh            everything
#   ./test.sh checks     checks and unit tests only
#   ./test.sh itest      skip checks and unit tests
#   ./test.sh up         provision the cluster and deploy the agent, the
#                        compactor and the sample workloads, print how to
#                        look around; no checks, no assertions, nothing purged.
#
# Env:
#   SKIP_BUILD=1         reuse target/images/node-agent.tar and
#                        target/images/compactor-oci
#   IMAGE_REF=ref        load a published image instead of building
#   CLUSTER_CONFIG=dir   y-cluster config dir (default: itest/cluster/qemu with
#                        /dev/kvm, itest/cluster/docker otherwise)
#   CONTEXT, S3_PORT     must match CLUSTER_CONFIG when overriding it
#   IMAGE_BUILD_ARGS     extra docker buildx flags, e.g. a CI cache
#
# The cluster is left running; y-cluster teardown -c <config dir> removes it.
set -euo pipefail
cd "$(dirname "$0")"

CONTEXT=${CONTEXT:-node-agent}
IMAGE=node-agent:itest
BUCKET=node-agent
CLUSTER=itest
S3_PORT=${S3_PORT:-30070}
export KUBECONFIG=$PWD/itest/cluster/kubeconfig
KUBECTL="kubectl --context=$CONTEXT"
MODE=${1:-all}
case "$MODE" in (all|checks|itest|up) ;; (*) echo "usage: $0 [checks|itest|up]" >&2; exit 2;; esac

phase() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
need() {
  command -v "$1" >/dev/null 2>&1 || fail "missing tool: $1 ($2)"
}

need docker "https://docs.docker.com/engine/install/"
need kubectl "https://kubernetes.io/docs/tasks/tools/"

# ---------------------------------------------------------------- checks --

if [ "$MODE" = all ] || [ "$MODE" = checks ]; then
  need go "https://go.dev/dl/"
  need shellcheck "https://github.com/koalaman/shellcheck"
  phase "checks: shell"
  shellcheck -x -P SCRIPTDIR test.sh compactor/build.sh

  for mod in images/fluentbit/plugins/cri_scrape images/blobs-to-prometheus tools/rules-manifest; do
    phase "checks: go ($mod)"
    (
      cd "$mod"
      unformatted=$(gofmt -l .)
      [ -z "$unformatted" ] || fail "gofmt: $unformatted"
      go vet ./...
      go test ./...
    )
  done

  phase "checks: kustomize"
  kubectl kustomize agent >/dev/null
  kubectl kustomize itest/k8s >/dev/null
  kubectl kustomize itest/k8s-docker >/dev/null
  kubectl kustomize compactor/deploy/release >/dev/null
  kubectl kustomize itest/compactor >/dev/null
fi

[ "$MODE" != checks ] || exit 0

need y-cluster "https://github.com/Yolean/y-cluster/releases"
if [ "$MODE" != up ]; then
  need duckdb "https://duckdb.org/docs/installation/"
  # the assertions run compactor/sql/, which the compactor runs embedded at
  # this version; another CLI version tests the contract on another engine
  want=$(sed -n 's/^DUCKDB_VERSION=//p' compactor/build.env)
  got=$(duckdb --version | awk '{print $1}')
  [ "$got" = "$want" ] || echo "warning: duckdb $got, the compactor embeds $want (compactor/build.env)" >&2
fi

# ----------------------------------------------------------------- build --

phase "image: build $IMAGE"
mkdir -p target/images
if [ -n "${IMAGE_REF:-}" ]; then
  echo "IMAGE_REF=$IMAGE_REF, skipping the build"
elif [ "${SKIP_BUILD:-}" = 1 ] && [ -f target/images/node-agent.tar ]; then
  echo "SKIP_BUILD=1, reusing target/images/node-agent.tar"
else
  read -ra build_args <<< "${IMAGE_BUILD_ARGS:-}"
  docker buildx build "${build_args[@]}" \
    --output "type=oci,dest=target/images/node-agent.tar,name=docker.io/library/$IMAGE" \
    images/fluentbit
fi

# The compactor's image comes from its own build script, for this host's
# arch only (compactor/README.md, "The build script"); the release pin is
# build.sh check's business, not the itest's.
COMPACTOR_IMAGE=ghcr.io/yolean/blobs-compactor:build
phase "image: build $COMPACTOR_IMAGE"
if [ "${SKIP_BUILD:-}" = 1 ] && [ -f target/images/compactor-oci/index.json ]; then
  echo "SKIP_BUILD=1, reusing target/images/compactor-oci"
else
  PLATFORMS="linux/$(docker version --format '{{.Server.Arch}}')" compactor/build.sh image target/images/compactor-oci
fi

# --------------------------------------------------------------- cluster --

phase "cluster: $CONTEXT"
if [ -z "${CLUSTER_CONFIG:-}" ]; then
  if [ -e /dev/kvm ] && command -v qemu-system-x86_64 >/dev/null 2>&1; then
    CLUSTER_CONFIG=itest/cluster/qemu
  else
    CLUSTER_CONFIG=itest/cluster/docker
  fi
fi
if y-cluster detect --context "$CONTEXT" >/dev/null 2>&1; then
  echo "reusing running cluster"
else
  y-cluster provision -c "$CLUSTER_CONFIG"
fi
BACKEND=$(y-cluster detect --context "$CONTEXT")
echo "backend: $BACKEND"
$KUBECTL get nodes -o wide
# the docker node has no journal; see itest/k8s-docker
if [ "$BACKEND" = docker ]; then
  MANIFESTS=itest/k8s-docker
else
  MANIFESTS=itest/k8s
fi

phase "cluster: load image"
# images load skips refs that already exist in containerd, and the tag is
# fixed, so drop the previous build's ref first (running pods keep their
# snapshots; the rollout restart below moves them to the new one)
# (stdin closed: on the docker backend ctr runs through docker exec -i)
y-cluster ctr --context "$CONTEXT" -- -n k8s.io images rm "docker.io/library/$IMAGE" </dev/null >/dev/null 2>&1 || true
if [ -n "${IMAGE_REF:-}" ]; then
  y-cluster images load --context "$CONTEXT" "$IMAGE_REF" </dev/null
  y-cluster ctr --context "$CONTEXT" -- -n k8s.io images tag "$IMAGE_REF" "docker.io/library/$IMAGE" </dev/null >/dev/null
else
  y-cluster images load --context "$CONTEXT" ./target/images/node-agent.tar
fi
y-cluster ctr --context "$CONTEXT" -- -n k8s.io images rm "$COMPACTOR_IMAGE" </dev/null >/dev/null 2>&1 || true
y-cluster images load --context "$CONTEXT" ./target/images/compactor-oci

phase "cluster: apply"
# bare test pods are immutable in most fields; on a reused cluster a changed
# manifest must recreate them, so apply always starts from absence
$KUBECTL delete pod metrics-multi metrics-classic metrics-numeric metrics-optout \
  --ignore-not-found --grace-period=1 >/dev/null
$KUBECTL apply -k "$MANIFESTS"
$KUBECTL rollout status statefulset/versitygw --timeout=180s
$KUBECTL wait --for=condition=complete job/create-bucket --timeout=180s
# The image tag is fixed, so apply does not replace a running agent. Restart
# it onto the image just loaded, and wait: the rolling update removes the
# old pod first, so the previous build's last upload lands before the purge
# below, not after it (a reused cluster otherwise keeps one object of the
# old line format in the assertions' corpus).
$KUBECTL -n observability rollout restart daemonset/node-agent >/dev/null
$KUBECTL -n observability rollout status daemonset/node-agent --timeout=180s >/dev/null

# Objects from earlier runs would make every assertion scan an unbounded
# corpus. The bucket is test state; start empty. In up mode the bucket is
# the user's data: leave it alone.
if [ "$MODE" != up ]; then
phase "cluster: purge bucket prefix from earlier runs"
# the compactor writes hourly/ and daily/: stop it first, its pod gone
$KUBECTL -n observability delete deployment blobs-compactor --ignore-not-found --cascade=foreground >/dev/null
$KUBECTL delete pod purge-bucket --ignore-not-found >/dev/null 2>&1
$KUBECTL run purge-bucket --restart=Never --image=amazon/aws-cli:2.22.35 \
  --env=AWS_ACCESS_KEY_ID=demoaccess --env=AWS_SECRET_ACCESS_KEY=demosecret \
  --env=AWS_DEFAULT_REGION=us-east-1 \
  --command -- sh -ec "for p in raw1 hourly daily; do aws --endpoint-url http://versitygw:7070 s3 rm --recursive --quiet s3://$BUCKET/\$p/$CLUSTER; done" >/dev/null
# a failed purge leaves the old corpus under every assertion: stop on it
deadline=$((SECONDS + 600))
while :; do
  purge=$($KUBECTL get pod purge-bucket -o jsonpath='{.status.phase}')
  case "$purge" in (Succeeded|Failed) break;; esac
  [ $SECONDS -lt $deadline ] || fail "purge-bucket still $purge after 600s"
  sleep 3
done
if [ "$purge" != Succeeded ]; then
  $KUBECTL logs pod/purge-bucket >&2 || true
  fail "could not purge s3://$BUCKET/{raw1,hourly,daily}/$CLUSTER (pod/purge-bucket $purge, logs above)"
fi
$KUBECTL delete pod purge-bucket >/dev/null

# Startup output (the burst, the >16 KiB line, restart counting) must land
# after the purge, so the pods that produce it start over. On a reused
# cluster the crash-looper's backoff has also grown to minutes.
$KUBECTL delete pod restarter reused --ignore-not-found >/dev/null
$KUBECTL rollout restart deployment/log-generator >/dev/null
$KUBECTL apply -k "$MANIFESTS" >/dev/null
$KUBECTL rollout status deployment/log-generator --timeout=120s >/dev/null
fi
$KUBECTL -n observability rollout status daemonset/node-agent --timeout=180s
$KUBECTL rollout status deployment/log-generator --timeout=120s
$KUBECTL wait pod/metrics-multi pod/metrics-numeric pod/metrics-optout --for=condition=Ready --timeout=120s

# The compactor folds the bucket from here on. Its tag is fixed too, so a
# restart moves it onto the image just loaded.
$KUBECTL apply -k itest/compactor
$KUBECTL -n observability rollout restart deployment/blobs-compactor >/dev/null
$KUBECTL -n observability rollout status deployment/blobs-compactor --timeout=120s

if [ "$MODE" = up ]; then
  cat <<INFO

The agent, the compactor and the sample workloads are running.

  export KUBECONFIG=$PWD/itest/cluster/kubeconfig
  kubectl --context=$CONTEXT get pods -A
  kubectl --context=$CONTEXT -n observability logs ds/node-agent -f

One blob per node every 30 s lands in versitygw, reachable from this
machine on NodePort $S3_PORT. local/blobs.sh opens a DuckDB session with
a view per table over it:

  S3_ENDPOINT=127.0.0.1:$S3_PORT local/blobs.sh -c "SELECT \"table\", count(*) FROM raw GROUP BY 1"
  local/blobs.sh $CLUSTER/\$(date -u +%Y-%m-%d/%H)            # one hour, interactive

The compactor folds the raw objects into hourly/$CLUSTER/ every 20 s
(local/blobs.sh adds views over that tier once it exists):

  kubectl --context=$CONTEXT -n observability logs deploy/blobs-compactor -f

Tear everything down with:

  y-cluster teardown -c $CLUSTER_CONFIG
INFO
  exit 0
fi

# ------------------------------------------------------------ scenarios --

# The image tag is fixed, so a rebuilt image needs a restart to be picked
# up. The restart doubles as the exit-flush test: a marker written just
# before it must arrive exactly once, from the old pod's shutdown flush.
phase "scenario: restart the agent with a marker in flight"
MARKER="restart-marker-$(date +%s)"
$KUBECTL exec deploy/log-generator -- sh -c "echo '$MARKER' > /proc/1/fd/1"
# a kernel log line for the journal path (privileged: /dev/kmsg is the host's)
KMSG_MARKER="kmsg-marker-$(date +%s)"
# an interrupted run leaves the pod behind, and run would fail on it
$KUBECTL delete pod kmsg-writer --ignore-not-found >/dev/null 2>&1
$KUBECTL run kmsg-writer --rm -i --restart=Never --image=busybox:1.37 \
  --overrides='{"spec":{"containers":[{"name":"kmsg","image":"busybox:1.37","command":["sh","-c","echo \"node-agent itest '"$KMSG_MARKER"'\" > /dev/kmsg"],"securityContext":{"privileged":true}}]}}' \
  >/dev/null 2>&1 || echo "  (could not write to /dev/kmsg; the kernel assertion will fail)"
sleep 8
$KUBECTL -n observability rollout restart daemonset/node-agent
$KUBECTL -n observability rollout status daemonset/node-agent --timeout=180s
# objects after this minute come from one agent process
SINCE_EPOCH=$(( $(date -u +%s) / 60 * 60 ))
SINCE="TIMESTAMP '$(date -u +'%Y-%m-%d %H:%M:00')'"
echo "  SINCE=$SINCE"

# A short-lived pod for podindex lifecycle assertions: it must outlive a
# discovery interval, then its sandbox leaves the ready list.
$KUBECTL delete pod lifecycle-probe --ignore-not-found >/dev/null 2>&1
$KUBECTL run lifecycle-probe --restart=Never --image=busybox:1.37 --command -- sleep 25 >/dev/null

# The bucket goes away for longer than any retry budget of the previous
# generation; the agent must keep buffering and ship everything afterwards.
# The Service is what disappears (the versitygw data is an emptyDir), so
# uploads fail on DNS until it is back.
phase "scenario: bucket unreachable for 100 s"
$KUBECTL delete service versitygw >/dev/null
sleep 30
OUTAGE_MARKER="outage-marker-$(date +%s)"
$KUBECTL exec deploy/log-generator -- sh -c "echo '$OUTAGE_MARKER' > /proc/1/fd/1"
sleep 70
$KUBECTL apply -k "$MANIFESTS" >/dev/null
echo "  service restored"

# Three incarnations of one pod name: the index must keep them apart.
phase "scenario: three incarnations of pod 'reused'"
for i in 1 2 3; do
  $KUBECTL delete pod reused --ignore-not-found --wait=true >/dev/null
  $KUBECTL run reused --restart=Never --image=busybox:1.37 --labels=app=reused --command -- \
    sh -c "echo reused incarnation $i; sleep 15" >/dev/null
  $KUBECTL wait --for=jsonpath='{.status.phase}'=Succeeded pod/reused --timeout=120s >/dev/null
  # exits are observed at the discovery cadence (10 s); a pod deleted
  # within one interval of its exit takes its container away before the
  # agent sees it (README.md, "Known gaps")
  sleep 12
done

# ---------------------------------------------------------------- assert --

# One DuckDB init for every assertion: the secret, and the views the
# compactor and local/blobs.sh read from the same .sql files, so the
# column contract lives in exactly one place (compactor/sql/raw.sql).
INIT=$(mktemp "${TMPDIR:-/tmp}/itest.XXXXXX")
INIT_S3=$INIT.s3 SCOPED=$INIT.scoped SNAPSHOT=$INIT.raw.csv PODS_BEFORE=$INIT.pods.parquet
trap 'rm -f "$INIT" "$INIT_S3" "$SCOPED" "$SNAPSHOT" "$PODS_BEFORE"' EXIT
cat >"$INIT_S3" <<SQL
.output /dev/null
INSTALL httpfs; LOAD httpfs; SET TimeZone='UTC';
SET s3_region='us-east-1'; SET s3_endpoint='127.0.0.1:$S3_PORT';
SET s3_access_key_id='demoaccess'; SET s3_secret_access_key='demosecret';
SET s3_use_ssl=false; SET s3_url_style='path';
.output
SQL
cat >"$INIT" <<SQL
.read $INIT_S3
.output /dev/null
SET VARIABLE raw_files = 's3://$BUCKET/raw1/$CLUSTER/*/*/*/*.jsonl.zst';
.read $PWD/compactor/sql/raw.sql
.read $PWD/compactor/sql/tables.sql
.output
SQL
RAW_OBJECTS="read_json_objects('s3://$BUCKET/raw1/$CLUSTER/*/*/*/*.jsonl.zst', format='newline_delimited', compression='zstd')"
# the key's time: first record's time in the object
KEYTIME="strptime(regexp_extract(filename, '/([0-9]{4}-[0-9]{2}-[0-9]{2}/[0-9]{2})/[^/]+/([0-9]{4})-', ['d', 't'])['d'] || '/' || regexp_extract(filename, '/([0-9]{4}-[0-9]{2}-[0-9]{2}/[0-9]{2})/[^/]+/([0-9]{4})-', ['d', 't'])['t'], '%Y-%m-%d/%H/%M%S')"
# naive UTC like every time column (now() itself carries a time zone),
# truncated so that per-minute counts only see complete minutes
NOW="date_trunc('minute', now() AT TIME ZONE 'UTC')"

# .read via -c rather than -init: -init prints a banner with an ANSI reset
# that lands in front of the first value line
# (QINIT names another init, scoped() below)
q() { duckdb -noheader -csv -c ".read ${QINIT:-$INIT}" -c "$1" 2>&1; }
show() { duckdb -c ".read $INIT" -c "$1" 2>&1; }

ERRORS=0
assert() {
  # assert <description> <sql returning a single truthy value>
  local out
  out=$(q "$2" | tr -d '[:space:]' || true)
  if [ "$out" = true ] || [ "$out" = 1 ]; then
    echo "  PASS: $1"
  else
    echo "  FAIL: $1 (got: $out)" >&2
    ERRORS=$((ERRORS + 1))
  fi
}

phase "assert: waiting for the outage backlog and the incarnations to land"
deadline=$((SECONDS + 420))
while :; do
  n=$(q "SELECT count(DISTINCT pod_uid) FROM logs WHERE pod = 'reused'" | tr -d '[:space:]' || true)
  m=$(q "SELECT count(*) FROM logs WHERE message = '$OUTAGE_MARKER'" | tr -d '[:space:]' || true)
  case "$n" in (''|*[!0-9]*) n=0;; esac
  case "$m" in (''|*[!0-9]*) m=0;; esac
  echo "  incarnations of 'reused' in the blobs: $n, outage marker copies: $m"
  if [ "$n" -ge 3 ] && [ "$m" -ge 1 ]; then break; fi
  if [ $SECONDS -ge $deadline ]; then
    $KUBECTL -n observability logs ds/node-agent --tail=60 || true
    fail "blobs incomplete after 420s"
  fi
  sleep 15
done
# The per-minute assertions look at complete minutes from SINCE+3 (the
# outage backlog has landed by then) up to two minutes ago; give them at
# least two such minutes. The last incarnation's lines are in the agent's
# buffer for up to 36 s as well.
while [ "$(date -u +%s)" -lt $((SINCE_EPOCH + 7 * 60 + 10)) ]; do sleep 10; done

phase "assert: objects"
show "SELECT filename, \"table\", count(*) AS lines FROM raw GROUP BY ALL ORDER BY filename, \"table\""

assert "keys are /raw1/cluster/date/HH/node/MMSS-uuid.jsonl.zst" \
  "SELECT bool_and(regexp_matches(filename, '^s3://$BUCKET/raw1/$CLUSTER/\d{4}-\d{2}-\d{2}/\d{2}/[^/]+/\d{4}-[0-9A-Za-z]+\.jsonl\.zst$')) FROM raw"
# every minute of the window, so a minute without objects fails too
MINUTES="range($SINCE + INTERVAL 3 MINUTE, $NOW - INTERVAL 2 MINUTE, INTERVAL 1 MINUTE) AS w(m)"
assert "about two objects per minute (upload_timeout 30s)" \
  "SELECT bool_and(coalesce(n, 0) BETWEEN 1 AND 3) FROM $MINUTES LEFT JOIN (SELECT date_trunc('minute', $KEYTIME) AS m, count(DISTINCT filename) AS n FROM raw WHERE $KEYTIME > $SINCE + INTERVAL 3 MINUTE AND $KEYTIME < $NOW - INTERVAL 2 MINUTE GROUP BY m) USING (m)"
assert "every object after the outage carries logs and metrics lines" \
  "SELECT bool_and(has_logs AND has_metrics) FROM (SELECT filename, bool_or(\"table\" = 'logs') AS has_logs, bool_or(\"table\" = 'metrics') AS has_metrics FROM raw WHERE $KEYTIME > $SINCE + INTERVAL 3 MINUTE GROUP BY filename)"
# out_s3 renders ts truncated to the microsecond, so it can be 1 us below
# the source's nanosecond time (seen 2026-09-28: .536233 vs ts .536232)
assert "ts is the event time at microseconds on every line" \
  "SELECT bool_and(ts IS NOT NULL) AND bool_and(time IS NULL OR abs(epoch_ns(time) - epoch_ns(ts)) <= 1000) FROM raw"
assert "container log lines carry exactly the contract keys (no Lua, no leaked parser fields)" \
  "SELECT list_sort(list_distinct(flatten(list(json_keys(json))))) = ['cluster', 'container', 'message', 'namespace', 'node', 'pod', 'pod_uid', 'restart', 'stream', 'table', 'time', 'ts']
   FROM $RAW_OBJECTS
   WHERE json_extract_string(json, 'table') = 'logs' AND json_extract_string(json, 'pod') IS NOT NULL"
# The contract in both directions: a field the agent emits that raw.sql
# does not pin would be silently ignored, a pinned field no line carries
# is a rename or a typo. Journal-only fields are absent on the docker
# backend; exit_message is only set when the runtime has one.
if [ "$BACKEND" = docker ]; then ABSENT_OK="['unit', 'priority', 'exit_message']"; else ABSENT_OK="['exit_message']"; fi
assert "every field the agent emits is a pinned column of compactor/sql/raw.sql (offenders listed)" \
  "WITH seen AS (SELECT list_distinct(flatten(list(json_keys(json)))) AS keys FROM $RAW_OBJECTS),
        pinned AS (SELECT list(column_name) AS cols FROM (DESCRIBE raw))
   SELECT list_filter(seen.keys, lambda k: NOT list_contains(pinned.cols, k)) = [] FROM seen, pinned"
assert "every pinned column is emitted by some line (missing listed)" \
  "WITH seen AS (SELECT list_distinct(flatten(list(json_keys(json)))) AS keys FROM $RAW_OBJECTS),
        pinned AS (SELECT list(column_name) AS cols FROM (DESCRIBE raw))
   SELECT list_filter(pinned.cols, lambda c: c <> 'filename' AND NOT list_contains(seen.keys, c) AND NOT list_contains($ABSENT_OK, c)) = [] FROM seen, pinned"
assert "a sample carries exactly name, type, labels and value" \
  "SELECT list_sort(list_distinct(flatten(list(json_keys(json_extract(json, '\$.samples[0]')))))) = ['labels', 'name', 'type', 'value']
   FROM $RAW_OBJECTS WHERE json_extract_string(json, 'table') = 'metrics'"
assert "cluster and node are on every line" \
  "SELECT bool_and(cluster = '$CLUSTER' AND node IS NOT NULL AND node <> '') FROM raw"
assert "no line is of an unknown table" \
  "SELECT bool_and(\"table\" IN ('logs', 'metrics', 'podindex')) FROM raw"

# Container logs
assert "log-generator lines present with namespace/pod/container from the path" \
  "SELECT count(*) > 0 FROM logs WHERE namespace = 'default' AND pod LIKE 'log-generator-%' AND container = 'app' AND message LIKE '%hello from log-generator%'"
assert "stderr lines keep stream=stderr" \
  "SELECT count(*) > 0 FROM logs WHERE pod LIKE 'log-generator-%' AND stream = 'stderr' AND message LIKE 'stack trace example%'"
assert "CRI partial lines are joined (long line > 16 KiB arrives as one message)" \
  "SELECT max(length(message)) > 16384 FROM logs WHERE pod LIKE 'log-generator-%' AND message LIKE '%long-line%'"
assert "burst timestamps are strictly increasing with sub-second precision" \
  "SELECT count(*) >= 200 AND count(DISTINCT time) = count(*) FROM logs WHERE pod LIKE 'log-generator-%' AND message LIKE '%\"burst\"%'"
assert "pod_uid is filled for container logs" \
  "SELECT bool_and(length(pod_uid) = 36) FROM logs WHERE pod IS NOT NULL"
assert "restarts show up as restart > 0 for the crash-looping pod" \
  "SELECT max(restart) >= 1 FROM logs WHERE pod = 'restarter'"
assert "a line read before the restart is shipped by the exit flush, exactly once" \
  "SELECT count(*) = 1 FROM logs WHERE message = '$MARKER'"
assert "the agent's own logs are excluded" \
  "SELECT count(*) = 0 FROM logs WHERE pod LIKE 'node-agent-%'"

# Outage: nothing lost, nothing duplicated
assert "a line written while the bucket was unreachable arrives exactly once" \
  "SELECT count(*) = 1 FROM logs WHERE message = '$OUTAGE_MARKER'"
# per pod: a rollout-restarted generator overlaps its predecessor briefly
assert "the generator's counter is contiguous across the outage" \
  "WITH c AS (SELECT DISTINCT pod, CAST(json_extract(message, '$.count') AS INTEGER) AS n FROM logs
     WHERE pod LIKE 'log-generator-%' AND message LIKE '%hello from log-generator%' AND time > $SINCE),
   per_pod AS (SELECT pod, count(*) AS seen, min(n) AS lo, max(n) AS hi FROM c GROUP BY pod)
   SELECT bool_and(seen = (SELECT count(*) FROM range(lo, hi + 1) r WHERE r.range % 10 <> 0)) FROM per_pod"
# out_s3 v5.1.2 gives up on a chunk with "... failed to send N/M times, will
# not retry", then "retry-exhausted chunk moved to quarantine" or "...
# deleting retry-exhausted chunk"; the engine's own give-up is "chunk ...
# cannot be retried". The logs are read first: a failed read is a failure,
# not a pass.
if ! agent_logs=$($KUBECTL -n observability logs ds/node-agent --all-containers --prefix 2>&1); then
  echo "  FAIL: could not read the agent's logs: $agent_logs" >&2; ERRORS=$((ERRORS + 1))
elif grep -iE 'will not retry|retry-exhausted|quarantine|cannot be retried' <<<"$agent_logs" >&2; then
  echo "  FAIL: the agent gave up on a chunk (lines above)" >&2; ERRORS=$((ERRORS + 1))
else
  echo "  PASS: no chunk was given up on (nothing quarantined or deleted)"
fi

# Incarnations
assert "three incarnations of 'reused' have three pod_uids in the logs, each with its own line" \
  "SELECT count(DISTINCT pod_uid) = 3 AND count(DISTINCT message) = 3 AND count(*) = 3 FROM logs WHERE pod = 'reused' AND message LIKE 'reused incarnation%'"
assert "podindex records the completed incarnations' exits before their deletion" \
  "SELECT count(DISTINCT pod_uid) = 3 AND bool_and(exit_code = 0) FROM podindex WHERE kind = 'container-exit' AND pod = 'reused'"
assert "podindex observed each incarnation with its own pod_started" \
  "SELECT count(DISTINCT pod_uid) = 3 AND count(DISTINCT pod_started) = 3 FROM podindex WHERE kind = 'pod-observed' AND pod = 'reused'"

# Journal
if [ "$BACKEND" = docker ]; then
  echo "  SKIP: journal assertions (k3s-in-docker has no journal)"
else
  assert "journal rows from the k3s unit are present" \
    "SELECT count(*) > 0 FROM logs WHERE stream = 'journal' AND unit = 'k3s.service' AND namespace = '_node'"
  assert "kernel messages (the OOM-kill path) arrive with unit=kernel" \
    "SELECT count(*) > 0 FROM logs WHERE stream = 'journal' AND unit = 'kernel' AND message LIKE '%$KMSG_MARKER%'"
fi

# podindex
assert "podindex: pods observed with identity and pod_started" \
  "SELECT count(*) > 0 FROM podindex WHERE kind = 'pod-observed' AND pod = 'metrics-multi' AND length(pod_uid) = 36 AND pod_started IS NOT NULL"
assert "podindex: container rows carry image and restart count" \
  "SELECT count(*) > 0 FROM podindex WHERE kind = 'container' AND pod = 'metrics-multi' AND container = 'app' AND image LIKE '%busybox%' AND restart = 0"
assert "podindex: pod labels survive as a map without io.kubernetes noise" \
  "SELECT bool_and(labels['app'] = 'metrics-multi' AND NOT map_contains(labels, 'io.kubernetes.pod.name')) FROM podindex WHERE kind = 'pod-observed' AND pod = 'metrics-multi'"
assert "podindex: crash-looping container leaves container-exit rows with the code" \
  "SELECT count(*) > 0 FROM podindex WHERE kind = 'container-exit' AND pod = 'restarter' AND exit_code = 1"
# exit_message is the runtime's message; a plain exit 1 has none, and an
# absent message must be absent, not ''
assert "podindex: container-exit rows carry no empty exit_message" \
  "SELECT bool_and(exit_message IS NULL OR exit_message <> '') FROM podindex WHERE kind = 'container-exit' AND pod = 'restarter'"
assert "podindex: pod rows carry the QoS class and pod IP from the sandbox status" \
  "SELECT count(*) > 0 AND bool_and(qos_class = 'besteffort' AND regexp_matches(pod_ip, '^\d+\.\d+\.\d+\.\d+\$')) FROM podindex WHERE kind = 'pod-observed' AND pod = 'metrics-multi'"
assert "podindex: container rows carry the resolved image digest and start time" \
  "SELECT count(container_started) > 0 AND bool_and(image_id LIKE 'docker.io/library/busybox@sha256:%' AND coalesce(container_started <= time, true)) FROM podindex WHERE kind = 'container' AND pod = 'metrics-multi' AND container = 'app'"
assert "podindex: container and container-exit rows carry the runtime's bare container id, one per run" \
  "SELECT bool_and(regexp_matches(container_id, '^[0-9a-f]{64}\$')) AND count(DISTINCT container_id) = count(DISTINCT (pod_uid, container, restart)) FROM podindex WHERE kind IN ('container', 'container-exit')"
assert "podindex: a best-effort container has no limits, only the minimal cpu_shares" \
  "SELECT bool_and(memory_limit IS NULL AND cpu_quota IS NULL AND cpu_shares = 2) FROM podindex WHERE kind = 'container' AND pod = 'metrics-multi' AND container = 'app'"
assert "podindex: limits are carried for a guaranteed pod (64Mi, 100m)" \
  "SELECT count(*) > 0 AND bool_and(memory_limit = 67108864 AND cpu_quota = 10000 AND cpu_shares = 102) FROM podindex WHERE kind = 'container' AND pod = 'metrics-numeric' AND container = 'web-app'"
assert "podindex: the guaranteed pod's QoS class" \
  "SELECT count(*) > 0 AND bool_and(qos_class = 'guaranteed') FROM podindex WHERE kind = 'pod-observed' AND pod = 'metrics-numeric'"
assert "podindex: the short-lived probe was observed and its deletion recorded" \
  "SELECT count(*) FILTER (kind = 'pod-observed') > 0 AND count(*) FILTER (kind = 'pod-deleted') > 0 FROM podindex WHERE pod = 'lifecycle-probe' AND time > $SINCE"
assert "podindex: checkpoints attest long-lived pods (2m cadence in itest)" \
  "SELECT count(*) > 0 FROM podindex WHERE kind = 'pod-checkpoint' AND pod LIKE 'log-generator-%' AND time > $SINCE"
assert "podindex: fields a kind does not carry are absent, not zero" \
  "SELECT bool_and(container IS NULL AND exit_code IS NULL AND labels IS NOT NULL AND image_id IS NULL AND memory_limit IS NULL) FROM podindex WHERE kind = 'pod-observed'"
assert "podindex: container-exit rows carry no container-row facts, container rows no exit facts" \
  "SELECT bool_and(CASE kind WHEN 'container-exit' THEN image_id IS NULL AND container_started IS NULL ELSE exit_code IS NULL AND exit_message IS NULL END) FROM podindex WHERE kind LIKE 'container%'"

# Metrics: discovery by annotation, named ports, numeric ports, opt-out
assert "otel port-scoped hint scrapes the app container" \
  "SELECT count(*) > 0 FROM metrics WHERE pod = 'metrics-multi' AND container = 'app' AND name = 'app_requests_total' AND labels['code'] = '200' AND value = 42"
assert "otel hint by port name with metrics_path config scrapes the sidecar" \
  "SELECT count(*) > 0 FROM metrics WHERE pod = 'metrics-multi' AND container = 'sidecar' AND name = 'sidecar_up' AND value = 1"
assert "classic prometheus.io without a port scrapes every declared container port" \
  "SELECT count(DISTINCT container) = 2 FROM metrics WHERE pod = 'metrics-classic' AND name IN ('classic_one', 'classic_two')"
assert "histogram flattened with le labels, type=histogram" \
  "SELECT count(*) = 3 FROM (SELECT DISTINCT labels['le'] FROM metrics WHERE pod = 'metrics-multi' AND name = 'app_latency_seconds_bucket' AND type = 'histogram')"
assert "hanging container reports up=0" \
  "SELECT count(*) > 0 AND bool_and(value = 0) FROM metrics WHERE pod = 'metrics-multi' AND container = 'slow' AND name = 'up'"
assert "hanging container scrapes hit the 10s timeout" \
  "SELECT max(value) BETWEEN 9.5 AND 12 FROM metrics WHERE pod = 'metrics-multi' AND container = 'slow' AND name = 'scrape_duration_seconds'"
assert "container without declared ports is not scraped" \
  "SELECT count(*) = 0 FROM metrics WHERE pod = 'metrics-multi' AND container = 'noport'"
assert "numeric port + custom path scraped and attributed to the declaring container" \
  "SELECT count(*) > 0 FROM metrics WHERE pod = 'metrics-numeric' AND container = 'web-app' AND name = 'numeric_gauge' AND labels['kind'] = 'custom' AND value = 7"
assert "pod without the scrape annotation is not scraped" \
  "SELECT count(*) = 0 FROM metrics WHERE pod = 'metrics-optout'"
assert "metrics lines carry the scraped container's restart count, the agent's own included" \
  "SELECT count(*) > 0 AND bool_and(restart IS NOT NULL) FROM metrics WHERE container <> ''"
assert "pod targets carry pod_uid" \
  "SELECT bool_and(length(pod_uid) = 36) FROM metrics WHERE job = 'pod'"
assert "node_exporter metrics arrive via the static target with the agent pod's identity" \
  "SELECT count(*) > 0 FROM metrics WHERE job = 'node' AND name = 'node_cpu_seconds_total' AND namespace = 'observability' AND pod LIKE 'node-agent-%' AND container = 'agent' AND length(pod_uid) = 36"
assert "agent self metrics arrive via the static target" \
  "SELECT count(*) > 0 FROM metrics WHERE job = 'node' AND name LIKE 'fluentbit_%' AND pod LIKE 'node-agent-%'"
assert "healthy targets are scraped about every 15s" \
  "SELECT bool_and(coalesce(n, 0) BETWEEN 3 AND 5) FROM $MINUTES LEFT JOIN (SELECT date_trunc('minute', time) AS m, count(*) AS n FROM metrics WHERE pod = 'metrics-multi' AND container = 'app' AND name = 'up' AND time > $SINCE + INTERVAL 3 MINUTE AND time < $NOW - INTERVAL 2 MINUTE GROUP BY m) USING (m)"

# -------------------------------------------------------------- compactor --

phase "assert: compactor"
# The raw keys now. The compactor then runs on until a rolling and a past
# pass that both started after this snapshot have succeeded (read from its
# /metrics through the API server), so between them they listed every key
# of it, and is stopped: the sidecar assertion below checks the folds, not
# the wait. A stop lets the job in flight commit.
FOLDED="read_csv('s3://$BUCKET/hourly/$CLUSTER/_folded/*/*.csv', header = true, columns = {'key': 'VARCHAR', 'final': 'BOOLEAN'})"
RAW_KEYS="glob('s3://$BUCKET/raw1/$CLUSTER/*/*/*/*.jsonl.zst')"
q "COPY (SELECT file FROM $RAW_KEYS) TO '$SNAPSHOT' (FORMAT csv, HEADER true)" >/dev/null
SNAPSHOT_AT=$(($(date +%s) + 1))
# compactor_metric <name> <job>: that sample of the running compactor's /metrics
compactor_metric() {
  local pod
  pod=$($KUBECTL -n observability get pods -l app=blobs-compactor -o jsonpath='{.items[0].metadata.name}' 2>/dev/null) || return 0
  [ -n "$pod" ] || return 0
  $KUBECTL get --raw "/api/v1/namespaces/observability/pods/$pod:9090/proxy/metrics" 2>/dev/null \
    | awk -v k="$1{job=\"$2\"}" '$1 == k { print $2 }'
}
# started_since <job> <epoch seconds>: the job's last run succeeded and
# started after then (its end less its duration)
started_since() {
  local end took
  end=$(compactor_metric blobs_compactor_last_success_timestamp_seconds "$1")
  took=$(compactor_metric blobs_compactor_last_duration_seconds "$1")
  [ -n "$end" ] && [ -n "$took" ] && awk -v e="$end" -v d="$took" -v t="$2" 'BEGIN { exit !(e - d > t) }'
}
deadline=$((SECONDS + 600))
until started_since rolling "$SNAPSHOT_AT" && started_since past "$SNAPSHOT_AT"; do
  if [ $SECONDS -ge $deadline ]; then
    $KUBECTL -n observability logs deploy/blobs-compactor --tail=40 || true
    fail "the compactor has not completed a rolling and a past pass in the 600s after the snapshot"
  fi
  echo "  waiting for a rolling and a past pass that start after the snapshot"
  sleep 10
done
$KUBECTL -n observability scale deployment/blobs-compactor --replicas=0 >/dev/null
deadline=$((SECONDS + 330))
while [ -n "$($KUBECTL -n observability get pods -l app=blobs-compactor -o name)" ]; do
  [ $SECONDS -lt $deadline ] || fail "the compactor's pod is still there 330s after scaling to 0"
  sleep 2
done

assert "the compactor's own metrics reach the blobs through the agent's scrape" \
  "SELECT count(*) > 0 FROM metrics WHERE pod LIKE 'blobs-compactor-%' AND name = 'blobs_compactor_last_success_timestamp_seconds'"
assert "every raw key in the bucket before the snapshot is listed in its hour's sidecar" \
  "SELECT count(*) = 0 FROM read_csv('$SNAPSHOT', header = true) s WHERE NOT EXISTS (SELECT 1 FROM $FOLDED f WHERE 's3://$BUCKET/' || f.key = s.file AND regexp_extract(f.key, '^raw1/[^/]+/([0-9-]+/[0-9]{2})/', 1) = regexp_extract(s.file, '/raw1/[^/]+/([0-9-]+/[0-9]{2})/', 1))"
assert "the sidecars list only keys the raw tier has" \
  "SELECT count(*) = 0 FROM $FOLDED WHERE 's3://$BUCKET/' || key NOT IN (SELECT file FROM $RAW_KEYS)"

# scoped <file glob of sidecars>: an init whose raw views read exactly the
# raw keys those sidecars list
scoped() {
  cat >"$SCOPED" <<SQL
.read $INIT_S3
.output /dev/null
SET VARIABLE raw_files = (SELECT list(DISTINCT 's3://$BUCKET/' || key) FROM read_csv('s3://$BUCKET/hourly/$CLUSTER/_folded/$1', header = true, columns = {'key': 'VARCHAR', 'final': 'BOOLEAN'}));
.read $PWD/compactor/sql/raw.sql
.read $PWD/compactor/sql/tables.sql
.output
SQL
}
# same_rows <view> <FROM source>: SQL that is true when the source holds
# exactly the distinct rows of the view, filename dropped, each once
same_rows() {
  echo "(SELECT count(*) FROM (SELECT DISTINCT * EXCLUDE (filename) FROM $1 EXCEPT SELECT * FROM $2)) = 0
    AND (SELECT count(*) FROM (SELECT * FROM $2 EXCEPT SELECT * EXCLUDE (filename) FROM $1)) = 0
    AND (SELECT count(*) FROM (SELECT DISTINCT * EXCLUDE (filename) FROM $1)) = (SELECT count(*) FROM $2)"
}
hourly() { echo "read_parquet('s3://$BUCKET/hourly/$CLUSTER/$1/$2.parquet')"; }
daily() {
  case $1 in
    (logs|metrics) echo "read_parquet('s3://$BUCKET/daily/$CLUSTER/$1/$2/*/*.parquet', hive_partitioning = false)" ;;
    (*) echo "read_parquet('s3://$BUCKET/daily/$CLUSTER/$1/$2.parquet')" ;;
  esac
}
for hour in $(q "SELECT regexp_extract(file, '_folded/(.*)\\.csv\$', 1) FROM glob('s3://$BUCKET/hourly/$CLUSTER/_folded/*/*.csv') ORDER BY 1"); do
  scoped "$hour.csv"
  QINIT=$SCOPED assert "hourly $hour: logs, metrics and podindex hold exactly the distinct rows of the raw keys its sidecar lists" \
    "SELECT $(same_rows logs "$(hourly logs "$hour")") AND $(same_rows metrics "$(hourly metrics "$hour")")
        AND $(same_rows podindex "$(hourly podindex "$hour")") AND (SELECT count(*) FROM logs) > 0"
done
assert "hourly pods: the three incarnations of 'reused', each its own row" \
  "SELECT count(DISTINCT pod_uid) = 3 FROM read_parquet('s3://$BUCKET/hourly/$CLUSTER/pods/*/*.parquet') WHERE pod = 'reused' AND container = 'reused'"

# compactor_job <args, a JSON array>: blobs-compactor with those args in a
# pod of its own, as the only writer; its log is in JOB_LOG
compactor_job() {
  local job
  $KUBECTL -n observability delete pod compactor-job --ignore-not-found >/dev/null 2>&1
  $KUBECTL -n observability run compactor-job --restart=Never --image="$COMPACTOR_IMAGE" --overrides="{
    \"apiVersion\": \"v1\", \"spec\": {\"automountServiceAccountToken\": false, \"containers\": [{
      \"name\": \"compactor-job\", \"image\": \"$COMPACTOR_IMAGE\", \"imagePullPolicy\": \"Never\",
      \"args\": $1, \"envFrom\": [{\"secretRef\": {\"name\": \"blobs-compactor-s3\"}}],
      \"env\": [{\"name\": \"CLUSTER_NAME\", \"value\": \"$CLUSTER\"}, {\"name\": \"S3_BUCKET\", \"value\": \"$BUCKET\"},
        {\"name\": \"S3_ENDPOINT\", \"value\": \"versitygw.default.svc.cluster.local:7070\"}, {\"name\": \"S3_TLS\", \"value\": \"off\"}]}]}}" >/dev/null
  deadline=$((SECONDS + 300))
  while :; do
    job=$($KUBECTL -n observability get pod compactor-job -o jsonpath='{.status.phase}')
    case "$job" in (Succeeded|Failed) break;; esac
    [ $SECONDS -lt $deadline ] || fail "compactor-job $1 still $job after 300s"
    sleep 3
  done
  JOB_LOG=$($KUBECTL -n observability logs pod/compactor-job)
  echo "$JOB_LOG" | tail -n 3
  $KUBECTL -n observability delete pod compactor-job >/dev/null
  if [ "$job" = Succeeded ]; then
    echo "  PASS: blobs-compactor $1"
  else
    echo "  FAIL: blobs-compactor $1 ($job, logs above)" >&2; ERRORS=$((ERRORS + 1))
  fi
}

# Every day with folded hours, rolled up by hand: more than one when the
# run crossed midnight UTC (the loop rolls up a past day itself; a rollup
# with nothing new does nothing).
DAYS=$(q "SELECT DISTINCT regexp_extract(file, '_folded/([0-9-]+)/', 1) AS d FROM glob('s3://$BUCKET/hourly/$CLUSTER/_folded/*/*.csv') ORDER BY d")
[ -n "$DAYS" ] || fail "no hourly sidecars"
# daily_rows <day> <assertion label>: the day's logs, metrics and podindex
# against the distinct rows of the raw keys its hourly sidecars list
daily_rows() {
  scoped "$1/*.csv"
  QINIT=$SCOPED assert "daily $1: $2" \
    "SELECT $(same_rows logs "$(daily logs "$1")") AND $(same_rows metrics "$(daily metrics "$1")")
        AND $(same_rows podindex "$(daily podindex "$1")") AND (SELECT count(*) FROM logs) > 0"
}
for day in $DAYS; do
  echo "  rollup $day"
  compactor_job "[\"rollup\", \"$day\"]"
  daily_rows "$day" "logs, metrics and podindex hold exactly the distinct rows of the day's folded raw keys"
  assert "daily $day: the sidecar lists every key of the day's hourly sidecars" \
    "SELECT (SELECT list(key ORDER BY key) FROM read_csv('s3://$BUCKET/daily/$CLUSTER/_folded/$day.csv.zst', header = true))
          = (SELECT list(DISTINCT key ORDER BY key) FROM read_csv('s3://$BUCKET/hourly/$CLUSTER/_folded/$day/*.csv', header = true))"
done
LAST=$(echo "$DAYS" | tail -n 1)
assert "daily $LAST: a namespace's logs are one object, found by its hive key" \
  "SELECT count(*) = 1 AND bool_and(namespace = 'default') FROM (SELECT DISTINCT namespace, filename FROM read_parquet('s3://$BUCKET/daily/$CLUSTER/logs/$LAST/*/*.parquet', hive_partitioning = true, filename = true) WHERE namespace = 'default')"
# per incarnation over the days: one that ran across midnight has a row in each
assert "daily pods: the three incarnations of 'reused', each with its one line and exit 0" \
  "SELECT count(*) = 3 AND bool_and(lines = 1) AND bool_and(exited) AND count(*) FILTER (succeeded) >= 2
   FROM (SELECT pod_uid, sum(lines) AS lines, bool_or(exit_code = 0) AS exited, bool_or(phase = 'succeeded') AS succeeded
         FROM read_parquet('s3://$BUCKET/daily/$CLUSTER/pods/*.parquet') WHERE pod = 'reused' AND container = 'reused' GROUP BY pod_uid)"

# An incremental rollup: a raw object of a namespace of its own, in an hour
# of the last day that has no keys yet and that the agent is not uploading
# to, folded by hand, then the day rolled up again. Only that namespace is
# rewritten; the other namespaces' pods rows stay as they were.
LATE_HOUR=$(q "SELECT lpad(min(h)::VARCHAR, 2, '0') FROM range(24) t(h)
  WHERE abs(epoch(strptime('$LAST', '%Y-%m-%d') + to_hours(h)) - epoch($NOW)) >= 7200
    AND lpad(h::VARCHAR, 2, '0') NOT IN (SELECT regexp_extract(file, '/$LAST/([0-9]{2})/', 1) FROM $RAW_KEYS)
    AND lpad(h::VARCHAR, 2, '0') NOT IN (SELECT regexp_extract(file, '_folded/$LAST/([0-9]{2})\\.csv\$', 1) FROM glob('s3://$BUCKET/hourly/$CLUSTER/_folded/$LAST/*.csv'))" | tr -d '[:space:]')
[ -n "$LATE_HOUR" ] || fail "no free hour in $LAST for the incremental rollup"
LATE_UID=$(q "SELECT uuid()" | tr -d '[:space:]')
q "COPY (SELECT * FROM $(daily pods "$LAST")) TO '$PODS_BEFORE' (FORMAT parquet)" >/dev/null || fail "snapshot of the daily pods of $LAST"
q "COPY (
  SELECT 'logs' AS \"table\", t AS time, t AS ts, '$CLUSTER' AS cluster, 'itest-late' AS node, 'itest-late' AS namespace,
         'late' AS pod, '$LATE_UID' AS pod_uid, 'late' AS container, 0 AS restart, 'stdout' AS stream, 'late line ' || i AS message
  FROM (SELECT i, strptime('$LAST $LATE_HOUR', '%Y-%m-%d %H')::TIMESTAMP_NS + to_seconds(i) AS t FROM range(3) r(i))
  UNION ALL BY NAME
  SELECT 'podindex' AS \"table\", t AS time, t AS ts, '$CLUSTER' AS cluster, 'itest-late' AS node, 'pod-observed' AS kind,
         'itest-late' AS namespace, 'late' AS pod, '$LATE_UID' AS pod_uid, t AS pod_started
  FROM (SELECT strptime('$LAST $LATE_HOUR', '%Y-%m-%d %H')::TIMESTAMP_NS AS t)
) TO 's3://$BUCKET/raw1/$CLUSTER/$LAST/$LATE_HOUR/itest-late/0000-$LATE_UID.jsonl.zst' (FORMAT json, COMPRESSION zstd)" >/dev/null \
  || fail "upload of the late raw object"
echo "  fold $LAST $LATE_HOUR, rollup $LAST"
compactor_job "[\"fold\", \"$LAST\", \"$LATE_HOUR\"]"
compactor_job "[\"rollup\", \"$LAST\"]"
if echo "$JOB_LOG" | grep -q '"hours":1,"namespaces":1,'; then
  echo "  PASS: the incremental rollup rewrote one hour's one namespace"
else
  echo "  FAIL: the incremental rollup did not rewrite exactly one hour's one namespace" >&2; ERRORS=$((ERRORS + 1))
fi
daily_rows "$LAST" "after the incremental rollup, still exactly the distinct rows of the day's folded raw keys"
assert "daily $LAST: the incremental rollup kept the other namespaces' pods rows as they were" \
  "SELECT (SELECT count(*) FROM (SELECT * FROM read_parquet('$PODS_BEFORE') EXCEPT SELECT * FROM $(daily pods "$LAST") WHERE namespace <> 'itest-late')) = 0
      AND (SELECT count(*) FROM (SELECT * FROM $(daily pods "$LAST") WHERE namespace <> 'itest-late' EXCEPT SELECT * FROM read_parquet('$PODS_BEFORE'))) = 0
      AND (SELECT count(*) FROM read_parquet('$PODS_BEFORE')) = (SELECT count(*) FROM $(daily pods "$LAST") WHERE namespace <> 'itest-late')"
assert "daily $LAST: the late namespace's pods row has its three lines" \
  "SELECT count(*) = 1 AND bool_and(lines = 3) FROM $(daily pods "$LAST") WHERE namespace = 'itest-late' AND pod_uid = '$LATE_UID'"
$KUBECTL -n observability scale deployment/blobs-compactor --replicas=1 >/dev/null

echo
if [ "$ERRORS" -gt 0 ]; then
  $KUBECTL -n observability logs ds/node-agent --tail=40 || true
  fail "$ERRORS assertion(s) failed"
fi
echo "PASS: all assertions passed"
