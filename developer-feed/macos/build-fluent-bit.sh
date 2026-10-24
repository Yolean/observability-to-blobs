#!/usr/bin/env bash
# Builds the fork's fluent-bit for macOS, natively: upstream at the
# Dockerfile's FLB_VERSION with images/fluentbit/patches/ applied, and the
# host build's cmake flags (images/fluentbit/host/build.sh) with what macOS
# has not: no systemd input, no jemalloc. Leaves the binary in the output
# directory; developer-feed/macos/install.sh installs it.
#
#   developer-feed/macos/build-fluent-bit.sh [output dir]    default target/macos
#
# Needs the Xcode command line tools and, from Homebrew:
#   brew install cmake pkg-config bison flex openssl@3 libyaml
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
out=${1:-$repo/target/macos}
mkdir -p "$out"
out=$(cd "$out" && pwd)

[ "$(uname -s)" = Darwin ] || { echo "build-fluent-bit.sh: macOS only" >&2; exit 2; }
command -v brew >/dev/null || { echo "build-fluent-bit.sh: needs Homebrew" >&2; exit 2; }
missing=
for f in cmake pkg-config bison flex openssl@3 libyaml; do
  brew --prefix "$f" >/dev/null 2>&1 && [ -d "$(brew --prefix "$f")" ] || missing="$missing $f"
done
[ -z "$missing" ] || { echo "build-fluent-bit.sh: brew install$missing" >&2; exit 2; }

version=$(sed -n 's/^ARG FLB_VERSION=//p' "$repo/images/fluentbit/Dockerfile")
src=$out/fluent-bit-$version
if [ ! -d "$src/.git" ]; then
  git clone -q --depth 1 --branch "$version" https://github.com/fluent/fluent-bit.git "$src"
fi
# the patches on a clean tree at the tag, every time
git -C "$src" checkout -q -f "$version"
git -C "$src" clean -q -fdx -e build
for p in "$repo"/images/fluentbit/patches/*.patch; do
  git -C "$src" apply "$p"
done

mkdir -p "$src/build"
cd "$src/build"
PATH="$(brew --prefix bison)/bin:$(brew --prefix flex)/bin:$PATH" cmake \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_PREFIX_PATH="$(brew --prefix openssl@3);$(brew --prefix libyaml)" \
  -DOPENSSL_ROOT_DIR="$(brew --prefix openssl@3)" \
  -DFLB_RELEASE=On \
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
  .. >"$out/cmake.log"
PATH="$(brew --prefix bison)/bin:$(brew --prefix flex)/bin:$PATH" make -j"$(sysctl -n hw.ncpu)" >"$out/make.log"
install -m 0755 bin/fluent-bit "$out/fluent-bit"
strip -x "$out/fluent-bit"
"$out/fluent-bit" --version | head -1
echo "built: $out/fluent-bit"
