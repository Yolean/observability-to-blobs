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
#   ./test.sh host       only the host phase (HOST_MODE.md) against the running
#                        qemu cluster: itest/host/phase.sh. It also runs at
#                        the end of the full run, on the qemu backend.
#
# Env:
#   SKIP_BUILD=1         reuse target/images/node-agent.tar and
#                        target/images/compactor-oci
#   HOST_TARBALL=file    run the host phase with this tarball instead of
#                        building one (HOST_BUILD_ARGS: extra buildx flags;
#                        more in itest/host/phase.sh)
#   IMAGE_REF=ref        load a published image instead of building
#   CLUSTER_CONFIG=dir   y-cluster config dir (default: itest/cluster/qemu with
#                        /dev/kvm, itest/cluster/docker otherwise)
#   CONTEXT, S3_PORT     must match CLUSTER_CONFIG when overriding it
#   BUILDKIT_PORT, LOGS_PORT, PUT_LOGS_PORT, SERVE_LOGS_PORT, LOCAL_PORT
#                        this host's ports for the port-forwards to the
#                        cluster's BuildKit (default 18234), the logs API of
#                        the compactor (18080), of the put compactor (18081)
#                        and of the serve pod (18082), and the host phase's
#                        gateway (17071, itest/host/run.sh). Two itests on
#                        one machine need their own, as well as their own
#                        CLUSTER_CONFIG, CONTEXT and S3_PORT; a port that
#                        already answers fails the run instead of reading
#                        the other itest's cluster.
#   IMAGE_BUILD_ARGS     extra docker buildx flags, e.g. a CI cache
#   KUBEBUILDER_ASSETS   etcd, kube-apiserver and kubectl at ENVTEST_K8S for
#                        tools/y-obs's tests (default: setup-envtest
#                        downloads them; the itest's are the cluster's release)
#   IMAGE_BUILDER        docker (default when docker is installed), buildkit
#                        (buildctl against BUILDKIT_HOST, the default when it is
#                        set; with BUILDKIT_TLS_DIR when that is set too, as
#                        eval "$(y-cluster dockerhost env)" sets both) or cluster (buildctl
#                        against a rootless BuildKit pod in the itest cluster,
#                        default without docker: no daemon on the host at all)
#
# The cluster is left running; y-cluster teardown -c <config dir> removes it.
set -euo pipefail
cd "$(dirname "$0")"

CONTEXT=${CONTEXT:-node-agent}
IMAGE=node-agent:itest
BUCKET=node-agent
CLUSTER=itest
# the second compactor, in spool mode, and the stock Fluent Bit that
# uploads to its put endpoint (itest/compactor-put)
PUT_NS=observability-put
PUT_CLUSTER=itest-put
S3_PORT=${S3_PORT:-30070}
# this host's ends of the port-forwards (the Env above)
BUILDKIT_PORT=${BUILDKIT_PORT:-18234}
LOGS_PORT=${LOGS_PORT:-18080}
PUT_LOGS_PORT=${PUT_LOGS_PORT:-18081}
SERVE_LOGS_PORT=${SERVE_LOGS_PORT:-18082}
export LOCAL_PORT=${LOCAL_PORT:-17071}
# the compactor's parquet keys (itest/compactor), kept across runs so that
# a bucket left by `up` stays readable: PARQUET_KEYS_DIR=$PARQUET_KEYS local/blobs.sh
PARQUET_KEYS=target/itest/parquet-keys
export KUBECONFIG=$PWD/itest/cluster/kubeconfig
KUBECTL="kubectl --context=$CONTEXT"
MODE=${1:-all}
case "$MODE" in (all|checks|itest|up|host) ;; (*) echo "usage: $0 [checks|itest|up|host]" >&2; exit 2;; esac
[ "$MODE" != host ] || exec env CONTEXT="$CONTEXT" CLUSTER_CONFIG="${CLUSTER_CONFIG:-itest/cluster/qemu}" itest/host/phase.sh

# one exit trap for the whole script: a second `trap ... EXIT` replaces the
# first, which would leave the BuildKit port-forward running
BUILDKIT_PF=""
INIT=""
# apiserver-at in the background (at_serve below)
AT_PID=""
# the port-forwards to the logs API (logs_forward below)
LOGS_PF_PIDS=()
cleanup() {
  [ -z "$BUILDKIT_PF" ] || kill "$BUILDKIT_PF" 2>/dev/null || true
  [ -z "$AT_PID" ] || kill "$AT_PID" 2>/dev/null || true
  [ ${#LOGS_PF_PIDS[@]} -eq 0 ] || kill "${LOGS_PF_PIDS[@]}" 2>/dev/null || true
  # the assertions' temporary files, all named after INIT
  [ -z "$INIT" ] || rm -f "$INIT" "$INIT".*
}
trap cleanup EXIT

phase() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
# port_free <port> <variable>: nothing listens on 127.0.0.1:<port>, else
# fail: a port-forward to it could not bind, and the run would read
# whatever answers there, such as another itest's cluster
port_free() {
  if (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; then
    fail "127.0.0.1:$1 is in use (another itest?); set $2 to a free port"
  fi
}
need() {
  command -v "$1" >/dev/null 2>&1 || fail "missing tool: $1 ($2)"
}
# The Kubernetes release of the API servers in tools/y-obs's
# acceptance test, the source and apiserver-at both. The itest's
# apiserver-at runs the itest cluster's own release instead (at_assets).
ENVTEST_K8S=1.37.0
SETUP_ENVTEST=v0.25.2
# envtest_assets: KUBEBUILDER_ASSETS, downloaded once into setup-envtest's
# cache unless already set
envtest_assets() {
  if [ -z "${KUBEBUILDER_ASSETS:-}" ]; then
    KUBEBUILDER_ASSETS=$(GOFLAGS='' go run "sigs.k8s.io/controller-runtime/tools/setup-envtest@$SETUP_ENVTEST" use "$ENVTEST_K8S" -p path) \
      || fail "setup-envtest could not provide Kubernetes $ENVTEST_K8S"
  fi
  export KUBEBUILDER_ASSETS
}
# run_pod <namespace> <name> <image> <overrides JSON, or ""> <command...>:
# a pod that runs to completion; its log is in POD_LOG, and it returns
# non-zero unless the pod succeeded
run_pod() {
  local ns=$1 name=$2 image=$3 overrides=$4 phase deadline
  shift 4
  local args=(--restart=Never --image="$image")
  [ -z "$overrides" ] || args+=(--overrides="$overrides")
  $KUBECTL -n "$ns" delete pod "$name" --ignore-not-found >/dev/null 2>&1
  $KUBECTL -n "$ns" run "$name" "${args[@]}" --command -- "$@" >/dev/null
  deadline=$((SECONDS + 300))
  while :; do
    phase=$($KUBECTL -n "$ns" get pod "$name" -o jsonpath='{.status.phase}')
    case "$phase" in (Succeeded|Failed) break;; esac
    [ $SECONDS -lt $deadline ] || fail "pod $ns/$name still $phase after 300s"
    sleep 2
  done
  POD_LOG=$($KUBECTL -n "$ns" logs "pod/$name")
  $KUBECTL -n "$ns" delete pod "$name" >/dev/null
  [ "$phase" = Succeeded ]
}
# spool_sh <command>: a shell command, without double quotes, in a pod
# that mounts the put compactor's spool at /spool (ReadWriteOnce: pods on
# its node share it)
spool_sh() {
  run_pod "$PUT_NS" spool-sh busybox:1.37 "{\"apiVersion\": \"v1\", \"spec\": {\"containers\": [{
    \"name\": \"spool-sh\", \"image\": \"busybox:1.37\", \"command\": [\"sh\", \"-c\", \"$1\"],
    \"volumeMounts\": [{\"name\": \"spool\", \"mountPath\": \"/spool\"}]}],
    \"volumes\": [{\"name\": \"spool\", \"persistentVolumeClaim\": {\"claimName\": \"blobs-compactor-spool\"}}]}}" sh -c "$1"
}

need kubectl "https://kubernetes.io/docs/tasks/tools/"

# ---------------------------------------------------------------- checks --

if [ "$MODE" = all ] || [ "$MODE" = checks ]; then
  need go "https://go.dev/dl/"
  need shellcheck "https://github.com/koalaman/shellcheck"
  phase "checks: shell"
  shellcheck -x -P SCRIPTDIR test.sh compactor/build.sh local/kubectl-at.sh operator-to-annotations/crds/vendor.sh tools/y-obs/release.sh

  # tools/y-obs's acceptance test runs a source API server, the
  # watcher, compactor/sql/ in DuckDB and apiserver-at
  need duckdb "https://duckdb.org/docs/installation/"
  envtest_assets
  for mod in images/fluentbit/plugins/cri_scrape images/fluentbit/plugins/k8s_objects images/blobs-to-prometheus \
    images/operator-to-annotations tools/rules-manifest tools/y-obs; do
    phase "checks: go ($mod)"
    (
      cd "$mod"
      unformatted=$(gofmt -l .)
      [ -z "$unformatted" ] || fail "gofmt: $unformatted"
      go vet ./...
      go test ./...
    )
  done

  phase "checks: host mode"
  need jq "https://jqlang.github.io/jq/"
  need python3 "python3"
  shellcheck images/fluentbit/host/*.sh itest/host/*.sh
  images/fluentbit/host/test-heartbeat.sh
  python3 -c 'import ast, sys; ast.parse(open(sys.argv[1]).read())' itest/host/s3-recorder.py
  kubectl kustomize itest/host >/dev/null

  phase "checks: kustomize"
  kubectl kustomize agent >/dev/null
  kubectl kustomize k8sagent >/dev/null
  kubectl kustomize itest/k8s >/dev/null
  kubectl kustomize itest/k8s-docker >/dev/null
  kubectl kustomize compactor/deploy/release >/dev/null
  kubectl kustomize itest/compactor >/dev/null
  kubectl kustomize itest/compactor-put >/dev/null
  kubectl kustomize itest/compactor-multi >/dev/null
  kubectl kustomize compactor/deploy/spool-networkpolicy >/dev/null
  kubectl kustomize itest/buildkit >/dev/null
  operator-to-annotations/crds/vendor.sh check
  kubectl kustomize operator-to-annotations >/dev/null
  kubectl kustomize itest/operator-to-annotations >/dev/null
  kubectl kustomize itest/operator-to-annotations/workloads >/dev/null
fi

[ "$MODE" != checks ] || exit 0

need y-cluster "https://github.com/Yolean/y-cluster/releases"
if [ "$MODE" != up ]; then
  need duckdb "https://duckdb.org/docs/installation/"
  need curl "https://curl.se/download.html"
  # the assertions run compactor/sql/, which the compactor runs embedded at
  # this version; another CLI version tests the contract on another engine
  want=$(sed -n 's/^DUCKDB_VERSION=//p' compactor/build.env)
  got=$(duckdb --version | awk '{print $1}')
  [ "$got" = "$want" ] || echo "warning: duckdb $got, the compactor embeds $want (compactor/build.env)" >&2
fi

# ----------------------------------------------------------------- build --

if [ -z "${IMAGE_BUILDER:-}" ]; then
  if [ -n "${BUILDKIT_HOST:-}" ]; then IMAGE_BUILDER=buildkit
  elif command -v docker >/dev/null 2>&1; then IMAGE_BUILDER=docker
  else IMAGE_BUILDER=cluster; fi
fi
case "$IMAGE_BUILDER" in (docker|buildkit|cluster) ;; (*) fail "IMAGE_BUILDER: docker, buildkit or cluster";; esac
# a BuildKit that wants client certificates (y-cluster dockerhost); exported
# for compactor/build.sh and images/fluentbit/host/build.sh too
if [ "$IMAGE_BUILDER" = buildkit ] && [ -n "${BUILDKIT_TLS_DIR:-}" ]; then
  buildctl() { command buildctl --tlsdir "$BUILDKIT_TLS_DIR" "$@"; }
  export -f buildctl
fi
BUILD=yes
if [ -n "${IMAGE_REF:-}" ] || { [ "${SKIP_BUILD:-}" = 1 ] && [ -f target/images/node-agent.tar ]; }; then
  BUILD=no
elif [ "$IMAGE_BUILDER" = docker ]; then
  need docker "https://docs.docker.com/engine/install/"
else
  need buildctl "https://github.com/moby/buildkit/releases"
fi

build_image() {
  phase "image: build $IMAGE ($IMAGE_BUILDER)"
  mkdir -p target/images
  if [ -n "${IMAGE_REF:-}" ]; then
    echo "IMAGE_REF=$IMAGE_REF, skipping the build"
  elif [ "$BUILD" = no ]; then
    echo "SKIP_BUILD=1, reusing target/images/node-agent.tar"
  elif [ "$IMAGE_BUILDER" = docker ]; then
    read -ra build_args <<< "${IMAGE_BUILD_ARGS:-}"
    docker buildx build "${build_args[@]}" \
      --output "type=oci,dest=target/images/node-agent.tar,name=docker.io/library/$IMAGE" \
      images/fluentbit
  else
    buildctl build --frontend dockerfile.v0 \
      --local context=images/fluentbit --local dockerfile=images/fluentbit \
      --output "type=oci,dest=target/images/node-agent.tar,name=docker.io/library/$IMAGE"
  fi
}

# operator-to-annotations' controller (operator-to-annotations/README.md)
CONTROLLER_IMAGE=operator-to-annotations:itest
build_controller() {
  phase "image: build $CONTROLLER_IMAGE ($IMAGE_BUILDER)"
  if [ "${SKIP_BUILD:-}" = 1 ] && [ -f target/images/operator-to-annotations.tar ]; then
    echo "SKIP_BUILD=1, reusing target/images/operator-to-annotations.tar"
  elif [ "$IMAGE_BUILDER" = docker ]; then
    docker buildx build \
      --output "type=oci,dest=target/images/operator-to-annotations.tar,name=docker.io/library/$CONTROLLER_IMAGE" \
      images/operator-to-annotations
  else
    buildctl build --frontend dockerfile.v0 \
      --local context=images/operator-to-annotations --local dockerfile=images/operator-to-annotations \
      --output "type=oci,dest=target/images/operator-to-annotations.tar,name=docker.io/library/$CONTROLLER_IMAGE"
  fi
}

# BuildKit in the itest cluster, reached through a port-forward, so a host
# with neither docker nor a BuildKit daemon can build
cluster_builder() {
  phase "image: BuildKit in $CONTEXT"
  $KUBECTL apply -k itest/buildkit >/dev/null
  $KUBECTL -n itest-buildkit rollout status deployment/buildkitd --timeout=300s
  port_free "$BUILDKIT_PORT" BUILDKIT_PORT
  $KUBECTL -n itest-buildkit port-forward deployment/buildkitd "$BUILDKIT_PORT:1234" >/dev/null 2>&1 &
  BUILDKIT_PF=$!
  export BUILDKIT_HOST=tcp://127.0.0.1:$BUILDKIT_PORT
  for _ in $(seq 1 30); do buildctl debug workers >/dev/null 2>&1 && return 0; sleep 1; done
  fail "BuildKit in $CONTEXT does not answer on $BUILDKIT_HOST"
}

# The compactor's image comes from its own build script, for this host's
# arch only (compactor/README.md, "The build script"); the release pin is
# build.sh check's business, not the itest's. Without docker, build.sh's
# image-buildkit runs the same steps through buildctl.
COMPACTOR_IMAGE=ghcr.io/yolean/observability-state:build
build_compactor() {
  phase "image: build $COMPACTOR_IMAGE ($IMAGE_BUILDER)"
  if [ "${SKIP_BUILD:-}" = 1 ] && [ -f target/images/compactor-oci/index.json ]; then
    echo "SKIP_BUILD=1, reusing target/images/compactor-oci"
  elif [ "$IMAGE_BUILDER" = docker ]; then
    PLATFORMS="linux/$(docker version --format '{{.Server.Arch}}')" compactor/build.sh image target/images/compactor-oci
  else
    PLATFORMS="linux/$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" compactor/build.sh image-buildkit target/images/compactor-oci
  fi
}

# the cluster builder needs the cluster first; the others build before it
if [ "$IMAGE_BUILDER" != cluster ]; then
  build_image
  build_compactor
  build_controller
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

if [ "$IMAGE_BUILDER" = cluster ]; then
  if [ "$BUILD" = yes ] || [ "${SKIP_BUILD:-}" != 1 ] || [ ! -f target/images/compactor-oci/index.json ] \
    || [ ! -f target/images/operator-to-annotations.tar ]; then
    cluster_builder
  fi
  build_image
  build_compactor
  build_controller
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
y-cluster ctr --context "$CONTEXT" -- -n k8s.io images rm "docker.io/library/$CONTROLLER_IMAGE" </dev/null >/dev/null 2>&1 || true
y-cluster images load --context "$CONTEXT" ./target/images/operator-to-annotations.tar

phase "cluster: apply"
# bare test pods are immutable in most fields; on a reused cluster a changed
# manifest must recreate them, so apply always starts from absence
$KUBECTL delete pod metrics-multi metrics-classic metrics-numeric metrics-optout \
  --ignore-not-found --grace-period=1 >/dev/null
# the prometheus-operator CRDs, as a cluster without the operator gets them
# (operator-to-annotations/README.md); too large for a client-side apply
$KUBECTL apply --server-side -k operator-to-annotations/crds >/dev/null
# crds_established crd/<name>...: a CRD created a moment ago may have no
# .status.conditions yet, which kubectl wait fails on instead of waiting
crds_established() {
  for _ in $(seq 1 15); do $KUBECTL wait --for condition=established --timeout=5s "$@" >/dev/null 2>&1 && return 0; sleep 1; done
  $KUBECTL wait --for condition=established --timeout=60s "$@" >/dev/null
}
# shellcheck disable=SC2046 # one crd/<name> per word
crds_established $(sed -n 's/^  name: \(.*\.monitoring\.coreos\.com\)$/crd\/\1/p' operator-to-annotations/crds/*/*.yaml)
$KUBECTL apply -k "$MANIFESTS"
$KUBECTL rollout status statefulset/versitygw --timeout=180s
$KUBECTL wait --for=condition=complete job/create-bucket --timeout=180s
# The image tag is fixed, so apply does not replace a running agent. Restart
# it onto the image just loaded, and wait: the rolling update removes the
# old pod first, so the previous build's last upload lands before the purge
# below, not after it (a reused cluster otherwise keeps one object of the
# old line format in the assertions' corpus).
$KUBECTL -n observability rollout restart daemonset/node-agent deployment/k8sagent >/dev/null
$KUBECTL -n observability rollout status daemonset/node-agent --timeout=180s >/dev/null
$KUBECTL -n observability rollout status deployment/k8sagent --timeout=180s >/dev/null

# operator-to-annotations: cert-manager for the webhook's certificate, the
# controller, then pods that only a PodMonitor asks to scrape
# (operator-to-annotations/README.md)
phase "cluster: operator-to-annotations"
# (recent y-cluster releases install it with the cluster: then that one)
CERT_MANAGER=v1.21.2
CERT_MANAGER_SHA256=e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f
CERT_MANAGER_YAML=target/itest/cert-manager-$CERT_MANAGER.yaml
if ! $KUBECTL get crd certificates.cert-manager.io >/dev/null 2>&1; then
  if [ ! -f "$CERT_MANAGER_YAML" ]; then
    mkdir -p target/itest
    curl -sSfLo "$CERT_MANAGER_YAML.tmp" "https://github.com/cert-manager/cert-manager/releases/download/$CERT_MANAGER/cert-manager.yaml"
    mv "$CERT_MANAGER_YAML.tmp" "$CERT_MANAGER_YAML"
  fi
  echo "$CERT_MANAGER_SHA256  $CERT_MANAGER_YAML" | sha256sum -c --quiet || fail "$CERT_MANAGER_YAML is not cert-manager $CERT_MANAGER"
  $KUBECTL apply --server-side -f "$CERT_MANAGER_YAML" >/dev/null
fi
for d in cert-manager cert-manager-cainjector cert-manager-webhook; do
  $KUBECTL -n cert-manager rollout status "deployment/$d" --timeout=300s >/dev/null
done
# cert-manager's own webhook refuses Certificates until its CA is injected
for _ in $(seq 1 30); do $KUBECTL apply -k itest/operator-to-annotations >/dev/null 2>&1 && break; sleep 2; done
$KUBECTL apply -k itest/operator-to-annotations >/dev/null
$KUBECTL -n observability rollout restart deployment/operator-to-annotations >/dev/null
$KUBECTL -n observability rollout status deployment/operator-to-annotations --timeout=180s
$KUBECTL apply -k itest/operator-to-annotations/workloads >/dev/null
# The webhook answers once cert-manager has injected its CA and the
# Service has the new pod: until then pods are admitted unannotated
# (failurePolicy Ignore). A server-side dry run that the PodMonitor
# selects comes back annotated when it does.
webhook_probe() {
  $KUBECTL run webhook-probe --dry-run=server --restart=Never --image=busybox:1.37 --labels=app=podmonitored \
    --overrides='{"apiVersion": "v1", "spec": {"containers": [{"name": "probe", "image": "busybox:1.37", "ports": [{"name": "metrics", "containerPort": 9201}]}]}}' \
    -o go-template='{{ with .metadata.annotations }}{{ with index . "observability-to-blobs/podmonitors" }}{{ . }}{{ end }}{{ end }}' 2>/dev/null
}
for _ in $(seq 1 60); do [ "$(webhook_probe)" = default/podmonitored ] && break; sleep 2; done
[ "$(webhook_probe)" = default/podmonitored ] || fail "the operator-to-annotations webhook does not annotate a pod its PodMonitor selects"
# the pods start over, admitted by the webhook as it is now
$KUBECTL delete pod podmonitored-preannotated --ignore-not-found --grace-period=1 >/dev/null
$KUBECTL apply -k itest/operator-to-annotations/workloads >/dev/null
$KUBECTL rollout restart deployment/podmonitored >/dev/null
$KUBECTL rollout status deployment/podmonitored --timeout=120s

# Objects from earlier runs would make every assertion scan an unbounded
# corpus. The bucket is test state; start empty. In up mode the bucket is
# the user's data: leave it alone.
if [ "$MODE" != up ]; then
phase "cluster: purge bucket prefix from earlier runs"
# the compactors write hourly/ and daily/: stop them first, their pods gone
$KUBECTL -n observability delete deployment blobs-compactor --ignore-not-found --cascade=foreground >/dev/null
$KUBECTL -n "$PUT_NS" delete deployment blobs-compactor --ignore-not-found --cascade=foreground >/dev/null
$KUBECTL delete pod purge-bucket --ignore-not-found >/dev/null 2>&1
# (the trailing slash: raw1/itest is a prefix of raw1/itest-put too)
$KUBECTL run purge-bucket --restart=Never --image=amazon/aws-cli:2.22.35 \
  --env=AWS_ACCESS_KEY_ID=demoaccess --env=AWS_SECRET_ACCESS_KEY=demosecret \
  --env=AWS_DEFAULT_REGION=us-east-1 \
  --command -- sh -ec "for c in $CLUSTER $PUT_CLUSTER; do for p in raw1 hourly daily; do aws --endpoint-url http://versitygw:7070 s3 rm --recursive --quiet s3://$BUCKET/\$p/\$c/; done; done" >/dev/null
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
  fail "could not purge s3://$BUCKET/{raw1,hourly,daily}/{$CLUSTER,$PUT_CLUSTER}/ (pod/purge-bucket $purge, logs above)"
fi
$KUBECTL delete pod purge-bucket >/dev/null
# the put compactor's spool outlives its pods: it starts empty too
if $KUBECTL -n "$PUT_NS" get pvc blobs-compactor-spool >/dev/null 2>&1; then
  spool_sh "rm -rf /spool/raw1 /spool/_tmp" || fail "could not empty the spool: $POD_LOG"
fi

# Startup output (the burst, the >16 KiB line, restart counting) must land
# after the purge, so the pods that produce it start over. On a reused
# cluster the crash-looper's backoff has also grown to minutes.
$KUBECTL delete pod restarter reused --ignore-not-found >/dev/null
$KUBECTL rollout restart deployment/log-generator >/dev/null
# the cluster agent's first snapshots, the base of every state, too
$KUBECTL -n observability rollout restart deployment/k8sagent >/dev/null
$KUBECTL apply -k "$MANIFESTS" >/dev/null
$KUBECTL rollout status deployment/log-generator --timeout=120s >/dev/null
fi
$KUBECTL -n observability rollout status daemonset/node-agent --timeout=180s
$KUBECTL -n observability rollout status deployment/k8sagent --timeout=180s
$KUBECTL rollout status deployment/log-generator --timeout=120s
$KUBECTL wait pod/metrics-multi pod/metrics-numeric pod/metrics-optout --for=condition=Ready --timeout=120s

# The compactor folds the bucket from here on, writing encrypted Parquet
# with the keys of $PARQUET_KEYS (compactor/README.md, "Encryption"):
# itest-1 at first, itest-2 after the rotation below. Its tag is fixed
# too, so a restart moves it onto the image just loaded.
# A key's content never changes under its id: a key missing here is taken
# from the cluster's Secret when it has one, and only minted when neither
# has it, so a cleaned target/ against a kept bucket keeps its keys.
(umask 077; mkdir -p "$PARQUET_KEYS")
for id in itest-1 itest-2; do
  [ -s "$PARQUET_KEYS/$id" ] && continue
  existing=$($KUBECTL -n observability get secret blobs-compactor-parquet-keys \
    -o go-template="{{ with index .data \"$id\" }}{{ . }}{{ end }}" 2>/dev/null || true)
  if [ -n "$existing" ]; then
    (umask 077; base64 -d <<< "$existing" > "$PARQUET_KEYS/$id")
  else
    (umask 077; head -c 32 /dev/urandom | base64 > "$PARQUET_KEYS/$id")
  fi
done
$KUBECTL -n observability create secret generic blobs-compactor-parquet-keys --from-file="$PARQUET_KEYS" \
  --dry-run=client -o yaml | $KUBECTL apply -f - >/dev/null
$KUBECTL apply -k itest/compactor
$KUBECTL -n observability rollout restart deployment/blobs-compactor >/dev/null
$KUBECTL -n observability rollout status deployment/blobs-compactor --timeout=120s
# The second compactor, in spool mode, and stock Fluent Bit shipping to
# its put endpoint. A new shipper pod reads every pod log from its start.
$KUBECTL apply -k itest/compactor-put
$KUBECTL -n "$PUT_NS" rollout restart deployment/blobs-compactor daemonset/stock-shipper >/dev/null
$KUBECTL -n "$PUT_NS" rollout status deployment/blobs-compactor --timeout=180s
$KUBECTL -n "$PUT_NS" rollout status daemonset/stock-shipper --timeout=180s

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

In namespace $PUT_NS, stock Fluent Bit uploads the same container logs
as cluster $PUT_CLUSTER to a second compactor's put endpoint, which spools
them on its volume and folds them into hourly/$PUT_CLUSTER/ (plain):

  kubectl --context=$CONTEXT -n $PUT_NS logs deploy/blobs-compactor -f

The compactor folds the raw objects into hourly/$CLUSTER/ every 20 s,
encrypted with the keys in $PARQUET_KEYS (local/blobs.sh adds views
over that tier once it exists, given the keys):

  kubectl --context=$CONTEXT -n observability logs deploy/blobs-compactor -f
  PARQUET_KEYS_DIR=$PARQUET_KEYS local/blobs.sh -c "SELECT count(*) FROM hourly_logs"

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
# the logs API reads the days before this one from the daily tier
RUN_DATE=$(date -u +%F)
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

# The cluster agent (K8SAGENT.md): an Event about the node, which lives in
# namespace default as every cluster-scoped object's events do, and a
# ConfigMap created, changed and deleted, at times kept for apiserver-at.
phase "scenario: cluster objects: an Event about the node, a ConfigMap's three states"
utc_now() { duckdb -noheader -csv -c "SELECT strftime(now() AT TIME ZONE 'UTC', '%Y-%m-%d %H:%M:%S.%f')"; }
NODE=$($KUBECTL get nodes -o jsonpath='{.items[0].metadata.name}')
NODE_EVENT="k8sagent-node-event-$(date +%s)"
$KUBECTL create -f - >/dev/null <<YAML
apiVersion: v1
kind: Event
metadata: {name: $NODE_EVENT, namespace: default}
involvedObject: {apiVersion: v1, kind: Node, name: $NODE, uid: "$($KUBECTL get node "$NODE" -o jsonpath='{.metadata.uid}')"}
reason: ItestNodeEvent
message: an event about the node, from test.sh
type: Normal
source: {component: test.sh}
YAML
# A CRD of two versions with large schemas (crd_mode redacted ships
# neither) and a custom resource of it, which apiserver-at must still list
# with the CRD's printer columns
BIG_DESCRIPTION=$(head -c 150000 /dev/zero | tr '\0' x)
gadget_version() { # name storage
  cat <<YAML
  - name: $1
    served: true
    storage: $2
    additionalPrinterColumns:
    - {name: Size, type: integer, jsonPath: .spec.size}
    - {name: Color, type: string, jsonPath: .spec.color, priority: 1}
    - {name: Age, type: date, jsonPath: .metadata.creationTimestamp}
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            description: "$1 $BIG_DESCRIPTION"
            properties:
              size: {type: integer}
              color: {type: string}
YAML
}
$KUBECTL delete crd gadgets.itest.example.com --ignore-not-found >/dev/null
{ cat <<YAML
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: gadgets.itest.example.com}
spec:
  group: itest.example.com
  scope: Namespaced
  names: {plural: gadgets, singular: gadget, kind: Gadget}
  versions:
YAML
  gadget_version v1alpha1 false
  gadget_version v1 true
} | $KUBECTL create -f - >/dev/null
crds_established crd/gadgets.itest.example.com
$KUBECTL apply -f - >/dev/null <<YAML
apiVersion: itest.example.com/v1
kind: Gadget
metadata: {name: g1, namespace: default}
spec: {size: 3, color: blue}
YAML
$KUBECTL delete configmap k8sagent-probe --ignore-not-found >/dev/null
PROBE_0=$(utc_now)
sleep 3
$KUBECTL create configmap k8sagent-probe --from-literal=v=1 >/dev/null
sleep 5
PROBE_1=$(utc_now)
sleep 5
$KUBECTL patch configmap k8sagent-probe -p '{"data":{"v":"2"}}' >/dev/null
sleep 5
PROBE_2=$(utc_now)
sleep 5
$KUBECTL delete configmap k8sagent-probe >/dev/null
sleep 5
PROBE_3=$(utc_now)
echo "  probe states at $PROBE_0 (none), $PROBE_1 (v=1), $PROBE_2 (v=2), $PROBE_3 (deleted)"

# The put endpoint with a client other than out_s3, aws-cli: a raw object
# for an hour three hours ago, which the past pass folds in full, so the
# hour is final and the object goes from the spool; the same PUT with a
# wrong secret, and without a signature, both refused.
phase "scenario: PUTs to the put endpoint: a late object, a wrong signature, none"
LATE_PUT_HOUR=$(duckdb -noheader -csv -c "SELECT strftime(date_trunc('hour', now() AT TIME ZONE 'UTC') - INTERVAL 3 HOUR, '%Y-%m-%d/%H')")
LATE_PUT_UID=$(duckdb -noheader -csv -c "SELECT uuid()")
LATE_PUT_KEY=raw1/$PUT_CLUSTER/$LATE_PUT_HOUR/itest-late/0000-$LATE_PUT_UID.jsonl.zst
late=$(mktemp "${TMPDIR:-/tmp}/itest-late.XXXXXX")
duckdb -c "COPY (
  SELECT 'logs' AS \"table\", t AS time, t AS ts, '$PUT_CLUSTER' AS cluster, 'itest-late' AS node, 'itest-late' AS namespace,
         'late' AS pod, '$LATE_PUT_UID' AS pod_uid, 'late' AS container, 0 AS restart, 'stdout' AS stream, 'late put line ' || i AS message
  FROM (SELECT i, strptime('$LATE_PUT_HOUR', '%Y-%m-%d/%H')::TIMESTAMP_NS + to_seconds(i) AS t FROM range(3) r(i))
) TO '$late' (FORMAT json, COMPRESSION zstd)" >/dev/null || fail "the late object"
late_b64=$(base64 < "$late" | tr -d '\n')
rm -f "$late"
put="aws --endpoint-url http://blobs-compactor-put:9000 s3api put-object --bucket $BUCKET --body /tmp/late.jsonl.zst --key"
run_pod "$PUT_NS" put-probe amazon/aws-cli:2.22.35 "" sh -c "
  export AWS_ACCESS_KEY_ID=itestput AWS_SECRET_ACCESS_KEY=itest-put-secret AWS_DEFAULT_REGION=us-east-1
  echo $late_b64 | base64 -d > /tmp/late.jsonl.zst
  $put $LATE_PUT_KEY >/dev/null && echo late-stored
  AWS_SECRET_ACCESS_KEY=wrong $put raw1/$PUT_CLUSTER/$LATE_PUT_HOUR/itest-late/0001-wrong.jsonl.zst 2>&1; echo wrong-exit \$?
  $put raw1/$PUT_CLUSTER/$LATE_PUT_HOUR/itest-late/0002-unsigned.jsonl.zst --no-sign-request 2>&1; echo unsigned-exit \$?" || true
PUT_PROBE=$POD_LOG
while IFS= read -r l; do echo "  $l"; done <<<"$PUT_PROBE"

# Fluent Bit buffers while the put endpoint is gone and uploads once it is
# back, so a line written while the compactor is down arrives, once.
phase "scenario: the put compactor down for 60 s, a marker in flight"
$KUBECTL -n "$PUT_NS" scale deployment/blobs-compactor --replicas=0 >/dev/null
deadline=$((SECONDS + 330))
while [ -n "$($KUBECTL -n "$PUT_NS" get pods -l app=blobs-compactor -o name)" ]; do
  [ $SECONDS -lt $deadline ] || fail "the put compactor's pod is still there 330s after scaling to 0"
  sleep 2
done
# the outage window, for the shipper's failed uploads (kubelet log
# timestamps are UTC RFC 3339)
PUT_DOWN_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
PUT_MARKER="put-marker-$(date +%s)"
$KUBECTL exec deploy/log-generator -- sh -c "echo '$PUT_MARKER' > /proc/1/fd/1"
sleep 60
$KUBECTL -n "$PUT_NS" scale deployment/blobs-compactor --replicas=1 >/dev/null
$KUBECTL -n "$PUT_NS" rollout status deployment/blobs-compactor --timeout=180s >/dev/null
PUT_UP_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "  the put compactor is back"

# ---------------------------------------------------------------- assert --

# One DuckDB init for every assertion: the secret, and the views the
# compactor and local/blobs.sh read from the same .sql files, so the
# column contract lives in exactly one place (compactor/sql/raw.sql).
umask 077  # the S3 secret and the parquet keys go into files below
INIT=$(mktemp "${TMPDIR:-/tmp}/itest.XXXXXX")
INIT_S3=$INIT.s3 SCOPED=$INIT.scoped SNAPSHOT=$INIT.raw.csv PODS_BEFORE=$INIT.pods.parquet ROTATED=$INIT.rotated.csv
LOGS_HDR=$INIT.logs.headers LOGS_OUT=$INIT.logs.body
cat >"$INIT_S3" <<SQL
.output /dev/null
INSTALL httpfs; LOAD httpfs; SET TimeZone='UTC';
SET s3_region='us-east-1'; SET s3_endpoint='127.0.0.1:$S3_PORT';
SET s3_access_key_id='demoaccess'; SET s3_secret_access_key='demosecret';
SET s3_use_ssl=false; SET s3_url_style='path';
SQL
# the compactor's parquet keys; 0600 like the S3 one (umask above)
for f in "$PARQUET_KEYS"/*; do
  echo "PRAGMA add_parquet_key('${f##*/}', '$(tr -d '[:space:]' < "$f")');"
done >>"$INIT_S3"
echo ".output" >>"$INIT_S3"
cat >"$INIT" <<SQL
.read $INIT_S3
.output /dev/null
SET VARIABLE raw_files = 's3://$BUCKET/raw1/$CLUSTER/*/*/*/*.jsonl.zst';
.read $PWD/compactor/sql/raw.sql
.read $PWD/compactor/sql/tables.sql
.read $PWD/local/apiserver_at.sql
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

# tier <stem glob> [read_parquet options]: a FROM clause over the tier
# objects <stem>.parquet and <stem>.k-<id>.parquet: one read of the plain
# objects and one per key id, united by name, because one read takes one
# key (compactor/README.md, "Encryption"). A read binds its glob, so only
# the reads that match objects are in it. No '?' in a stem: in an s3 URL
# it starts the query string.
tier() {
  local stem=$1 opts=${2:+, $2} id from=""
  for id in $(q "SELECT DISTINCT coalesce(nullif(regexp_extract(file, '\\.k-([a-z0-9-]+)\\.parquet\$', 1), ''), '-') FROM glob('$stem*.parquet') ORDER BY 1"); do
    if [ "$id" = - ]; then
      from="${from:+$from UNION ALL BY NAME }SELECT * FROM read_parquet('$stem.parquet', union_by_name = true$opts)"
    else
      from="${from:+$from UNION ALL BY NAME }SELECT * FROM read_parquet('$stem.k-$id.parquet', encryption_config = {footer_key: '$id'}$opts)"
    fi
  done
  echo "(${from:-SELECT NULL AS none WHERE false})"
}

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

# expect <description> <command...>: PASS when the command succeeds, as
# assert does for SQL
expect() {
  local what=$1
  shift
  if "$@"; then
    echo "  PASS: $what"
  else
    echo "  FAIL: $what" >&2
    ERRORS=$((ERRORS + 1))
  fi
}

# logs_expect <description> <command...>: expect, showing the logs API's
# last answer on a failure
logs_expect() {
  local what=$1
  shift
  if "$@"; then
    echo "  PASS: $what"
  else
    echo "  FAIL: $what, the answer:" >&2
    logs_show
    ERRORS=$((ERRORS + 1))
  fi
}

# The logs API (compactor/README.md, "The logs API"), through a
# port-forward from this host.
# logs_forward <namespace> <resource> <local port> <its variable>: forward
# to port 8080 of svc/... or pod/..., until the script exits, and wait for
# the API
logs_forward() {
  local deadline=$((SECONDS + 60))
  port_free "$3" "$4"
  $KUBECTL -n "$1" port-forward "$2" "$3:8080" >/dev/null 2>&1 &
  LOGS_PF_PIDS+=($!)
  until curl -sf -o /dev/null "http://127.0.0.1:$3/openapi.json"; do
    [ $SECONDS -lt $deadline ] || return 1
    sleep 1
  done
}
# logs_get <local port> <path>: the answer's status in LOGS_CODE, its
# headers in LOGS_HEADERS, its body in LOGS_BODY (trailing newlines cut)
logs_get() {
  LOGS_CODE=$(curl -sS -o "$LOGS_OUT" -D "$LOGS_HDR" -w '%{http_code}' "http://127.0.0.1:$1$2" 2>&1) || LOGS_CODE="curl failed: $LOGS_CODE"
  LOGS_HEADERS=$(tr -d '\r' < "$LOGS_HDR")
  LOGS_BODY=$(cat "$LOGS_OUT")
}
# logs_header <name>: that header of the last answer
logs_header() {
  awk -v k="$(tr '[:upper:]' '[:lower:]' <<<"$1")" -F': ' 'tolower($1) == k { v = $2 } END { print v }' <<<"$LOGS_HEADERS"
}
# logs_is <status> <content type> [header=value...]: the last answer has them
logs_is() {
  local kv
  [ "$LOGS_CODE" = "$1" ] && [ "$(logs_header Content-Type)" = "$2" ] || return 1
  shift 2
  for kv in "$@"; do
    [ "$(logs_header "${kv%%=*}")" = "${kv#*=}" ] || return 1
  done
}
# logs_show: the last answer, for a failure's context
logs_show() { printf '    %s\n' "status $LOGS_CODE" "$LOGS_HEADERS" "${LOGS_BODY:0:600}" >&2; }
# logs_tail_lines <n>: the body is n lines, each a CRI timestamp, a space
# and a message, and X-Logs-Timestamp-End is the last line's timestamp
logs_tail_lines() {
  local n
  n=$(grep -c . <<<"$LOGS_BODY")
  [ "$n" = "$1" ] && ! grep -qvE '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{9}Z ' <<<"$LOGS_BODY" \
    && [ "$(tail -n 1 <<<"$LOGS_BODY" | cut -d' ' -f1)" = "$(logs_header X-Logs-Timestamp-End)" ]
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
# the node agents' objects; the cluster agent's are under _k8sagent
NODE_AGENT="filename NOT LIKE '%/_k8sagent/%'"
assert "about two objects per minute per node (upload_timeout 30s)" \
  "SELECT bool_and(coalesce(n, 0) BETWEEN 1 AND 3) FROM $MINUTES LEFT JOIN (SELECT date_trunc('minute', $KEYTIME) AS m, count(DISTINCT filename) AS n FROM raw WHERE $KEYTIME > $SINCE + INTERVAL 3 MINUTE AND $KEYTIME < $NOW - INTERVAL 2 MINUTE AND $NODE_AGENT GROUP BY m) USING (m)"
assert "every node agent object after the outage carries logs and metrics lines" \
  "SELECT bool_and(has_logs AND has_metrics) FROM (SELECT filename, bool_or(\"table\" = 'logs') AS has_logs, bool_or(\"table\" = 'metrics') AS has_metrics FROM raw WHERE $KEYTIME > $SINCE + INTERVAL 3 MINUTE AND $NODE_AGENT GROUP BY filename)"
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
assert "cluster is on every line, node on every line but the cluster agent's" \
  "SELECT bool_and(cluster = '$CLUSTER' AND (\"table\" = 'apiserver' OR (node IS NOT NULL AND node <> ''))) FROM raw"
assert "no line is of an unknown table" \
  "SELECT bool_and(\"table\" IN ('logs', 'metrics', 'podindex', 'apiserver')) FROM raw"

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
  "SELECT bool_and(length(pod_uid) = 36) FROM metrics WHERE scrape = 'pod'"
assert "node_exporter metrics arrive via the static target with the agent pod's identity" \
  "SELECT count(*) > 0 FROM metrics WHERE scrape = 'node' AND name = 'node_cpu_seconds_total' AND namespace = 'observability' AND pod LIKE 'node-agent-%' AND container = 'agent' AND length(pod_uid) = 36"
assert "agent self metrics arrive via the static target" \
  "SELECT count(*) > 0 FROM metrics WHERE scrape = 'node' AND name LIKE 'fluentbit_%' AND pod LIKE 'node-agent-%'"
assert "no metrics line has job: scrape names the scrape path" \
  "SELECT count(*) = 0 FROM raw WHERE \"table\" = 'metrics' AND job IS NOT NULL"
assert "a Deployment's pod's samples carry its Deployment and ReplicaSet (README.md, \"Workload names\")" \
  "SELECT count(*) > 0 AND bool_and(k8s_deployment_name = 'podmonitored' AND k8s_replicaset_name LIKE 'podmonitored-%'
     AND k8s_daemonset_name IS NULL AND k8s_job_name IS NULL) FROM metrics WHERE pod LIKE 'podmonitored-%' AND pod <> 'podmonitored-preannotated'"
assert "a bare pod's samples and the static targets' carry no workload" \
  "SELECT count(*) > 0 AND bool_and(coalesce(k8s_deployment_name, k8s_replicaset_name, k8s_statefulset_name, k8s_daemonset_name,
     k8s_job_name, k8s_cronjob_name) IS NULL) FROM metrics WHERE pod = 'metrics-multi' OR scrape = 'node'"
assert "healthy targets are scraped about every 15s" \
  "SELECT bool_and(coalesce(n, 0) BETWEEN 3 AND 5) FROM $MINUTES LEFT JOIN (SELECT date_trunc('minute', time) AS m, count(*) AS n FROM metrics WHERE pod = 'metrics-multi' AND container = 'app' AND name = 'up' AND time > $SINCE + INTERVAL 3 MINUTE AND time < $NOW - INTERVAL 2 MINUTE GROUP BY m) USING (m)"

phase "assert: prometheus-operator CRDs (operator-to-annotations/crds)"
# a manifest at v1 applies; any other version, or a kind the base leaves
# out, fails at apply instead of being stored and read by nothing
dry_run() { # <apiVersion> <kind> <spec>
  $KUBECTL apply --dry-run=server -f - >/dev/null 2>&1 <<YAML
apiVersion: $1
kind: $2
metadata: {name: itest-dry-run, namespace: default}
spec: $3
YAML
}
expect "a v1 PodMonitor applies" \
  dry_run monitoring.coreos.com/v1 PodMonitor '{selector: {matchLabels: {app: x}}, podMetricsEndpoints: [{port: metrics}]}'
expect "a v1 ServiceMonitor applies" \
  dry_run monitoring.coreos.com/v1 ServiceMonitor '{selector: {matchLabels: {app: x}}, endpoints: [{port: metrics}]}'
expect "a v1 PrometheusRule applies" \
  dry_run monitoring.coreos.com/v1 PrometheusRule '{groups: [{name: g, rules: [{alert: A, expr: vector(1)}]}]}'
expect "a v1 Prometheus and a v1 Alertmanager apply" \
  eval "dry_run monitoring.coreos.com/v1 Prometheus '{}' && dry_run monitoring.coreos.com/v1 Alertmanager '{}'"
expect "a v1alpha1 PodMonitor fails at apply" \
  eval "! dry_run monitoring.coreos.com/v1alpha1 PodMonitor '{}'"
expect "a ScrapeConfig, which the base leaves out, fails at apply" \
  eval "! dry_run monitoring.coreos.com/v1alpha1 ScrapeConfig '{}'"

# -------------------------------------------------------------- compactor --

phase "assert: operator-to-annotations"
# the newest pod of the Deployment, and its annotation <key>
PODMONITORED=$($KUBECTL get pod -l app=podmonitored,pod-template-hash --sort-by=.metadata.creationTimestamp -o name | tail -n 1)
annotation() { # <pod> <key>
  $KUBECTL get "$1" -o go-template="{{ with .metadata.annotations }}{{ with index . \"$2\" }}{{ . }}{{ end }}{{ end }}"
}
annotation_is() { [ "$(annotation "$1" "$2")" = "$3" ]; }
OTA_LOGS=$($KUBECTL -n observability logs deployment/operator-to-annotations)
logged() { grep -F "$1" <<< "$OTA_LOGS" | grep -qF "$2"; }
ota_ports() {
  annotation_is "$PODMONITORED" io.opentelemetry.discovery.metrics.9201/enabled true &&
    annotation_is "$PODMONITORED" io.opentelemetry.discovery.metrics.9202/enabled true
}
ota_config() {
  annotation_is "$PODMONITORED" io.opentelemetry.discovery.metrics.9201/config "collection_interval: 20s
metric_relabel_configs:
- action: drop
  regex: podmonitored_main_dropped
  source_labels:
  - __name__
- action: labeldrop
  regex: worker" &&
    annotation_is "$PODMONITORED" io.opentelemetry.discovery.metrics.9202/config "metrics_path: /metrics"
}
ota_marker_only() {
  annotation_is "$PODMONITORED" io.opentelemetry.discovery.metrics/enabled "" &&
    annotation_is "$PODMONITORED" observability-to-blobs/podmonitors default/podmonitored
}
ota_preannotated() {
  annotation_is pod/podmonitored-preannotated observability-to-blobs/podmonitors "" &&
    annotation_is pod/podmonitored-preannotated io.opentelemetry.discovery.metrics.9201/enabled "" &&
    annotation_is pod/podmonitored-preannotated prometheus.io/scrape false
}
ota_report() {
  local pm='"podMonitor":"default/podmonitored"'
  logged "$pm" '"condition":"PartiallyTranslated"' && logged "$pm" 'podMetricsEndpoints[1].scrapeTimeout' &&
    ! logged "$pm" 'metricRelabelings' &&
    logged "$pm" '"preannotated":[{"pod":"default/podmonitored-preannotated"'
}
expect "the webhook enables each endpoint's port by number: the main container's 9201, the worker's 9202" ota_ports
expect "each port's config carries its endpoint's interval, path and metricRelabelings" ota_config
expect "no pod-level key, and the marker names the PodMonitor" ota_marker_only
expect "a selected pod with its own scrape annotations is left as it is" ota_preannotated
ota_preannotated_info() {
  grep -F "not applying PodMonitor default/podmonitored" <<< "$OTA_LOGS" | grep -F default/podmonitored-preannotated | grep -qF '"level":"INFO"'
}
expect "the webhook skips it at level INFO, naming the pod and the PodMonitor" ota_preannotated_info
expect "the report: partially translated, scrapeTimeout untranslated, metricRelabelings translated, the pre-annotated pod preannotated" ota_report
expect "a ServiceMonitor is logged as not supported" \
  logged "ServiceMonitor is not supported" default/podmonitored
assert "the main container's metrics, attributed to it" \
  "SELECT count(*) > 0 FROM metrics WHERE pod LIKE 'podmonitored-%' AND container = 'main' AND name = 'podmonitored_main_up' AND value = 1"
assert "the worker's metrics, attributed to it" \
  "SELECT count(*) > 0 FROM metrics WHERE pod LIKE 'podmonitored-%' AND container = 'worker' AND name = 'podmonitored_worker_up' AND value = 1"
assert "neither container's series in the other's" \
  "SELECT count(*) = 0 FROM metrics WHERE pod LIKE 'podmonitored-%' AND ((container = 'main' AND name = 'podmonitored_worker_up') OR (container = 'worker' AND name = 'podmonitored_main_up'))"
# the newest pod's: on a reused cluster a replica annotated by the previous
# run's webhook can still ship for a moment after the purge
PODMONITORED_POD=${PODMONITORED#pod/}
assert "main's metricRelabelings: the dropped series is not shipped" \
  "SELECT count(*) = 0 FROM metrics WHERE pod = '$PODMONITORED_POD' AND name = 'podmonitored_main_dropped'"
assert "main's metricRelabelings: worker is dropped, and of the two series that leaves equal, the first is kept" \
  "SELECT count(*) > 0 AND bool_and(value = 1 AND labels = MAP {'code': '200'}) FROM metrics
   WHERE pod = '$PODMONITORED_POD' AND container = 'main' AND name = 'podmonitored_main_requests_total'"
assert "main's scrapes count 4 samples scraped, 2 after relabeling; the worker's 1 and 1" \
  "SELECT bool_and(CASE container WHEN 'main' THEN s = 4 AND p = 2 WHEN 'worker' THEN s = 1 AND p = 1 END) AND count(DISTINCT container) = 2
   FROM (SELECT container, time, max(value) FILTER (WHERE name = 'scrape_samples_scraped') AS s,
                max(value) FILTER (WHERE name = 'scrape_samples_post_metric_relabeling') AS p
         FROM metrics WHERE pod = '$PODMONITORED_POD' AND name LIKE 'scrape_samples_%' GROUP BY container, time)"
assert "main is scraped every 20 s (its endpoint's interval), the worker every 15 s (the default)" \
  "WITH g AS (SELECT container, epoch(time) - epoch(lag(time) OVER (PARTITION BY pod, container ORDER BY time)) AS gap
              FROM metrics WHERE pod LIKE 'podmonitored-%' AND name = 'up')
   SELECT bool_and(CASE container WHEN 'main' THEN m BETWEEN 19 AND 21 WHEN 'worker' THEN m BETWEEN 14 AND 16 END) AND count(*) = 2
   FROM (SELECT container, median(gap) AS m FROM g WHERE gap IS NOT NULL GROUP BY container)"

phase "assert: cluster objects (K8SAGENT.md)"
assert "the cluster agent's keys hold apiserver lines, and only they do" \
  "SELECT bool_and((\"table\" = 'apiserver') = (filename LIKE '%/_k8sagent/%')) FROM raw"
assert "built-in and custom resources are snapshotted, k3s's Addons included" \
  "SELECT count(DISTINCT api_version || ' ' || resource) = 15 FROM apiserver WHERE kind = 'snapshot' AND (api_version, resource) IN (
     ('v1', 'pods'), ('v1', 'events'), ('v1', 'configmaps'), ('v1', 'secrets'), ('v1', 'nodes'), ('v1', 'namespaces'),
     ('v1', 'services'), ('apps/v1', 'deployments'), ('apps/v1', 'daemonsets'), ('apps/v1', 'statefulsets'), ('batch/v1', 'jobs'),
     ('apiextensions.k8s.io/v1', 'customresourcedefinitions'), ('rbac.authorization.k8s.io/v1', 'clusterroles'),
     ('storage.k8s.io/v1', 'storageclasses'), ('k3s.cattle.io/v1', 'addons'))"
assert "leases, events.k8s.io's copy of the events and the deprecated v1 endpoints are not watched" \
  "SELECT count(*) = 0 FROM apiserver WHERE resource = 'leases' OR api_version LIKE 'events.k8s.io/%' OR (api_version = 'v1' AND resource = 'endpoints')"
assert "EndpointSlices are watched, versitygw's among them" \
  "SELECT count(*) > 0 FROM apiserver WHERE api_version = 'discovery.k8s.io/v1' AND resource = 'endpointslices' AND kind = 'object-snapshot'
     AND json_extract_string(apijson, '\$.metadata.labels.\"kubernetes.io/service-name\"') = 'versitygw'"
assert "every snapshot row counts the object rows of its snapshot" \
  "WITH n AS (SELECT api_version, resource, time, count(DISTINCT coalesce(namespace, '') || '/' || name) AS c
              FROM apiserver WHERE kind = 'object-snapshot' GROUP BY ALL)
   SELECT count(*) > 0 AND bool_and(coalesce(n.c, 0) = s.rows) FROM apiserver s LEFT JOIN n USING (api_version, resource, time) WHERE s.kind = 'snapshot'"
assert "configmaps are snapshotted at least twice at multiples of SNAPSHOT_INTERVAL (2m)" \
  "SELECT count(DISTINCT time) >= 2 FROM apiserver WHERE kind = 'snapshot' AND api_version = 'v1' AND resource = 'configmaps'
     AND minute(time) % 2 = 0 AND second(time) < 5 AND time > $SINCE"
assert "CRDs ship without schemas (crd_mode redacted), marked" \
  "SELECT count(*) > 0 AND bool_and(json_extract_string(apijson, '\$.metadata.annotations.\"observability-to-blobs/redacted\"') = 'schemas'
     AND NOT contains(apijson, '\"properties\"')) FROM apiserver WHERE resource = 'customresourcedefinitions' AND apijson IS NOT NULL"
assert "the gadgets CRD (two versions, 300 KB of schemas) ships in under 4 KB, both versions and their printer columns kept" \
  "SELECT count(*) > 0 AND bool_and(length(apijson) < 4000 AND NOT contains(apijson, 'xxxxxxxxxx')
     AND json_array_length(apijson, '\$.spec.versions') = 2
     AND json_extract_string(apijson, '\$.spec.versions[1].additionalPrinterColumns[1].name') = 'Color')
   FROM apiserver WHERE resource = 'customresourcedefinitions' AND name = 'gadgets.itest.example.com' AND apijson IS NOT NULL"
assert "no object carries managedFields" \
  "SELECT count(*) > 0 AND bool_and(NOT contains(apijson, '\"managedFields\"')) FROM apiserver WHERE apijson IS NOT NULL"
# every value of every Secret row, as (key, value) structs
SECRET_VALUES="(SELECT namespace, name, unnest(map_entries(json_transform(json_extract(apijson, '\$.data'), '\"MAP(VARCHAR, VARCHAR)\"'))) AS e
  FROM apiserver WHERE api_version = 'v1' AND resource = 'secrets' AND apijson IS NOT NULL)"
assert "Secrets carry the redaction mark" \
  "SELECT count(*) > 0 AND bool_and(json_extract_string(apijson, '\$.metadata.annotations.\"observability-to-blobs/redacted\"') = 'data')
   FROM apiserver WHERE api_version = 'v1' AND resource = 'secrets' AND apijson IS NOT NULL"
assert "Secret values are k8sobs_ and random letters and digits, empty ones empty" \
  "SELECT count(*) > 0 AND bool_and(e.value = '' OR regexp_full_match(decode(from_base64(e.value)), 'k8sobs_[A-Za-z0-9]+')) FROM $SECRET_VALUES"
# demoaccess and demosecret, 10 bytes each
assert "a scrambled value has a tenth of the original's length, rounded up (node-agent-s3: k8sobs_ and one)" \
  "SELECT count(*) > 0 AND bool_and(length(decode(from_base64(e.value))) = 8) FROM $SECRET_VALUES
   WHERE namespace = 'observability' AND name LIKE 'node-agent-s3-%'"
# node-agent-s3, created by kubectl apply -k: its data and the applied
# manifest annotation both hold the base64 of the secret key
assert "no row holds the S3 secret's data, base64 or applied" \
  "SELECT count(*) = 0 FROM apiserver WHERE contains(apijson, '$(printf demosecret | base64)')"
assert "events of all namespaces, the node's own (namespace default) included" \
  "SELECT count(DISTINCT namespace) >= 2
      AND count(*) FILTER (WHERE name = '$NODE_EVENT' AND namespace = 'default' AND json_extract_string(apijson, '\$.involvedObject.kind') = 'Node') >= 1
   FROM apiserver WHERE api_version = 'v1' AND resource = 'events'"
assert "pod 'reused': three incarnations added, the first two deleted" \
  "SELECT count(DISTINCT object_uid) FILTER (WHERE kind = 'object-added') = 3 AND count(DISTINCT object_uid) FILTER (WHERE kind = 'object-deleted') >= 2
   FROM apiserver WHERE api_version = 'v1' AND resource = 'pods' AND namespace = 'default' AND name = 'reused' AND time > $SINCE"

# kubectl at a time, from the raw tier alone: apiserver-at, a stock
# kube-apiserver at the cluster's release over etcd holding apiserver_at(t)
phase "assert: kubectl at a time (y-obs apiserver-at)"
# apiserver-at at the cluster's own release: another release's API server
# prints other columns, and its RBAC bootstrap adds its release's system
# roles to the state (seen: 1.37 against a 1.35 cluster, 2026-10-07)
need jq "https://jqlang.github.io/jq/"
AT_K8S=$($KUBECTL version -o json | jq -r .serverVersion.gitVersion | sed -E 's/^v([0-9]+\.[0-9]+)\..*/\1.x/')
AT_ASSETS=$(GOFLAGS='' go run "sigs.k8s.io/controller-runtime/tools/setup-envtest@$SETUP_ENVTEST" use "$AT_K8S" -p path) \
  || fail "setup-envtest could not provide Kubernetes $AT_K8S"
echo "  apiserver-at runs Kubernetes $AT_K8S: $AT_ASSETS"
(cd tools/y-obs && go build -o ../../target/bin/y-obs .) || fail "build tools/y-obs"
# at_serve <UTC time>: apiserver-at serving the raw tier's objects as they
# were then, in the background, reached with at_kubectl
at_serve() {
  at_stop
  q "COPY (SELECT api_version, resource, namespace, name, apijson FROM apiserver_at(TIMESTAMP_NS '$1')) TO '$INIT.at.jsonl' (FORMAT json)" >/dev/null \
    || fail "apiserver_at($1)"
  rm -f "$INIT.at.kubeconfig"
  target/bin/y-obs apiserver-at -assets "$AT_ASSETS" -objects "$INIT.at.jsonl" -kubeconfig "$INIT.at.kubeconfig" -label "$1" >"$INIT.at.log" 2>&1 &
  AT_PID=$!
  local deadline=$((SECONDS + 120))
  until grep -q '^serving ' "$INIT.at.log"; do
    if ! kill -0 "$AT_PID" 2>/dev/null || [ $SECONDS -ge $deadline ]; then
      cat "$INIT.at.log" >&2
      fail "apiserver-at at $1 did not start"
    fi
    sleep 1
  done
  echo "  apiserver-at at $1: $(grep '^serving ' "$INIT.at.log")"
}
at_stop() {
  [ -n "$AT_PID" ] || return 0
  kill "$AT_PID" 2>/dev/null || true
  wait "$AT_PID" 2>/dev/null || true
  AT_PID=""
}
at_kubectl() { kubectl --kubeconfig "$INIT.at.kubeconfig" "$@"; }
# kubectl output without the ages it renders relative to now, and without
# the column padding that follows their width
# (and without the resourceVersion, apiserver-at's own, that describe of a
# custom resource prints)
timeless() { perl -pe 's/\b(\d+d)?(\d+h)?(\d+m)?\d+[smhd]\b/<age>/g; s/(Resource Version:\s+)\d+/$1<rv>/; s/[ \t]+/ /g; s/ $//'; }

probe_is() { [ "$(at_kubectl get configmap k8sagent-probe -o jsonpath='{.data.v}' 2>&1)" = "$1" ]; }
# (kubectl's output is captured first: it exits non-zero on these, which
# pipefail would make the pipeline's status)
probe_gone() { local out; out=$(at_kubectl get configmap k8sagent-probe 2>&1 || true); [[ $out == *NotFound* ]]; }
at_serve "$PROBE_0"; expect "at $PROBE_0, before its creation, there is no ConfigMap k8sagent-probe" probe_gone
at_serve "$PROBE_1"; expect "at $PROBE_1 ConfigMap k8sagent-probe has v=1" probe_is 1
refused() { local out; out=$(at_kubectl create namespace at-write 2>&1 || true); [[ $out == *MethodNotAllowed* ]]; }
expect "apiserver-at refuses a write" refused
at_serve "$PROBE_2"; expect "at $PROBE_2 ConfigMap k8sagent-probe has v=2" probe_is 2
at_serve "$PROBE_3"; expect "at $PROBE_3, after its deletion, there is no ConfigMap k8sagent-probe" probe_gone

# The present: what kubectl shows of the live cluster now, of the kinds
# that hold still for the length of a comparison, is what apiserver-at
# shows from the blobs at the time after it was read.
LIVE_ARGS=(
  "get namespaces"
  "get configmaps -A"
  "get secrets -A"
  "get services -A -o wide"
  "get deployments -A -o wide"
  "get daemonsets -A -o wide"
  "get statefulsets -A -o wide"
  "get customresourcedefinitions"
  "get clusterroles"
  "get storageclasses"
  "get nodes -o wide"
  "get addons.k3s.cattle.io -A"
  "get gadgets -A -o wide"
  "get gadgets.v1alpha1.itest.example.com -A -o wide"
  "describe gadget -n default g1"
  "get pods -l app=log-generator -o wide"
  "describe deployment log-generator"
  "describe service versitygw"
)
i=0
for a in "${LIVE_ARGS[@]}"; do
  read -ra args <<<"$a"
  $KUBECTL "${args[@]}" 2>&1 | timeless >"$INIT.live.$i"
  i=$((i + 1))
done
PRESENT=$(utc_now)
deadline=$((SECONDS + 120))
until [ "$(q "SELECT max(time) > TIMESTAMP_NS '$PRESENT' + INTERVAL 1 SECOND FROM apiserver" | tr -d '[:space:]')" = true ]; do
  [ $SECONDS -lt $deadline ] || fail "no cluster agent rows after $PRESENT in the bucket within 120s"
  sleep 5
done
at_serve "$PRESENT"
i=0
for a in "${LIVE_ARGS[@]}"; do
  read -ra args <<<"$a"
  at_kubectl "${args[@]}" 2>&1 | timeless >"$INIT.at.$i"
  if diff -u "$INIT.live.$i" "$INIT.at.$i" >"$INIT.diff"; then
    echo "  PASS: kubectl $a: the live cluster and apiserver-at at $PRESENT agree"
  else
    echo "  FAIL: kubectl $a: the live cluster (-) and apiserver-at at $PRESENT (+) differ:" >&2
    sed 's/^/    /' "$INIT.diff" >&2
    ERRORS=$((ERRORS + 1))
  fi
  i=$((i + 1))
done
at_stop

# y-obs's investigation commands against what the agents shipped: the
# bucket alone, through the binary the kubectl phase built. The window's
# hours are recent, so the planner reads raw; sql reads the encrypted
# hourly tier with the compactor's keys.
phase "assert: y-obs over the bucket (tools/y-obs)"
yobs() {
  env Y_OBS_CONTEXT= S3_ENDPOINT="127.0.0.1:$S3_PORT" S3_BUCKET="$BUCKET" S3_TLS=off \
    AWS_ACCESS_KEY_ID=demoaccess AWS_SECRET_ACCESS_KEY=demosecret CLUSTER_NAME="$CLUSTER" \
    target/bin/y-obs "$@" --since 3h --max-objects 2000 2>"$INIT.yobs.err"
}
yobs_expect() {
  local what=$1 want=$2 out
  shift 2
  if out=$(yobs "$@") && grep -qE -- "$want" <<<"$out"; then
    echo "  PASS: y-obs $what"
  else
    echo "  FAIL: y-obs $what: no line matches $want in:" >&2
    { echo "$out"; cat "$INIT.yobs.err"; } | sed 's/^/    /' | head -20 >&2
    ERRORS=$((ERRORS + 1))
  fi
}
GEN_POD=$(yobs pods -n default --name '^log-generator-' --phase running --format csv | sed -n '2p' | cut -d, -f2)
GEN_NODE=$(yobs pods -n default --name '^log-generator-' --phase running --format csv | sed -n '2p' | cut -d, -f4)
yobs_expect "pods: a running log-generator incarnation" ",running," pods -n default --name '^log-generator-' --format csv
yobs_expect "pods --at -1m: the log-generator alive a minute ago" ",running," pods -n default --name '^log-generator-' --at -1m --format csv
yobs_expect "grep --count: the restart marker exactly once" $'\t1\t' grep "$MARKER" -n default --count
yobs_expect "logs: the generator's lines, with timestamps" "Z [{].*hello from log-generator" logs default "${GEN_POD:-none}" app --grep 'hello from log-generator' --tail 3 --timestamps
if [ "$BACKEND" = docker ]; then
  echo "  SKIP: y-obs logs _node (k3s-in-docker has no journal)"
else
  yobs_expect "logs _node: the kernel marker in the node's journal" "$KMSG_MARKER" logs _node "${GEN_NODE:-none}" --grep "$KMSG_MARKER" --tail 0
fi
yobs_expect "exits: the restarter's exit code 1" "^[^,]*,default,restarter,[^,]*,[^,]*,[0-9]+,1," exits -n default --name '^restarter$' --format csv
yobs_expect "metrics latest: app_requests_total code 200 is 42" '"value":42' metrics latest app_requests_total --pod '^metrics-multi$' --where code=200 --format jsonl
yobs_status() {
  env Y_OBS_CONTEXT= S3_ENDPOINT="127.0.0.1:$S3_PORT" S3_BUCKET="$BUCKET" S3_TLS=off \
    AWS_ACCESS_KEY_ID=demoaccess AWS_SECRET_ACCESS_KEY=demosecret target/bin/y-obs status --cluster "$CLUSTER" 2>"$INIT.yobs.err"
}
if out=$(yobs_status) && grep -qE "^$CLUSTER	raw	[0-9-]+T[0-9]{2}	" <<<"$out" && grep -qE "^$CLUSTER	hourly	" <<<"$out"; then
  echo "  PASS: y-obs status: the cluster's raw and hourly tiers with their spans"
else
  echo "  FAIL: y-obs status: no raw and hourly rows for $CLUSTER in:" >&2
  { echo "$out"; cat "$INIT.yobs.err"; } | sed 's/^/    /' | head -20 >&2
  ERRORS=$((ERRORS + 1))
fi
yobs_expect "sql: the encrypted hourly tier with the parquet keys" "^true$" sql --parquet-keys-dir "$PARQUET_KEYS" --format csv "SELECT count(*) > 0 AS rows FROM hourly_logs"

# y-obs prometheus: the last two minutes loaded, older ranges through
# remote read, following, reuse and stop. The bucket was purged earlier in
# the run, so "older" is minutes, not hours. Needs the Prometheus server.
PROM_BIN=${PROMETHEUS:-$(command -v y-prometheus || command -v prometheus || true)}
if [ -z "$PROM_BIN" ]; then
  echo "  SKIP: y-obs prometheus (no PROMETHEUS, y-prometheus or prometheus)"
else
  YOBS_RUN=$(mktemp -d "${TMPDIR:-/tmp}/itest-yobs.XXXXXX")
  yobs_prom() {
    env Y_OBS_CONTEXT= XDG_RUNTIME_DIR="$YOBS_RUN" PROMETHEUS="$PROM_BIN" S3_ENDPOINT="127.0.0.1:$S3_PORT" S3_BUCKET="$BUCKET" S3_TLS=off \
      AWS_ACCESS_KEY_ID=demoaccess AWS_SECRET_ACCESS_KEY=demosecret CLUSTER_NAME="$CLUSTER" \
      target/bin/y-obs prometheus --no-open --max-objects 2000 "$@"
  }
  promq() { curl -s "$PROM_URL/api/v1/$1" --data-urlencode "query=$2" "${@:3}"; }
  # the loaded window's bounds, as y-obs reports them on its target
  prom_pushed() { promq query "yobs_pushed_window_seconds{bound=\"$1\"}" | grep -o '"[0-9]*"\]' | tr -dc 0-9 || true; }
  prom_names() { curl -s "$PROM_URL/api/v1/label/__name__/values" | grep -q '"app_requests_total"'; }
  prom_value() { promq query 'app_requests_total{pod="metrics-multi",container="app",code="200"}' | grep -q '"42"'; }
  # five minutes ending before the loaded window: only remote read has them
  prom_older() {
    local from
    from=$(prom_pushed from)
    [ -n "$from" ] && promq query_range 'count(node_load1)' --data-urlencode start=$((from - 300)) \
      --data-urlencode end=$((from - 30)) --data-urlencode step=30 | grep -q '"values":\[\['
  }
  prom_reused() { [ "$(yobs_prom 2>/dev/null)" = "$PROM_URL" ]; }
  prom_gone() { ! curl -sf "$PROM_URL/-/ready" >/dev/null; }
  if PROM_URL=$(yobs_prom --since 2m 2>"$INIT.yobs.err") && [ -n "$PROM_URL" ]; then
    echo "  PASS: y-obs prometheus: started at $PROM_URL ($(grep -o 'loaded [0-9]* samples[^:]*' "$INIT.yobs.err"))"
    expect "y-obs prometheus: progress shown, names autocompleting before the samples load" \
      grep -q 'names autocomplete; loading the samples' "$INIT.yobs.err"
    expect "y-obs prometheus: autocomplete knows app_requests_total" prom_names
    expect "y-obs prometheus: app_requests_total{code=\"200\"} of metrics-multi is 42" prom_value
    # Prometheus scrapes y-obs every 15 s: the bounds are there after one
    sleep 20
    expect "y-obs prometheus: minutes before the loaded window, through remote read" prom_older
    PUSHED_TO=$(prom_pushed to)
    sleep 45
    PUSHED_TO2=$(prom_pushed to)
    expect "y-obs prometheus: following moves the loaded window's end ($PUSHED_TO to $PUSHED_TO2)" test "${PUSHED_TO2:-0}" -gt "${PUSHED_TO:-0}"
    expect "y-obs prometheus: a repeated call reuses the instance" prom_reused
    expect "y-obs prometheus --stop ends it" yobs_prom --stop
    expect "y-obs prometheus: stopped, Prometheus answers no more" prom_gone
  else
    echo "  FAIL: y-obs prometheus did not start:" >&2
    sed 's/^/    /' "$INIT.yobs.err" | head -20 >&2
    ERRORS=$((ERRORS + 1))
  fi
  yobs_prom --stop >/dev/null 2>&1 || true
  rm -rf "$YOBS_RUN"
fi

phase "assert: compactor"
# The key rotation: the hourly objects so far, then PARQUET_KEY_ID
# itest-2. They keep their names, so their key; new hours and the day's
# partitions get itest-2.
HOURLY_OBJECTS="glob('s3://$BUCKET/hourly/$CLUSTER/*/*/*.parquet')"
q "COPY (SELECT file FROM $HOURLY_OBJECTS) TO '$ROTATED' (FORMAT csv, HEADER true)" >/dev/null
echo "  rotate PARQUET_KEY_ID to itest-2 after $(($(wc -l < "$ROTATED") - 1)) hourly objects"
$KUBECTL -n observability set env deployment/blobs-compactor PARQUET_KEY_ID=itest-2 >/dev/null
$KUBECTL -n observability rollout status deployment/blobs-compactor --timeout=360s >/dev/null
# The raw keys now. The compactor then runs on until a rolling and a past
# pass that both started after this snapshot have succeeded (read from its
# /metrics through the API server), so between them they listed every key
# of it, and is stopped: the sidecar assertion below checks the folds, not
# the wait. A stop lets the job in flight commit.
FOLDED="read_csv('s3://$BUCKET/hourly/$CLUSTER/_folded/*/*.csv', header = true, columns = {'key': 'VARCHAR', 'final': 'BOOLEAN'})"
RAW_KEYS="glob('s3://$BUCKET/raw1/$CLUSTER/*/*/*/*.jsonl.zst')"
q "COPY (SELECT file FROM $RAW_KEYS) TO '$SNAPSHOT' (FORMAT csv, HEADER true)" >/dev/null
SNAPSHOT_AT=$(($(date +%s) + 1))
# compactor_metric <name> <job> [namespace]: that sample of the running
# compactor's /metrics (it compacts one cluster: its series' cluster label
# is not matched)
compactor_metric() {
  local pod ns=${3:-observability}
  pod=$($KUBECTL -n "$ns" get pods -l app=blobs-compactor -o jsonpath='{.items[0].metadata.name}' 2>/dev/null) || return 0
  [ -n "$pod" ] || return 0
  $KUBECTL get --raw "/api/v1/namespaces/$ns/pods/$pod:9090/proxy/metrics" 2>/dev/null \
    | awk -v n="$1{" -v j="job=\"$2\"" 'index($1, n) == 1 && index($1, j) { print $2 }'
}
# started_since <job> <epoch seconds> [namespace]: the job's last run
# succeeded and started after then (its end less its duration)
started_since() {
  local end took
  end=$(compactor_metric blobs_compactor_last_success_timestamp_seconds "$1" "${3:-}")
  took=$(compactor_metric blobs_compactor_last_duration_seconds "$1" "${3:-}")
  [ -n "$end" ] && [ -n "$took" ] && awk -v e="$end" -v d="$took" -v t="$2" 'BEGIN { exit !(e - d > t) }'
}
# the put compactor too: by then its spool had every upload the shipper
# made before the snapshot, so between them its passes folded them all
deadline=$((SECONDS + 600))
until started_since rolling "$SNAPSHOT_AT" && started_since past "$SNAPSHOT_AT" \
  && started_since rolling "$SNAPSHOT_AT" "$PUT_NS" && started_since past "$SNAPSHOT_AT" "$PUT_NS"; do
  if [ $SECONDS -ge $deadline ]; then
    $KUBECTL -n observability logs deploy/blobs-compactor --tail=40 || true
    $KUBECTL -n "$PUT_NS" logs deploy/blobs-compactor --tail=40 || true
    fail "the compactors have not each completed a rolling and a past pass in the 600s after the snapshot"
  fi
  echo "  waiting for a rolling and a past pass of each compactor that start after the snapshot"
  sleep 10
done
$KUBECTL -n observability scale deployment/blobs-compactor --replicas=0 >/dev/null
deadline=$((SECONDS + 330))
while [ -n "$($KUBECTL -n observability get pods -l app=blobs-compactor -o name)" ]; do
  [ $SECONDS -lt $deadline ] || fail "the compactor's pod is still there 330s after scaling to 0"
  sleep 2
done

# The read-only logs API, blobs-compactor serve, in a pod of its own while
# no compactor folds $CLUSTER: a line the agent uploads now is in raw1/
# only, so the API can only answer it from there.
$KUBECTL -n observability delete pod logs-serve --ignore-not-found >/dev/null 2>&1
$KUBECTL -n observability run logs-serve --restart=Never --image="$COMPACTOR_IMAGE" --overrides="{
  \"apiVersion\": \"v1\", \"spec\": {\"automountServiceAccountToken\": false, \"containers\": [{
    \"name\": \"logs-serve\", \"image\": \"$COMPACTOR_IMAGE\", \"imagePullPolicy\": \"Never\",
    \"args\": [\"serve\"], \"envFrom\": [{\"secretRef\": {\"name\": \"blobs-compactor-s3\"}}],
    \"env\": [{\"name\": \"CLUSTER_NAME\", \"value\": \"$CLUSTER\"}, {\"name\": \"S3_BUCKET\", \"value\": \"$BUCKET\"},
      {\"name\": \"S3_ENDPOINT\", \"value\": \"versitygw.default.svc.cluster.local:7070\"}, {\"name\": \"S3_TLS\", \"value\": \"off\"},
      {\"name\": \"LOGS_LISTEN\", \"value\": \":8080\"},
      {\"name\": \"PARQUET_KEYS_DIR\", \"value\": \"/etc/blobs-compactor/parquet-keys\"}],
    \"volumeMounts\": [{\"name\": \"parquet-keys\", \"mountPath\": \"/etc/blobs-compactor/parquet-keys\", \"readOnly\": true}]}],
  \"volumes\": [{\"name\": \"parquet-keys\", \"secret\": {\"secretName\": \"blobs-compactor-parquet-keys\"}}]}}" >/dev/null
$KUBECTL -n observability wait --for=condition=Ready pod/logs-serve --timeout=120s >/dev/null \
  || { $KUBECTL -n observability logs pod/logs-serve || true; fail "blobs-compactor serve is not ready"; }
logs_forward observability pod/logs-serve "$SERVE_LOGS_PORT" SERVE_LOGS_PORT \
  || { $KUBECTL -n observability logs pod/logs-serve || true; fail "no logs API on the serve pod's port 8080"; }
GEN_POD=$($KUBECTL get pods -l app=log-generator -o jsonpath='{.items[0].metadata.name}')
SERVE_MARKER="serve-marker-$(date +%s)"
$KUBECTL exec deploy/log-generator -- sh -c "echo '$SERVE_MARKER' > /proc/1/fd/1"
deadline=$((SECONDS + 150))
while logs_get "$SERVE_LOGS_PORT" "/api/v1/logs/n/default/$GEN_POD/app?grep=$SERVE_MARKER&tail=0"; [ "$LOGS_BODY" != "$SERVE_MARKER" ]; do
  [ $SECONDS -lt $deadline ] || break
  sleep 5
done
serve_marker() {
  [ "$LOGS_BODY" = "$SERVE_MARKER" ] && logs_is 200 "text/plain; charset=utf-8" X-Logs-Source=blob X-Logs-Pod-Phase=Running \
    X-Logs-Tail-Effect=all
}
logs_expect "serve: a line the agent uploaded while no compactor runs is served, once, from raw1/" serve_marker
assert "serve: that line is in raw1/ and in no hourly object" \
  "SELECT (SELECT count(*) FROM logs WHERE message = '$SERVE_MARKER') = 1
      AND (SELECT count(*) FROM $(tier "s3://$BUCKET/hourly/$CLUSTER/logs/*/[0-9][0-9]") WHERE message = '$SERVE_MARKER') = 0"
$KUBECTL -n observability delete pod logs-serve --wait=false >/dev/null

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
# hourly <table> <date/HH>, daily <table> <date>: a FROM clause over the
# tier's object(s), each read with its key (tier above)
hourly() { tier "s3://$BUCKET/hourly/$CLUSTER/$1/$2"; }
daily() {
  case $1 in
    (logs|metrics) tier "s3://$BUCKET/daily/$CLUSTER/$1/$2/*/data_0" "hive_partitioning = false" ;;
    (*) tier "s3://$BUCKET/daily/$CLUSTER/$1/$2" ;;
  esac
}
for hour in $(q "SELECT regexp_extract(file, '_folded/(.*)\\.csv\$', 1) FROM glob('s3://$BUCKET/hourly/$CLUSTER/_folded/*/*.csv') ORDER BY 1"); do
  scoped "$hour.csv"
  QINIT=$SCOPED assert "hourly $hour: logs, metrics, podindex and apiserver hold exactly the distinct rows of the raw keys its sidecar lists" \
    "SELECT $(same_rows logs "$(hourly logs "$hour")") AND $(same_rows metrics "$(hourly metrics "$hour")")
        AND $(same_rows podindex "$(hourly podindex "$hour")") AND $(same_rows apiserver "$(hourly apiserver "$hour")")
        AND (SELECT count(*) FROM logs) > 0"
done
assert "hourly pods: the three incarnations of 'reused', each its own row" \
  "SELECT count(DISTINCT pod_uid) = 3 FROM $(tier "s3://$BUCKET/hourly/$CLUSTER/pods/*/[0-9][0-9]") WHERE pod = 'reused' AND container = 'reused'"

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
        {\"name\": \"S3_ENDPOINT\", \"value\": \"versitygw.default.svc.cluster.local:7070\"}, {\"name\": \"S3_TLS\", \"value\": \"off\"},
        {\"name\": \"PARQUET_KEYS_DIR\", \"value\": \"/etc/blobs-compactor/parquet-keys\"}, {\"name\": \"PARQUET_KEY_ID\", \"value\": \"itest-2\"}],
      \"volumeMounts\": [{\"name\": \"parquet-keys\", \"mountPath\": \"/etc/blobs-compactor/parquet-keys\", \"readOnly\": true}]}],
    \"volumes\": [{\"name\": \"parquet-keys\", \"secret\": {\"secretName\": \"blobs-compactor-parquet-keys\"}}]}}" >/dev/null
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
# daily_rows <day> <assertion label>: the day's logs, metrics, podindex and objects
# against the distinct rows of the raw keys its hourly sidecars list
daily_rows() {
  scoped "$1/*.csv"
  QINIT=$SCOPED assert "daily $1: $2" \
    "SELECT $(same_rows logs "$(daily logs "$1")") AND $(same_rows metrics "$(daily metrics "$1")")
        AND $(same_rows podindex "$(daily podindex "$1")") AND $(same_rows apiserver "$(daily apiserver "$1")")
        AND (SELECT count(*) FROM logs) > 0"
}
for day in $DAYS; do
  echo "  rollup $day"
  compactor_job "[\"rollup\", \"$day\"]"
  daily_rows "$day" "logs, metrics, podindex and apiserver hold exactly the distinct rows of the day's folded raw keys"
  assert "daily $day: the sidecar lists every key of the day's hourly sidecars" \
    "SELECT (SELECT list(key ORDER BY key) FROM read_csv('s3://$BUCKET/daily/$CLUSTER/_folded/$day.csv.zst', header = true))
          = (SELECT list(DISTINCT key ORDER BY key) FROM read_csv('s3://$BUCKET/hourly/$CLUSTER/_folded/$day/*.csv', header = true))"
done
LAST=$(echo "$DAYS" | tail -n 1)
assert "daily $LAST: a namespace's logs are one object, found by its hive key" \
  "SELECT count(*) = 1 AND bool_and(namespace = 'default') FROM (SELECT DISTINCT namespace, filename FROM $(tier "s3://$BUCKET/daily/$CLUSTER/logs/$LAST/*/data_0" "hive_partitioning = true, filename = true") WHERE namespace = 'default')"
# per incarnation over the days: one that ran across midnight has a row in each
assert "daily pods: the three incarnations of 'reused', each with its one line and exit 0" \
  "SELECT count(*) = 3 AND bool_and(lines = 1) AND bool_and(exited) AND count(*) FILTER (succeeded) >= 2
   FROM (SELECT pod_uid, sum(lines) AS lines, bool_or(exit_code = 0) AS exited, bool_or(phase = 'succeeded') AS succeeded
         FROM $(tier "s3://$BUCKET/daily/$CLUSTER/pods/[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]") WHERE pod = 'reused' AND container = 'reused' GROUP BY pod_uid)"

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
TIER_OBJECTS="(SELECT file FROM $HOURLY_OBJECTS UNION ALL SELECT file FROM glob('s3://$BUCKET/daily/$CLUSTER/*/*/*/*.parquet')
  UNION ALL SELECT file FROM glob('s3://$BUCKET/daily/$CLUSTER/*/*.parquet'))"
assert "encryption: every hourly and daily object is under key itest-1 or itest-2, one object per hour, table and namespace" \
  "SELECT count(*) > 0 AND count(*) FILTER (NOT regexp_matches(file, '\\.k-itest-[12]\\.parquet\$')) = 0
          AND count(*) = count(DISTINCT regexp_replace(file, '(\\.k-[a-z0-9-]+)?\\.parquet\$', ''))
   FROM $TIER_OBJECTS"
assert "encryption: the hourly objects from before the rotation keep their names, so their key itest-1" \
  "SELECT count(*) > 0 AND bool_and(file LIKE '%.k-itest-1.parquet') AND bool_and(file IN (SELECT file FROM $HOURLY_OBJECTS))
   FROM read_csv('$ROTATED', header = true)"
assert "encryption: the daily objects of $LAST, first written after the rotation, are under itest-2" \
  "SELECT count(*) > 0 AND bool_and(file LIKE '%.k-itest-2.parquet') FROM $TIER_OBJECTS WHERE file LIKE '%/daily/%/$LAST%'"
plain=$(q "SELECT count(*) FROM read_parquet('$(q "SELECT min(file) FROM $HOURLY_OBJECTS" | tr -d '[:space:]')')" || true)
case "$plain" in
  (*"is encrypted, but 'encryption_config' was not set"*) echo "  PASS: encryption: a read without the key fails" ;;
  (*) echo "  FAIL: encryption: a read without the key gave: $plain" >&2; ERRORS=$((ERRORS + 1)) ;;
esac
$KUBECTL -n observability scale deployment/blobs-compactor --replicas=1 >/dev/null

# ------------------------------------------------------- every cluster --

phase "assert: one compactor for every cluster of a bucket (itest/compactor-multi)"
# $CLUSTER's raw tier so far, copied into a bucket of its own twice, as
# $CLUSTER and $CLUSTER-copy: the compactor without CLUSTER_NAME finds both
# and writes each the same as the other, as two compactors of one cluster
# each would.
MULTI_BUCKET=node-agent-multi
$KUBECTL -n observability-multi scale deployment/blobs-compactor --replicas=0 >/dev/null 2>&1 || true
$KUBECTL delete pod copy-multi --ignore-not-found >/dev/null 2>&1
$KUBECTL run copy-multi --restart=Never --image=amazon/aws-cli:2.22.35 \
  --env=AWS_ACCESS_KEY_ID=demoaccess --env=AWS_SECRET_ACCESS_KEY=demosecret \
  --env=AWS_DEFAULT_REGION=us-east-1 \
  --command -- sh -ec "s3() { aws --endpoint-url http://versitygw:7070 s3 \"\$@\"; }
    s3 mb s3://$MULTI_BUCKET 2>/dev/null || true
    s3 rm --recursive --quiet s3://$MULTI_BUCKET/
    s3 sync --quiet s3://$BUCKET/raw1/$CLUSTER/ s3://$MULTI_BUCKET/raw1/$CLUSTER/
    s3 sync --quiet s3://$MULTI_BUCKET/raw1/$CLUSTER/ s3://$MULTI_BUCKET/raw1/$CLUSTER-copy/" >/dev/null
deadline=$((SECONDS + 600))
while :; do
  copy=$($KUBECTL get pod copy-multi -o jsonpath='{.status.phase}')
  case "$copy" in (Succeeded|Failed) break;; esac
  [ $SECONDS -lt $deadline ] || fail "copy-multi still $copy after 600s"
  sleep 3
done
[ "$copy" = Succeeded ] || { $KUBECTL logs pod/copy-multi >&2 || true; fail "could not copy raw1/$CLUSTER/ into s3://$MULTI_BUCKET/"; }
$KUBECTL delete pod copy-multi >/dev/null
$KUBECTL apply -k itest/compactor-multi >/dev/null
$KUBECTL -n observability-multi scale deployment/blobs-compactor --replicas=1 >/dev/null
$KUBECTL -n observability-multi rollout status deployment/blobs-compactor --timeout=180s
# multi_rows <table>: true when both clusters' hourly rows are there and equal
multi_rows() {
  echo "SELECT a > 0 AND a = b FROM (SELECT
    (SELECT count(*) FROM read_parquet('s3://$MULTI_BUCKET/hourly/$CLUSTER/$1/*/*.parquet')) AS a,
    (SELECT count(*) FROM read_parquet('s3://$MULTI_BUCKET/hourly/$CLUSTER-copy/$1/*/*.parquet')) AS b)"
}
multi_folded() {
  local c=$1
  echo "(SELECT count(*) FROM read_csv('s3://$MULTI_BUCKET/hourly/$c/_folded/*/*.csv', header = true, columns = {'key': 'VARCHAR', 'final': 'BOOLEAN'}))"
}
MULTI_RAW="(SELECT count(*) FROM glob('s3://$MULTI_BUCKET/raw1/$CLUSTER/*/*/*/*.jsonl.zst'))"
for _ in $(seq 1 60); do
  [ "$(q "SELECT $(multi_folded "$CLUSTER") = $MULTI_RAW AND $(multi_folded "$CLUSTER-copy") = $MULTI_RAW" | tr -d '[:space:]')" = true ] && break
  sleep 3
done
assert "every raw key of both clusters is folded" \
  "SELECT $MULTI_RAW > 0 AND $(multi_folded "$CLUSTER") = $MULTI_RAW AND $(multi_folded "$CLUSTER-copy") = $MULTI_RAW"
for t in logs metrics podindex; do
  assert "$t: both clusters' hourly rows, equal" "$(multi_rows $t)"
done
MULTI_POD=$($KUBECTL -n observability-multi get pod -l app=blobs-compactor -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
MULTI_METRICS=$($KUBECTL get --raw "/api/v1/namespaces/observability-multi/pods/$MULTI_POD:9090/proxy/metrics" 2>/dev/null || true)
multi_metrics() {
  grep -q "^blobs_compactor_raw_keys_read_total{cluster=\"$CLUSTER\"} [1-9]" <<< "$MULTI_METRICS" &&
    grep -q "^blobs_compactor_raw_keys_read_total{cluster=\"$CLUSTER-copy\"} [1-9]" <<< "$MULTI_METRICS"
}
expect "/metrics: a series per cluster" multi_metrics
MULTI_LOGS=$($KUBECTL -n observability-multi logs deployment/blobs-compactor)
multi_found() {
  grep -F '"msg":"cluster found"' <<< "$MULTI_LOGS" | grep -qF "\"cluster\":\"$CLUSTER\"" &&
    grep -F '"msg":"cluster found"' <<< "$MULTI_LOGS" | grep -qF "\"cluster\":\"$CLUSTER-copy\""
}
expect "the log names each cluster found" multi_found
# it is done: the rest of the run does not need it
$KUBECTL -n observability-multi scale deployment/blobs-compactor --replicas=0 >/dev/null

# ------------------------------------------------------- put and spool --

phase "assert: the put endpoint and spool mode ($PUT_CLUSTER)"
# put_logs: a FROM clause over $PUT_CLUSTER's hourly logs (plain objects)
put_logs() { tier "s3://$BUCKET/hourly/$PUT_CLUSTER/logs/*/[0-9][0-9]"; }
PUT_FOLDED="read_csv('s3://$BUCKET/hourly/$PUT_CLUSTER/_folded/*/*.csv', header = true, columns = {'key': 'VARCHAR', 'final': 'BOOLEAN'}, filename = true)"
assert "put: stock Fluent Bit's lines reach $PUT_CLUSTER's hourly tier with the identity from the CRI path" \
  "SELECT count(*) > 0 AND bool_and(cluster = '$PUT_CLUSTER' AND node <> '' AND namespace = 'default' AND length(pod_uid) = 36
     AND container = 'app' AND restart IS NOT NULL AND stream IN ('stdout', 'stderr')) FROM $(put_logs) WHERE pod LIKE 'log-generator-%'"
for m in "$MARKER" "$OUTAGE_MARKER" "$PUT_MARKER"; do
  assert "put: $m is in $PUT_CLUSTER's hourly logs exactly once" \
    "SELECT count(*) = 1 FROM $(put_logs) WHERE message = '$m'"
done
assert "put: the generator's counter is contiguous in $PUT_CLUSTER's hourly logs, the put compactor's downtime included" \
  "WITH c AS (SELECT DISTINCT pod, CAST(json_extract(message, '$.count') AS INTEGER) AS n FROM $(put_logs)
     WHERE pod LIKE 'log-generator-%' AND message LIKE '%hello from log-generator%' AND time > $SINCE),
   per_pod AS (SELECT pod, count(*) AS seen, min(n) AS lo, max(n) AS hi FROM c GROUP BY pod)
   SELECT count(*) > 0 AND bool_and(seen = (SELECT count(*) FROM range(lo, hi + 1) r WHERE r.range % 10 <> 0)) FROM per_pod"
assert "put: no raw1/$PUT_CLUSTER/ object is in the bucket (the agent's raw1/$CLUSTER/ ones are)" \
  "SELECT (SELECT count(*) FROM glob('s3://$BUCKET/raw1/$PUT_CLUSTER/*/*/*/*')) = 0 AND (SELECT count(*) FROM $RAW_KEYS) > 0"
if ! shipper_logs=$($KUBECTL -n "$PUT_NS" logs ds/stock-shipper 2>&1) \
  || ! outage_logs=$($KUBECTL -n "$PUT_NS" logs ds/stock-shipper --timestamps --since-time="$PUT_DOWN_AT" 2>&1); then
  echo "  FAIL: could not read the stock shipper's logs: $shipper_logs $outage_logs" >&2; ERRORS=$((ERRORS + 1))
else
  # only the lines of the outage: the shipper's start, before the put
  # compactor's, fails uploads too
  if awk -v up="$PUT_UP_AT" '$1 < up' <<<"$outage_logs" | grep -q 'PutObject request failed'; then
    echo "  PASS: the shipper's uploads failed while the put compactor was down ($PUT_DOWN_AT to $PUT_UP_AT)"
  else
    echo "  FAIL: no upload of the shipper failed while the put compactor was down ($PUT_DOWN_AT to $PUT_UP_AT)" >&2; ERRORS=$((ERRORS + 1))
  fi
  if grep -iE 'will not retry|retry-exhausted|quarantine|cannot be retried' <<<"$shipper_logs" >&2; then
    echo "  FAIL: the shipper gave up on a chunk (lines above)" >&2; ERRORS=$((ERRORS + 1))
  else
    echo "  PASS: the shipper gave up on no chunk"
  fi
fi
# the PUTs of aws-cli
if grep -q '^late-stored$' <<<"$PUT_PROBE"; then
  echo "  PASS: put: aws-cli's signed PutObject is stored"
else
  echo "  FAIL: put: aws-cli's signed PutObject was not stored (output above)" >&2; ERRORS=$((ERRORS + 1))
fi
if grep -q 'SignatureDoesNotMatch' <<<"$PUT_PROBE" && grep -q '^wrong-exit [1-9]' <<<"$PUT_PROBE"; then
  echo "  PASS: put: a PUT signed with a wrong secret is refused (SignatureDoesNotMatch)"
else
  echo "  FAIL: put: a PUT signed with a wrong secret was not refused (output above)" >&2; ERRORS=$((ERRORS + 1))
fi
if grep -q 'AccessDenied' <<<"$PUT_PROBE" && grep -q '^unsigned-exit [1-9]' <<<"$PUT_PROBE"; then
  echo "  PASS: put: an unsigned PUT is refused (AccessDenied)"
else
  echo "  FAIL: put: an unsigned PUT was not refused (output above)" >&2; ERRORS=$((ERRORS + 1))
fi
assert "put: the late object's hour is final, its sidecar lists the key and the hour holds its lines" \
  "SELECT (SELECT bool_or(final) AND count(*) FILTER (key = '$LATE_PUT_KEY') = 1
           FROM read_csv('s3://$BUCKET/hourly/$PUT_CLUSTER/_folded/$LATE_PUT_HOUR.csv', header = true, columns = {'key': 'VARCHAR', 'final': 'BOOLEAN'}))
      AND (SELECT count(*) = 3 FROM $(tier "s3://$BUCKET/hourly/$PUT_CLUSTER/logs/$LATE_PUT_HOUR") WHERE pod_uid = '$LATE_PUT_UID')"
assert "put: no sidecar lists the refused keys" \
  "SELECT count(*) = 0 FROM $PUT_FOLDED WHERE key LIKE '%/itest-late/0001-wrong.%' OR key LIKE '%/itest-late/0002-unsigned.%'"
# The spool after the final folds: no file of a final hour is left, the
# late object's hour included. Only files older than the last past pass's
# start count: a late upload to a final hour waits for the next past pass,
# and a file's time is its write's, which precedes its rename into the key
# by the time the body took (30 s allowed). The final hours are read
# first: an hour final then had its files of before that pass deleted by
# the time of the listing.
final_hours=$(q "SELECT DISTINCT regexp_extract(filename, '_folded/(.*)\\.csv\$', 1) AS h FROM $PUT_FOLDED WHERE final ORDER BY h")
past_started=$($KUBECTL get --raw "/api/v1/namespaces/$PUT_NS/pods/$($KUBECTL -n "$PUT_NS" get pods -l app=blobs-compactor \
    -o jsonpath='{.items[0].metadata.name}'):9090/proxy/metrics" 2>/dev/null \
  | awk 'index($1, "blobs_compactor_last_success_timestamp_seconds{") == 1 && index($1, "job=\"past\"") { e = $2 }
         index($1, "blobs_compactor_last_duration_seconds{") == 1 && index($1, "job=\"past\"") { d = $2 }
         END { if (e != "" && d != "") printf "%d\n", e - d - 30 }')
if [ -z "$past_started" ]; then
  echo "  FAIL: put: no past pass of the put compactor in its metrics" >&2; ERRORS=$((ERRORS + 1))
elif spool_sh "find /spool -type f -exec stat -c '%Y %n' {} +"; then
  spool_files=$(awk -v t="$past_started" '$1 < t { print $2 }' <<<"$POD_LOG")
  echo "  the spool holds $(grep -c . <<<"$POD_LOG" || true) files, $(grep -c . <<<"$spool_files" || true) of them written before the last past pass; final hours: ${final_hours//$'\n'/ }"
  left=$(for h in $final_hours; do grep -F "/spool/raw1/$PUT_CLUSTER/$h/" <<<"$spool_files" || true; done)
  if grep -qxF "$LATE_PUT_HOUR" <<<"$final_hours" && [ -z "$left" ] && ! grep -qF /itest-late/ <<<"$spool_files"; then
    echo "  PASS: put: the spool keeps no file of a final hour, the late object's included"
  else
    echo "  FAIL: put: the spool keeps files of final hours, or no hour is final: ${left:-none}" >&2; ERRORS=$((ERRORS + 1))
  fi
else
  echo "  FAIL: could not list the spool: $POD_LOG" >&2; ERRORS=$((ERRORS + 1))
fi

# ------------------------------------------------------------- logs API --

# logs_api <local port> <the generator's phase> <reused's phase>: the logs
# API's answers for the generator, its markers, the terminated pod
# 'reused' (its latest incarnation), and the errors
gen_tail() {
  logs_is 200 "text/plain; charset=utf-8" X-Logs-Source=blob "X-Logs-Pod-Phase=$1" X-Logs-Tail-Effect=cap && logs_tail_lines 5
}
once() { [ "$LOGS_BODY" = "$1" ] && logs_is 200 "text/plain; charset=utf-8" X-Logs-Tail-Effect=all; }
reused() {
  local phase=$1
  # a run that crossed midnight UTC reads the day before from the daily tier
  [ "$LOGS_BODY" = "[pod/reused/reused] reused incarnation 3" ] || return 1
  logs_is 200 "text/plain; charset=utf-8" X-Logs-Source=blob X-Logs-Tail-Effect=all "X-Logs-Pod-Phase=$phase" \
    || { [ "$(date -u +%F)" != "$RUN_DATE" ] && logs_is 200 "text/plain; charset=utf-8" X-Logs-Pod-Phase=Archived; }
}
problem() { logs_is "$1" application/problem+json && grep -qF "\"status\":$1,\"detail\":\"$2\"" <<<"$LOGS_BODY"; }
openapi() { logs_is 200 application/openapi+json && grep -q '"operationId": *"get-pod-logs-container"' <<<"$LOGS_BODY"; }
logs_api() {
  local port=$1 m
  logs_get "$port" "/api/v1/logs/n/default/$GEN_POD/app?tail=5&timestamps=true"
  logs_expect "logs API: the generator's last 5 lines, timestamped, as text/plain, X-Logs-* headers" gen_tail "$2"
  for m in "$MARKER" "$OUTAGE_MARKER" "$PUT_MARKER"; do
    logs_get "$port" "/api/v1/logs/n/default/$GEN_POD?grep=$m&tail=0"
    logs_expect "logs API: $m once" once "$m"
  done
  logs_get "$port" "/api/v1/logs/n/default/reused?prefix=true"
  logs_expect "logs API: the terminated pod 'reused' is its latest incarnation, phase $3" reused "$3"
  logs_get "$port" "/api/v1/logs/n/default/no-such-pod"
  logs_expect "logs API: an unknown pod is a 404 problem" problem 404 "no archived logs for default/no-such-pod"
  logs_get "$port" "/api/v1/logs/n/default/reused/nope"
  logs_expect "logs API: an unknown container is a 404 problem that names the known ones" \
    problem 404 'container \"nope\" not found in pod default/reused, known containers: [reused]'
  logs_get "$port" "/api/v1/logs/n/default/reused?source=k8s"
  logs_expect "logs API: source=k8s is a 400 problem" \
    problem 400 "invalid source value: this server has no k8s source, use 'blob' or omit it"
  logs_get "$port" "/openapi.json"
  logs_expect "logs API: /openapi.json is the spec" openapi
}
phase "assert: the logs API ($CLUSTER: encrypted tiers, raw1/ in the bucket)"
$KUBECTL -n observability rollout status deployment/blobs-compactor --timeout=180s >/dev/null
logs_forward observability svc/blobs-compactor-logs "$LOGS_PORT" LOGS_PORT || fail "no logs API on svc/blobs-compactor-logs in observability"
logs_api "$LOGS_PORT" Running Succeeded
# stock Fluent Bit ships no podindex: the phase is unknown
phase "assert: the logs API ($PUT_CLUSTER: plain tiers, the spool)"
logs_forward "$PUT_NS" svc/blobs-compactor-logs "$PUT_LOGS_PORT" PUT_LOGS_PORT || fail "no logs API on svc/blobs-compactor-logs in $PUT_NS"
logs_api "$PUT_LOGS_PORT" Unknown Unknown

# The host agent on the node itself (HOST_MODE.md): tarball, systemd unit,
# its own bucket. It needs the qemu node; k3s-in-docker has no systemd.
if [ "$BACKEND" = docker ]; then
  echo "  SKIP: host phase (k3s-in-docker is not a host with systemd)"
else
  CONTEXT=$CONTEXT CLUSTER_CONFIG=$CLUSTER_CONFIG itest/host/phase.sh || ERRORS=$((ERRORS + 1))
fi

echo
if [ "$ERRORS" -gt 0 ]; then
  $KUBECTL -n observability logs ds/node-agent --tail=40 || true
  fail "$ERRORS assertion(s) failed"
fi
echo "PASS: all assertions passed"
