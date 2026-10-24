#!/usr/bin/env bash
# Builds everything the user-level host agent runs on this Mac into
# target/macos/ (developer-feed/macos/README.md), no arguments:
#
#   developer-feed/macos/build.sh
#
#   bin/fluent-bit    the fork's fluent-bit, natively: upstream at the
#                     Dockerfile's FLB_VERSION with images/fluentbit/patches/
#                     and this build's own patches/, the host build's cmake
#                     flags without what macOS has not
#   bin/dev-feed-r2   the R2 credential tool (developer-feed/cloudflare/)
#   bin/dev-feed-git  the git collector's command (developer-feed/git/)
#   BUILD             the commit it was built from, and the versions
#
# install.sh installs from there. Incremental: a second run only rebuilds
# what changed. Needs the Xcode command line tools, Go, and from Homebrew:
#   brew install cmake pkg-config bison flex openssl@3 libyaml
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
out=$repo/target/macos
mkdir -p "$out/bin"

[ "$(uname -s)" = Darwin ] || { echo "build.sh: macOS only" >&2; exit 2; }
command -v brew >/dev/null || { echo "build.sh: needs Homebrew" >&2; exit 2; }
command -v go >/dev/null || { echo "build.sh: needs Go" >&2; exit 2; }
missing=
for f in cmake pkg-config bison flex openssl@3 libyaml; do
  brew --prefix "$f" >/dev/null 2>&1 && [ -d "$(brew --prefix "$f")" ] || missing="$missing $f"
done
[ -z "$missing" ] || { echo "build.sh: brew install$missing" >&2; exit 2; }

commit=$(git -C "$repo" rev-parse HEAD)
dirty=$(git -C "$repo" status --porcelain | grep -q . && echo yes || echo no)
echo "== building from $commit (uncommitted changes: $dirty)"

echo "== dev-feed-r2, dev-feed-git, dev-feed-backfill"
(cd "$repo/developer-feed/cloudflare" && go build -trimpath -o "$out/bin/dev-feed-r2" .)
(cd "$repo/developer-feed/git" && go build -trimpath -o "$out/bin/dev-feed-git" .)
(cd "$repo/developer-feed/backfill" && go build -trimpath -o "$out/bin/dev-feed-backfill" .)

version=$(sed -n 's/^ARG FLB_VERSION=//p' "$repo/images/fluentbit/Dockerfile")
src=$out/fluent-bit-$version
echo "== fluent-bit $version with images/fluentbit/patches/"
if [ ! -d "$src/.git" ]; then
  git clone -q --depth 1 --branch "$version" https://github.com/fluent/fluent-bit.git "$src"
fi
# the patches on the tag's tree, every time; build/ is kept, so that make
# only recompiles what the patches changed
git -C "$src" checkout -q -f "$version"
git -C "$src" clean -q -fdx -e build
# the fork's patches, then the Mac build's own (developer-feed/macos/patches/:
# what only this build needs, such as credential_process on Darwin)
for p in "$repo"/images/fluentbit/patches/*.patch "$here"/patches/*.patch; do
  git -C "$src" apply "$p"
done

mkdir -p "$src/build"
cd "$src/build"
PATH="$(brew --prefix bison)/bin:$(brew --prefix flex)/bin:$PATH"
export PATH
# FLB_SECURITY off: with FLB_RELEASE it adds GNU ld's -z relro, -z now and
# -z noexecstack whatever the platform, which Apple's ld refuses (every
# cmake check then fails to link, and Threads is "not found"); the
# hardening that applies here is in CMAKE_C_FLAGS instead.
# HAVE_PIPE2 off: c-ares finds pipe2 in the SDK, which marks it available
# from macOS 27 only; clang refuses it below that, and the binary would not
# start on the macOS it was built on. c-ares falls back to pipe().
cmake \
  -DHAVE_PIPE2=0 \
  -DCMAKE_BUILD_TYPE=RelWithDebInfo \
  -DCMAKE_C_FLAGS="-fstack-protector-strong -D_FORTIFY_SOURCE=2" \
  -DCMAKE_PREFIX_PATH="$(brew --prefix openssl@3);$(brew --prefix libyaml)" \
  -DOPENSSL_ROOT_DIR="$(brew --prefix openssl@3)" \
  -DFLB_RELEASE=On \
  -DFLB_SECURITY=Off \
  -DFLB_SIMD=On \
  -DFLB_JEMALLOC=Off \
  -DFLB_TLS=On \
  -DFLB_SHARED_LIB=Off \
  -DFLB_EXAMPLES=Off \
  -DFLB_HTTP_SERVER=On \
  -DFLB_IN_SYSTEMD=Off \
  -DFLB_IN_EXEC=On \
  -DFLB_KAFKA=Off \
  -DFLB_OUT_PGSQL=Off \
  -DFLB_ARROW=Off \
  -DFLB_WASM=Off \
  -DFLB_LUAJIT=Off \
  -DFLB_PROXY_GO=Off \
  -DFLB_LOG_NO_CONTROL_CHARS=On \
  .. 2>&1 | tee "$out/cmake.log"
make -j"$(sysctl -n hw.ncpu)" 2>&1 | tee "$out/make.log"
# with its debug info, not stripped, and the .dSYM beside it, where macOS
# keeps debug info: fluent-bit prints a crash's backtrace only from that (a
# SIGSEGV on a stop under launchd is not yet understood)
install -m 0755 bin/fluent-bit "$out/bin/fluent-bit"
rm -rf "$out/bin/fluent-bit.dSYM"
dsymutil "$out/bin/fluent-bit" -o "$out/bin/fluent-bit.dSYM"

{
  echo "commit=$commit"
  echo "dirty=$dirty"
  echo "built=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "fluent_bit=$("$out/bin/fluent-bit" --version | head -1)"
  echo "go=$(go env GOVERSION)"
} >"$out/BUILD"
echo "== built into $out"
cat "$out/BUILD"
