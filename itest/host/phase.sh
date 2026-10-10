#!/usr/bin/env bash
# The host phase as test.sh runs it: builds the tarball unless there is one,
# finds how to reach the itest node, and runs run.sh. `./test.sh host` runs
# only this; ./test.sh runs it after the node agent's assertions.
#
# Env (the ones test.sh also takes keep their meaning):
#   CLUSTER_CONFIG  y-cluster config dir (default itest/cluster/qemu)
#   CONTEXT         the kubectl context (default node-agent)
#   KUBECONFIG      default itest/cluster/kubeconfig
#   HOST_TARBALL    use this tarball (its .sha256 beside it), do not build
#   SKIP_BUILD=1    use target/host/*.tar.gz if there is one
#   HOST_VERSION    the version in the tarball's name (default dev)
#   HOST_BUILD_ARGS extra docker buildx flags, e.g. a CI cache
#   LOCAL_PORT      this host's port for the gateway's port-forward (default
#                   17071; itest/host/run.sh)
#   BUILDKIT_HOST   build with buildctl against this BuildKit (set by test.sh
#                   when it built the image in the itest cluster); without it
#                   docker builds, and without docker there is no builder
set -euo pipefail
cd "$(dirname "$0")/../.."

CLUSTER_CONFIG=${CLUSTER_CONFIG:-itest/cluster/qemu}
CONTEXT=${CONTEXT:-node-agent}
export KUBECONFIG=${KUBECONFIG:-$PWD/itest/cluster/kubeconfig}
export CONTEXT

phase() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# the host phase needs systemd, auditd and a reboot: the qemu node
backend=$(y-cluster detect --context "$CONTEXT" 2>/dev/null) || fail "no running cluster for context $CONTEXT"
[ "$backend" = qemu ] || { echo "SKIP: the host phase needs the qemu node ($CONTEXT is $backend)"; exit 0; }

if [ -n "${HOST_TARBALL:-}" ]; then
  tarball=$HOST_TARBALL
else
  tarball=
  if [ "${SKIP_BUILD:-}" = 1 ]; then
    tarball=$(find target/host -name 'observability-to-blobs-host-*-linux-*.tar.gz' 2>/dev/null | head -n 1)
  fi
  if [ -z "$tarball" ]; then
    phase "host: build the tarball"
    if [ -n "${BUILDKIT_HOST:-}" ]; then builder=buildctl
    elif command -v docker >/dev/null 2>&1; then builder=docker
    else fail "no builder for the tarball: install docker, set BUILDKIT_HOST, or set HOST_TARBALL"; fi
    rm -rf target/host
    BUILDER=$builder BUILDX_ARGS="${HOST_BUILD_ARGS:-}" HOST_VERSION="${HOST_VERSION:-dev}" \
      images/fluentbit/host/build.sh target/host
    tarball=$(find target/host -name 'observability-to-blobs-host-*-linux-*.tar.gz' | head -n 1)
  fi
fi
[ -f "$tarball" ] || fail "no tarball ($tarball)"

# the node's sshd, from the cluster config: sshPort, name and the cache
# directory where y-cluster keeps the node's key
cfg=$CLUSTER_CONFIG/y-cluster-provision.yaml
[ -f "$cfg" ] || fail "no $cfg"
ssh_port=$(sed -n 's/^sshPort: *"\{0,1\}\([0-9]\{1,\}\)"\{0,1\} *$/\1/p' "$cfg")
name=$(sed -n 's/^name: *\([^ ]*\) *$/\1/p' "$cfg")
cache=$(sed -n 's/^cacheDir: *\([^ ]*\) *$/\1/p' "$cfg")
[ -n "$ssh_port" ] && [ -n "$name" ] || fail "sshPort and name not found in $cfg"
export VM_SSH_PORT=$ssh_port
export VM_SSH_KEY=${cache:-$HOME/.cache/y-cluster-qemu}/$name-ssh
export TARBALL=$tarball

phase "host: run the phase with $tarball"
exec itest/host/run.sh
