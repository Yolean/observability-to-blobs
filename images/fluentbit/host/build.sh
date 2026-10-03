#!/usr/bin/env bash
# Builds the host agent tarball (HOST_MODE.md) with the Dockerfile's
# host-tarball stage, on the base the host runs, and leaves the tarball and
# its .sha256 in the output directory.
#
#   host/build.sh [output dir]        default: target/host under the repository
#
# Env:
#   HOST_VERSION   the version in the file name (default: dev)
#   BUILD_BASE     the distribution image, with its digest (default: Ubuntu 24.04)
#   BUILDER        docker (docker buildx, the default) or buildctl (against
#                  BUILDKIT_HOST, as test.sh builds the image without docker)
#   BUILDX_ARGS    extra docker buildx flags, e.g. a CI cache
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
ctx=$here/..
out=${1:-$here/../../../target/host}
base=${BUILD_BASE:-ubuntu:24.04@sha256:a853f94d226358a79c740cfc7bce0c289748f3fe3488d921d038ccd752c61b60}
version=${HOST_VERSION:-dev}
# exec is built in (the heartbeat), the Kafka output and the Go plugin
# loader are not: the host agent has no use for them and its binary then
# needs fewer libraries from the host
cmake_extra="-DFLB_IN_EXEC=On -DFLB_KAFKA=Off -DFLB_PROXY_GO=Off"
mkdir -p "$out"
case "${BUILDER:-docker}" in
  docker)
    read -ra extra <<< "${BUILDX_ARGS:-}"
    docker buildx build "${extra[@]}" \
      --target host-tarball \
      --build-arg "BUILD_BASE=$base" \
      --build-arg "FLB_CMAKE_EXTRA=$cmake_extra" \
      --build-arg "HOST_VERSION=$version" \
      --output "type=local,dest=$out" \
      "$ctx";;
  buildctl)
    buildctl build --frontend dockerfile.v0 \
      --local "context=$ctx" --local "dockerfile=$ctx" \
      --opt target=host-tarball \
      --opt "build-arg:BUILD_BASE=$base" \
      --opt "build-arg:FLB_CMAKE_EXTRA=$cmake_extra" \
      --opt "build-arg:HOST_VERSION=$version" \
      --output "type=local,dest=$out";;
  *) echo "BUILDER: docker or buildctl" >&2; exit 2;;
esac
ls -l "$out"
