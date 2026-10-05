# Fluent Bit fork

`Dockerfile` clones upstream at `FLB_VERSION`, compiles it, then applies
`patches/*.patch` in order and compiles again, so a patch change costs an
incremental make (seconds) instead of the full build (minutes cold).
`plugins/*` are Go input plugins built as cgo shared objects and loaded
through fluent-bit's proxy; the runtime image is distroless with the shared
libraries `ldd` reports.

The fork carries one patch, `0001-http-client-match-header-at-line-start`:
GCS sends `x-goog-stored-content-length` before `Content-Length` and
upstream's `strcasestr` header lookup matches the wrong one. Everything else
the agent needs is stock `out_s3` (`format json_lines`, `compression zstd`,
`upload_timeout`, `$UUID` keys). `UPSTREAM.md` lists what should go back
upstream.

## Host mode

The same Fluent Bit also runs as a systemd service on a host outside the
cluster (`../../HOST_MODE.md`). `host/` holds what that service is made of
(`host.yaml`, the heartbeat script, the unit) and `host/build.sh` builds the
release tarball: the Dockerfile's `build` stage again, on `ubuntu:24.04` and
with `FLB_CMAKE_EXTRA` adding the `exec` input and leaving out Kafka and the
Go plugin loader, then `host-package` and `host-tarball`. `host/test-heartbeat.sh`
tests the heartbeat script alone; `test.sh checks` runs it.

## Working on the patch

The patch file is generated, not edited. Keep it as a commit on a branch of
an upstream clone and diff it out:

```bash
git clone --depth 1 --branch v5.1.2 https://github.com/fluent/fluent-bit.git
cd fluent-bit && git checkout -b node-agent
git apply --index ../images/fluentbit/patches/0001-*.patch && git commit -q -m 0001-http-client-match-header-at-line-start
# edit, commit --amend, then:
git diff v5.1.2 node-agent > ../images/fluentbit/patches/0001-http-client-match-header-at-line-start.patch
```

Build once in a container made from the `build-deps` stage, with the clone
mounted, and iterate with `make` in the build directory:

```bash
docker build --target build-deps -t flb-build-deps images/fluentbit
docker run -d --name flb-dev -u "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$PWD/fluent-bit:/src/fluent-bit" flb-build-deps sleep infinity
docker exec -w /src/fluent-bit flb-dev sh -c 'mkdir -p build && cd build && cmake <same flags as the Dockerfile> .. && make -j"$(nproc)"'
```

A local versitygw (`docker run -p 7070:7070 ... versity/versitygw posix /data`)
and a `dummy` input are enough to exercise out_s3 config without a cluster.
