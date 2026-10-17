#!/usr/bin/env bash
# The one build recipe for blobs-compactor (compactor/README.md, "The
# build script"), the same on a developer machine and in CI. The host
# needs bash and docker; Go, gcc, contain and crane run in the pinned
# builder (build.env).
#
#   build.sh test               go vet and go test, host arch
#   build.sh image [oci-dir]    binaries for PLATFORMS, httpfs, OCI layout,
#                               selftest on the host arch; prints the digest
#   build.sh image-buildkit [oci-dir]
#                               the same steps for one platform, run by buildctl against
#                               BUILDKIT_HOST instead of docker (a host without docker)
#   build.sh check              image, and fail unless the digest is the release pin
#   build.sh bump               image, and move the release pin if the digest moved
#   build.sh publish            check, then push the pinned tag (REGISTRY_USER, REGISTRY_TOKEN)
#   build.sh host-setup         register qemu for the foreign arch (Linux without Docker Desktop)
#   build.sh tidy               go mod tidy
#
# PLATFORMS defaults to the release platforms; the itest sets the host's.
# The Go caches live in a docker volume, not a bind mount: Docker Desktop's
# file sharing loses files when Go extracts modules into a host directory.
# A remote daemon (DOCKER_HOST tcp://, such as y-cluster dockerhost) shares
# no files with this host: the repository is copied into a volume of its
# own instead of mounted, and what the build writes is copied back.
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
REMOTE=
case "${DOCKER_HOST:-}" in (tcp://*|ssh://*) REMOTE=${BUILD_SRC_VOLUME:-blobs-compactor-src} ;; esac

# The digest is a function of the exact Go release, and GOTOOLCHAIN=auto
# lets another go win over go.mod's toolchain line:
# the toolchain line, else the go line.
GOTOOLCHAIN=$(sed -n 's/^toolchain //p' "$HERE/go.mod")
[ -n "$GOTOOLCHAIN" ] || GOTOOLCHAIN=go$(sed -n 's/^go //p' "$HERE/go.mod")
case "$BUILDER" in
  golang:"${GOTOOLCHAIN#go}"-*) ;;
  *) echo "[build.sh] warning: $BUILDER is not $GOTOOLCHAIN, every cold build downloads the toolchain" >&2 ;;
esac

if command -v docker >/dev/null 2>&1; then
  HOST_PLATFORM=linux/$(docker version --format '{{.Server.Arch}}')
else
  case "$(uname -m)" in (x86_64) HOST_PLATFORM=linux/amd64 ;; (aarch64|arm64) HOST_PLATFORM=linux/arm64 ;; (*) HOST_PLATFORM=linux/$(uname -m) ;; esac
fi

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
    -v "${REMOTE:-$REPO}:/repo" -w /repo/compactor -v "$CACHE:/cache" \
    -e HOME=/tmp -e GOCACHE="/cache/go-build/${platform#linux/}" -e GOMODCACHE=/cache/gomod \
    -e GOTOOLCHAIN="$GOTOOLCHAIN" -e GOFLAGS=-mod=readonly -e CGO_ENABLED=1 \
    ${BUILDER_ENV[@]+"${BUILDER_ENV[@]}"} \
    "$(builder_ref "$platform")" "$@"
}

# The volume is created by docker as root; the builder runs as the caller.
# With a remote daemon the repository volume too, which then gets a fresh
# copy of the repository, without .git and target/.
cache_init() {
  docker run --rm --platform "$HOST_PLATFORM" -v "$CACHE:/cache" "$(builder_ref "$HOST_PLATFORM")" \
    chown "$(id -u):$(id -g)" /cache
  [ -n "$REMOTE" ] || return 0
  docker run --rm --platform "$HOST_PLATFORM" -v "$REMOTE:/repo" "$(builder_ref "$HOST_PLATFORM")" \
    sh -c 'find /repo -mindepth 1 -maxdepth 1 -exec rm -rf {} + && chown "$1" /repo' sh "$(id -u):$(id -g)"
  tar -C "$REPO" --exclude=./.git --exclude=./target -cf - . \
    | docker run -i --rm --platform "$HOST_PLATFORM" --user "$(id -u):$(id -g)" -v "$REMOTE:/repo" \
      "$(builder_ref "$HOST_PLATFORM")" tar -C /repo -xf -
}

# copy_back <path...>: with a remote daemon, the build's files at those
# paths under the repository, from its volume to this host
copy_back() {
  [ -n "$REMOTE" ] || return 0
  docker run --rm --platform "$HOST_PLATFORM" -v "$REMOTE:/repo" "$(builder_ref "$HOST_PLATFORM")" \
    tar -C /repo -cf - "$@" | tar -C "$REPO" -xf -
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
  local mounts=(
    -v "$REPO/$OUT/app/$HOST_PLATFORM/blobs-compactor:/usr/local/bin/blobs-compactor:ro"
    -v "$REPO/$OUT/duckdb/$HOST_PLATFORM/httpfs.duckdb_extension:/usr/local/lib/duckdb/httpfs.duckdb_extension:ro"
  )
  if [ -n "$REMOTE" ]; then
    # a volume mounts directories only: the two directories, in place of
    # the base's (empty) /usr/local/bin and /usr/local/lib/duckdb
    mounts=(
      --mount "type=volume,src=$REMOTE,dst=/usr/local/bin,volume-subpath=$OUT/app/$HOST_PLATFORM,readonly"
      --mount "type=volume,src=$REMOTE,dst=/usr/local/lib/duckdb,volume-subpath=$OUT/duckdb/$HOST_PLATFORM,readonly"
    )
  fi
  docker run --rm --platform "$HOST_PLATFORM" --read-only --tmpfs /tmp --user 65532:65534 \
    "${mounts[@]}" "$base" /usr/local/bin/blobs-compactor selftest >&2
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
  # with a remote daemon the layout was written in the volume: replace the
  # host's copy; with a local one it is the host's already
  [ -z "$REMOTE" ] || rm -rf "${REPO:?}/$oci"
  copy_back "$OUT/app" "$OUT/duckdb" "$oci"
  # the layout's first manifest is the image index
  DIGEST=$(sed -n 's/.*"digest": *"\(sha256:[0-9a-f]*\)".*/\1/p' "$REPO/$oci/index.json" | head -n 1)
  [ -n "$DIGEST" ] || { log "no image index digest in $oci/index.json"; exit 1; }
}

# image_buildkit [oci-dir]: image's compile, httpfs, selftest and contain steps, with the same
# pins, as one BuildKit build. One platform, the selftest's; the release build stays image's.
image_buildkit() {
  local oci=${1:-$OUT/oci} p=${PLATFORMS} arch dir var base
  case "$p" in (*,*) log "image-buildkit builds one platform, PLATFORMS=$p"; exit 1 ;; esac
  [ -n "${BUILDKIT_HOST:-}" ] || { log "image-buildkit needs BUILDKIT_HOST"; exit 1; }
  command -v buildctl >/dev/null 2>&1 || { log "image-buildkit needs buildctl"; exit 1; }
  arch=${p#linux/}
  var=HTTPFS_SHA256_linux_$arch
  base=$(sed -n 's/^base: //p' "$HERE/contain.yaml")
  dir=$(mktemp -d); trap 'rm -rf "$dir"' RETURN
  cat >"$dir/Dockerfile" <<DOCKERFILE
FROM $(builder_ref "$p") AS build
ENV HOME=/tmp GOFLAGS=-mod=readonly CGO_ENABLED=1 GOTOOLCHAIN=$GOTOOLCHAIN
COPY . /repo/compactor
WORKDIR /repo/compactor
RUN go build -trimpath -buildvcs=false -ldflags='-s -w' -o /repo/$OUT/app/$p/blobs-compactor .
RUN mkdir -p /repo/$OUT/duckdb/$p \\
 && curl -fsSL -o /tmp/httpfs.gz "https://extensions.duckdb.org/$DUCKDB_VERSION/linux_$arch/httpfs.duckdb_extension.gz" \\
 && echo "${!var:?$var missing in build.env}  /tmp/httpfs.gz" | sha256sum -c --status \\
 && gunzip -c /tmp/httpfs.gz > /repo/$OUT/duckdb/$p/httpfs.duckdb_extension

FROM $base AS selftest
COPY --from=build /repo/$OUT/app/$p/blobs-compactor /usr/local/bin/blobs-compactor
COPY --from=build /repo/$OUT/duckdb/$p/httpfs.duckdb_extension /usr/local/lib/duckdb/httpfs.duckdb_extension
USER 65532:65534
RUN ["/usr/local/bin/blobs-compactor", "selftest"]

FROM build AS assemble
COPY --from=selftest /usr/local/bin/blobs-compactor /tmp/selftested
ENV PLATFORMS=$p IMAGE=$IMAGE_REPO:build
RUN go run github.com/turbokube/contain/cmd/contain@$CONTAIN build --platforms-env-require --push=false --output /out --logger plain

FROM scratch
COPY --from=assemble /out /
DOCKERFILE
  log "image-buildkit $p on $BUILDKIT_HOST"
  rm -rf "${REPO:?}/$oci"
  buildctl build --frontend dockerfile.v0 --local context="$HERE" --local dockerfile="$dir" \
    --opt platform="$p" --output "type=local,dest=$REPO/$oci" >&2
  DIGEST=$(sed -n 's/.*"digest": *"\(sha256:[0-9a-f]*\)".*/\1/p' "$REPO/$oci/index.json" | head -n 1)
  [ -n "$DIGEST" ] || { log "no image digest in $oci/index.json"; exit 1; }
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
    # the one image in the layout is the index the pin names; with --index
    # crane would push index.json itself, a wrapper around it, under the tag
    crane push "/repo/$1" "$3:$2"
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
  image-buildkit) image_buildkit "${2:-}"; echo "$DIGEST" ;;
  check)      check ;;
  bump)       bump ;;
  publish)    publish ;;
  host-setup) host_setup ;;
  tidy)       cache_init; BUILDER_ENV=(-e GOFLAGS=-mod=mod); builder "$HOST_PLATFORM" go mod tidy
              copy_back compactor/go.mod compactor/go.sum ;;
  *)          trap - EXIT; sed -n '2,/^\[ -z/p' "$0" | sed '$d' >&2; exit 2 ;;
esac
