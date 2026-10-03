#!/usr/bin/env bash
# The one build recipe for blobs-compactor (compactor/README.md, "The
# build script"), the same on a developer machine and in CI. The host
# needs bash and docker; Go, gcc, contain and crane run in the pinned
# builder (build.env).
#
#   build.sh test               go vet and go test, host arch
#   build.sh image [oci-dir]    binaries for PLATFORMS, httpfs, OCI layout,
#                               selftest on the host arch; prints the digest
#   build.sh check              image, and fail unless the digest is the release pin
#   build.sh bump               image, and move the release pin if the digest moved
#   build.sh publish            check, then push the pinned tag (REGISTRY_USER, REGISTRY_TOKEN)
#   build.sh host-setup         register qemu for the foreign arch (Linux without Docker Desktop)
#   build.sh tidy               go mod tidy
#
# PLATFORMS defaults to the release platforms; the itest sets the host's.
# The Go caches live in a docker volume, not a bind mount: Docker Desktop's
# file sharing loses files when Go extracts modules into a host directory.
[ -z "${DEBUG:-}" ] || set -x
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$HERE/.." && pwd)
# shellcheck source=build.env
. "$HERE/build.env"
PLATFORMS=${PLATFORMS:-$RELEASE_PLATFORMS}
OUT=target/compactor
PIN=compactor/deploy/release/kustomization.yaml
CACHE=${BUILD_CACHE_VOLUME:-blobs-compactor-cache}

# The digest is a function of the exact Go release, and GOTOOLCHAIN=auto
# lets another go win over go.mod's toolchain line:
# the toolchain line, else the go line.
GOTOOLCHAIN=$(sed -n 's/^toolchain //p' "$HERE/go.mod")
[ -n "$GOTOOLCHAIN" ] || GOTOOLCHAIN=go$(sed -n 's/^go //p' "$HERE/go.mod")
case "$BUILDER" in
  golang:"${GOTOOLCHAIN#go}"-*) ;;
  *) echo "[build.sh] warning: $BUILDER is not $GOTOOLCHAIN, every cold build downloads the toolchain" >&2 ;;
esac

HOST_PLATFORM=linux/$(docker version --format '{{.Server.Arch}}')

log() { echo "[build.sh] $*" >&2; }

# builder_ref <platform>: the builder's manifest for that platform
builder_ref() {
  local var=BUILDER_linux_${1#linux/}
  echo "${!var:?$var missing in build.env}"
}

# builder <platform> <command...>: runs in the pinned builder with the
# repo at /repo; BUILDER_ENV adds -e flags
BUILDER_ENV=()
builder() {
  local platform=$1; shift
  docker run --rm --platform "$platform" --user "$(id -u):$(id -g)" \
    -v "$REPO:/repo" -w /repo/compactor -v "$CACHE:/cache" \
    -e HOME=/tmp -e GOCACHE="/cache/go-build/${platform#linux/}" -e GOMODCACHE=/cache/gomod \
    -e GOTOOLCHAIN="$GOTOOLCHAIN" -e GOFLAGS=-mod=readonly -e CGO_ENABLED=1 \
    ${BUILDER_ENV[@]+"${BUILDER_ENV[@]}"} \
    "$(builder_ref "$platform")" "$@"
}

# The volume is created by docker as root; the builder runs as the caller.
cache_init() {
  docker run --rm --platform "$HOST_PLATFORM" -v "$CACHE:/cache" "$(builder_ref "$HOST_PLATFORM")" \
    chown "$(id -u):$(id -g)" /cache
}

platforms() { echo "${PLATFORMS//,/ }"; }

need_platform() {
  local err
  [ "$1" = "$HOST_PLATFORM" ] && return 0
  err=$(docker run --rm --platform "$1" "$(builder_ref "$1")" true 2>&1 >/dev/null) && return 0
  log "this docker host cannot run $1: $err"
  log "if qemu is missing, run compactor/build.sh host-setup once"
  exit 1
}

compile() {
  log "compile $1 with $GOTOOLCHAIN"
  builder "$1" go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o "/repo/$OUT/app/$1/blobs-compactor" .
}

# httpfs <platform>: the signed extension, verified against build.env
# before it is unpacked; the download is cached in the volume
httpfs() {
  local arch=${1#linux/} var
  var=HTTPFS_SHA256_linux_$arch
  log "httpfs $DUCKDB_VERSION linux_$arch"
  # shellcheck disable=SC2016 # expanded by the builder's sh
  builder "$HOST_PLATFORM" sh -euc '
    gz=/cache/duckdb/$1/linux_$2/httpfs.duckdb_extension.gz
    if ! echo "$3  $gz" | sha256sum -c --status 2>/dev/null; then
      mkdir -p "${gz%/*}"
      curl -fsSL -o "$gz.part" "https://extensions.duckdb.org/$1/linux_$2/httpfs.duckdb_extension.gz"
      echo "$3  $gz.part" | sha256sum -c --status \
        || { echo "httpfs $1 linux_$2: sha256 differs from build.env" >&2; exit 1; }
      mv "$gz.part" "$gz"
    fi
    mkdir -p "/repo/$4/duckdb/linux/$2"
    gunzip -c "$gz" > "/repo/$4/duckdb/linux/$2/httpfs.duckdb_extension"
  ' sh "$DUCKDB_VERSION" "$arch" "${!var:?$var missing in build.env}" "$OUT"
}

# selftest: the host-arch binary and extension in the pinned base, as
# the image runs them (read-only root, tmpfs /tmp, nonroot)
selftest() {
  local base
  base=$(sed -n 's/^base: //p' "$HERE/contain.yaml")
  log "selftest $HOST_PLATFORM in $base"
  docker run --rm --platform "$HOST_PLATFORM" --read-only --tmpfs /tmp --user 65532:65534 \
    -v "$REPO/$OUT/app/$HOST_PLATFORM/blobs-compactor:/usr/local/bin/blobs-compactor:ro" \
    -v "$REPO/$OUT/duckdb/$HOST_PLATFORM/httpfs.duckdb_extension:/usr/local/lib/duckdb/httpfs.duckdb_extension:ro" \
    "$base" /usr/local/bin/blobs-compactor selftest >&2
}

# image [oci-dir]: sets DIGEST. bash runs $( ) without errexit, so image,
# check and bump are called as commands, never in $( ), and the first
# failed step stops the script.
DIGEST=
image() {
  local oci=${1:-$OUT/oci} p
  DIGEST=
  cache_init
  for p in $(platforms); do need_platform "$p"; done
  rm -rf "${REPO:?}/$OUT/app" "${REPO:?}/$OUT/duckdb" "${REPO:?}/$oci"
  for p in $(platforms); do
    compile "$p"
    httpfs "$p"
  done
  case ",$PLATFORMS," in (*",$HOST_PLATFORM,"*) selftest ;; esac
  log "assemble $PLATFORMS with contain $CONTAIN"
  BUILDER_ENV=(-e "PLATFORMS=$PLATFORMS" -e "IMAGE=$IMAGE_REPO:build")
  builder "$HOST_PLATFORM" go run "github.com/turbokube/contain/cmd/contain@$CONTAIN" \
    build --platforms-env-require --push=false --output "/repo/$oci" --logger plain >&2
  BUILDER_ENV=()
  # the layout's first manifest is the image index
  DIGEST=$(sed -n 's/.*"digest": *"\(sha256:[0-9a-f]*\)".*/\1/p' "$REPO/$oci/index.json" | head -n 1)
  [ -n "$DIGEST" ] || { log "no image index digest in $oci/index.json"; exit 1; }
}

pinned() { [ ! -f "$REPO/$PIN" ] || sed -n "s/^  $1: //p" "$REPO/$PIN"; }

check() {
  local want
  image
  want=$(pinned digest)
  if [ "$DIGEST" != "$want" ]; then
    log "built $DIGEST"
    log "but $PIN pins ${want:-nothing}"
    log "run compactor/build.sh bump and commit the result"
    exit 1
  fi
  log "the build reproduces the pin $(pinned newTag)@$want"
}

bump() {
  local tag
  image
  if [ "$DIGEST" = "$(pinned digest)" ]; then
    log "pin unchanged: $(pinned newTag)@$DIGEST"
    return
  fi
  tag=$(date -u +%Y%m%dT%H%M%SZ)
  mkdir -p "$(dirname "$REPO/$PIN")"
  cat > "$REPO/$PIN" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
# The release: the base with the pin, written by compactor/build.sh bump
# and asserted by build.sh check. The tag is not in the binary: the digest
# moves only when the image content does.
resources:
- ../base
images:
- name: $IMAGE_REPO
  newTag: $tag
  digest: $DIGEST
EOF
  log "pin moved to $tag@$DIGEST"
}

publish() {
  local tag
  : "${REGISTRY_USER:?}" "${REGISTRY_TOKEN:?}"
  check
  tag=$(pinned newTag)
  log "push $IMAGE_REPO:$tag"
  BUILDER_ENV=(-e REGISTRY_USER -e REGISTRY_TOKEN -e "CRANE=$CRANE")
  # shellcheck disable=SC2016 # expanded by the builder's sh
  builder "$HOST_PLATFORM" sh -euc '
    crane() { go run "github.com/google/go-containerregistry/cmd/crane@$CRANE" "$@"; }
    echo "$REGISTRY_TOKEN" | crane auth login "${3%%/*}" -u "$REGISTRY_USER" --password-stdin
    crane push --index "/repo/$1" "$3:$2"
  ' sh "$OUT/oci" "$tag" "$IMAGE_REPO"
  BUILDER_ENV=()
  log "pushed $IMAGE_REPO:$tag@$DIGEST"
}

host_setup() {
  local p arches=""
  for p in $(platforms); do
    [ "$p" = "$HOST_PLATFORM" ] || arches="$arches${arches:+,}${p#linux/}"
  done
  [ -n "$arches" ] || { log "nothing to emulate on $HOST_PLATFORM"; return; }
  docker run --privileged --rm "$BINFMT" --install "$arches"
}

# errexit stops at the failed step, after that step's own output
on_exit() { local rc=$?; [ "$rc" = 0 ] || log "${CMD:-build.sh} failed, exit $rc"; }
trap on_exit EXIT
CMD=${1:-}
case "$CMD" in
  test)       cache_init; builder "$HOST_PLATFORM" sh -euc 'go vet ./... && go test ./...' ;;
  image)      image "${2:-}"; echo "$DIGEST" ;;
  check)      check ;;
  bump)       bump ;;
  publish)    publish ;;
  host-setup) host_setup ;;
  tidy)       cache_init; BUILDER_ENV=(-e GOFLAGS=-mod=mod); builder "$HOST_PLATFORM" go mod tidy ;;
  *)          trap - EXIT; sed -n '2,/^\[ -z/p' "$0" | sed '$d' >&2; exit 2 ;;
esac
