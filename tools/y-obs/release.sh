#!/usr/bin/env bash
# release.sh OUTDIR [VERSION]: builds y-obs for this host's platform with
# DuckDB's signed httpfs extension embedded, so the binary needs no
# extension download at run time, into OUTDIR/obs-{os}-{arch} with a
# .sha256 beside it. The asset is obs: ystack's y-bin entry installs it
# with its own y- prefix. The release workflow runs it on one native runner
# per platform: duckdb-go links prebuilt static libraries through cgo, so
# no cross compiling. Needs go, a C compiler, curl and sha256sum or
# shasum.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=release.env
. "$HERE/release.env"
OUT=${1:?OUTDIR}
VERSION=${2:-dev}
GOOS=$(go env GOOS) GOARCH=$(go env GOARCH)
case "$GOOS" in linux) ext_os=linux ;; darwin) ext_os=osx ;; *) echo "release.sh: $GOOS is not a release platform" >&2; exit 1 ;; esac
platform=${ext_os}_$GOARCH
var=HTTPFS_SHA256_$platform
want=${!var:?$var missing in release.env}
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -c1-64; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl -sSfL -o "$work/httpfs.gz" "https://extensions.duckdb.org/$DUCKDB_VERSION/$platform/httpfs.duckdb_extension.gz"
got=$(sha256 "$work/httpfs.gz")
[ "$got" = "$want" ] || { echo "release.sh: httpfs $platform sha256 $got, release.env pins $want" >&2; exit 1; }
mkdir -p "$HERE/httpfs"
gunzip -c "$work/httpfs.gz" > "$HERE/httpfs/httpfs.duckdb_extension"

mkdir -p "$OUT"
bin=$OUT/obs-$GOOS-$GOARCH
(cd "$HERE" && CGO_ENABLED=1 go build -tags httpfs_embed -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" -o "$bin" .)
rm -f "$HERE/httpfs/httpfs.duckdb_extension"
(cd "$OUT" && echo "$(sha256 "${bin##*/}")  ${bin##*/}" > "${bin##*/}.sha256")
echo "$bin"
