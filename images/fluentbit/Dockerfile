# Fluent Bit fork for the node agent. Layout:
#   build-deps  toolchain + Go (shared by dev loop and CI)
#   build       upstream at FLB_VERSION compiled, then patches/ applied and recompiled
#   goplugins   Go input plugins (cgo shared objects loaded by fluent-bit's proxy)
#   runtime     distroless, nonroot, only the shared libs ldd says we need
FROM debian:trixie@sha256:34cd9e9fd437c0a095ec39cb2e73422c9f30821b0d0848ed74fd0d43bae4d958 AS build-deps

RUN set -ex; \
  export DEBIAN_FRONTEND=noninteractive; \
  apt-get update; \
  apt-get install -y --no-install-recommends \
    build-essential cmake curl ca-certificates git make pkg-config \
    libssl-dev libcurl4-openssl-dev zlib1g-dev libyaml-dev libsystemd-dev \
    flex bison libsasl2-dev; \
  rm -rf /var/lib/apt/lists/*

COPY --from=golang:1.26-trixie@sha256:b389f1219965d8ba67776b81d17308ab25fa763be3855e5fe63ebcb10e15f3a1 /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH

# Upstream is configured and compiled before the patches are copied in, so
# a patch change costs an incremental make, not the 15 minute full build.
FROM build-deps AS build
ARG FLB_VERSION=v5.1.2
ARG FLB_TRACE=Off
ARG FLB_HTTP_CLIENT_DEBUG=Off
RUN git clone --depth 1 --branch $FLB_VERSION https://github.com/fluent/fluent-bit.git /src/fluent-bit
WORKDIR /src/fluent-bit/build
RUN cmake \
    -DFLB_RELEASE=On \
    -DFLB_TRACE=${FLB_TRACE} \
    -DFLB_HTTP_CLIENT_DEBUG=${FLB_HTTP_CLIENT_DEBUG} \
    -DFLB_SIMD=On \
    -DFLB_JEMALLOC=On \
    -DFLB_TLS=On \
    -DFLB_SHARED_LIB=Off \
    -DFLB_EXAMPLES=Off \
    -DFLB_HTTP_SERVER=On \
    -DFLB_IN_SYSTEMD=On \
    -DFLB_IN_EXEC=Off \
    -DFLB_KAFKA=On \
    -DFLB_OUT_PGSQL=Off \
    -DFLB_ARROW=Off \
    -DFLB_WASM=Off \
    -DFLB_LUAJIT=Off \
    -DFLB_PROXY_GO=On \
    -DFLB_LOG_NO_CONTROL_CHARS=On \
    .. \
  && make -j"$(nproc)"

COPY patches/ /src/patches/
# update-index --refresh: when the clone layer is imported from a CI cache
# the mtimes are normalized, git considers every tracked file stat-dirty,
# and `git apply --index` would refuse with "does not match index"
RUN cd /src/fluent-bit \
  && (git update-index --refresh >/dev/null 2>&1 || true) \
  && for p in /src/patches/*.patch; do echo "applying $p"; git apply "$p" || exit 1; done \
  && cd build && make -j"$(nproc)" \
  && bin/fluent-bit --version

FROM build-deps AS goplugins
COPY plugins/ /src/plugins/
RUN set -ex; mkdir -p /out; \
  for d in /src/plugins/*/; do \
    name=$(basename "$d"); \
    (cd "$d" && CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -o "/out/in_${name}.so" .); \
  done; ls -la /out

# Collect runtime shared libs via ldd; /lib is a symlink to /usr/lib in distroless.
FROM build AS runtime-libs
COPY --from=goplugins /out/ /out/
RUN set -ex; \
  PKG_PREFIX="$(uname -m)-linux-gnu"; \
  LIBDIR=/runtime-libs/usr/lib/$PKG_PREFIX; \
  mkdir -p "$LIBDIR"; \
  for bin in /src/fluent-bit/build/bin/fluent-bit /out/*.so; do \
    ldd "$bin" | awk '/=>/ {print $3}' | sort -u | while read -r lib; do \
      [ -f "$lib" ] || continue; \
      cp -dav "$lib" "$LIBDIR"/; \
      real=$(readlink -f "$lib"); \
      [ "$real" != "$lib" ] && cp -dav "$real" "$LIBDIR"/; \
    done; \
  done; \
  ls "$LIBDIR"

FROM gcr.io/distroless/cc-debian13:nonroot@sha256:d97bc0a941b8d4be647dc0ee75b264ddbb772f1ac5ba690a4309c00723b23775

COPY --from=build /src/fluent-bit/build/bin/fluent-bit /usr/local/bin/fluent-bit
COPY --from=build /src/fluent-bit/conf/parsers.conf /fluent-bit/etc/parsers.conf
COPY --from=goplugins /out/ /fluent-bit/lib/
COPY --from=runtime-libs /runtime-libs/ /

EXPOSE 2020
ENTRYPOINT ["/usr/local/bin/fluent-bit"]
