# blobs-compactor: build, image and deployment

> The design for this iteration, final as of 2026-09-25.
> One Go binary with DuckDB embedded, built
> reproducibly by one script that runs the same on an Ubuntu host, a Mac
> with Docker, and GitHub Actions. This document is what the compactor
> computes, how it is built, shipped and run, and how the fold uses the SQL
> in `sql/`. Built so far: the build (`build.sh`,
> the pins, CI), `selftest`, and the embedded SQL with the tests of the
> engine behaviour the fold relies on. To build: `fold`, `rollup`, the
> loop, `deploy/base`.

## Decisions

| | |
|---|---|
| Engine | DuckDB **v1.5.6** (latest, 2026-09-28) embedded via `github.com/duckdb/duckdb-go/v2` **v2.10506.0**, which links prebuilt static libraries from `duckdb-go-bindings` (version-locked by go.sum). DuckDB itself is never compiled here |
| Go | `go 1.27.0`, `toolchain go1.27.1` in `compactor/go.mod` |
| C toolchain | the Debian gcc 14.2 inside a digest-pinned `golang:1.27.1-trixie`; one native toolchain per target arch, run under emulation for the foreign arch. No cross compilers, nothing from the host |
| Host requirements | bash and docker. No Go, contain, crane, jq or gcc on the host |
| Runtime base | `gcr.io/distroless/cc-debian13:nonroot`, digest-pinned; same Debian release as the builder, so glibc matches |
| httpfs | not in the static libraries (they carry json, parquet, icu, core_functions); a signed `httpfs.duckdb_extension` per platform, sha256-pinned, baked into the image, `LOAD`ed by path with autoinstall off |
| Platforms | linux/amd64 and linux/arm64, one index digest |
| Image | `ghcr.io/yolean/blobs-compactor:<UTC-seconds tag>@<digest>`, pinned in `deploy/release/kustomization.yaml` |

## Spike results (2026-09-25, on this Mac with Docker 29.8)

A minimal `database/sql` program on duckdb-go v2.10505.0 showed:

- **Byte-identical binaries from different checkout paths.** Built twice
  in `golang:1.27.1-trixie` with `CGO_ENABLED=1 go build -trimpath
  -buildvcs=false -ldflags="-s -w"`, mounted at `/a/src` and at
  `/elsewhere/deeper/src2`. arm64 (native): both `b842573b...`. amd64
  (emulated): both `37e4b553...`, 47 to 71 s per cold build. The cgo link
  is reproducible given the pinned builder; `-trimpath` covers the path.
- **Linkage** is `libstdc++.so.6`, `libm`, `libgcc_s`, `libc`. It runs on
  `distroless/cc-debian13`, and fails on `distroless/base-debian13`
  (no libstdc++).
- **The statically embedded engine loads the httpfs extension from a
  file.** With `--read-only` on distroless/cc: `LOAD
  '/out/httpfs.duckdb_extension'` succeeds, `duckdb_extensions()` reports
  it loaded, and an HTTPS parquet read works (7,433,139 rows), so the CA
  bundle is found too. This was the biggest risk, because a loadable
  extension resolves DuckDB symbols from the host process.
- Binary 55 MB (arm64), httpfs 20 MB.
- Running the amd64 binary under emulation on an arm64 Mac is impractical:
  a remote read hung for over 14 min. Emulation is only ever used to
  *build*. Images are exercised on their native arch.

Not yet shown: that an Ubuntu amd64 host and GHA produce the same digests
as this Mac. That is acceptance check 1 below.

## The build script

`compactor/build.sh <command>` is the only entry point, and it is identical
locally and in CI:

| command | does |
|---|---|
| `test` | `go vet` and `go test ./...` in the builder, host arch. Tests run the real embedded engine over `.jsonl.zst` fixtures they write to a temp dir; no `testdata/`, no bucket |
| `image [oci-dir]` | for each of `PLATFORMS` (default: the release platforms) builds the binary, fetches and verifies httpfs, runs `selftest` natively, assembles the OCI layout with contain, prints the index digest |
| `check` | `image`, then fails unless the digest equals the release pin, printing `build.sh bump` as the remedy |
| `bump` | `image`; if the digest differs from the pin, mint a UTC-seconds tag and rewrite the pin; if equal, no-op |
| `publish` | `check`, then push the OCI layout to `ghcr.io/yolean/blobs-compactor:<pinned tag>` with credentials from `REGISTRY_USER`/`REGISTRY_TOKEN` |
| `host-setup` | registers qemu binfmt for the foreign arch (`docker run --privileged` of a digest-pinned `tonistiigi/binfmt`); once per Ubuntu host, once per CI job, not needed with Docker Desktop. `image` detects a missing handler and names this command |

How every step runs:

```
docker run --rm --platform linux/<arch> --user "$(id -u):$(id -g)" \
  -v "$REPO:/repo" -w /repo/compactor \
  -v blobs-compactor-cache:/cache \
  -e HOME=/tmp -e GOCACHE=/cache/go-build/<arch> -e GOMODCACHE=/cache/gomod \
  -e GOTOOLCHAIN=<from go.mod> -e GOFLAGS=-mod=readonly -e CGO_ENABLED=1 \
  "$BUILDER" go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o /repo/target/compactor/app/linux/<arch>/blobs-compactor .
```

- **Pins** live in `compactor/build.env`: `BUILDER` (golang:1.27.1-trixie
  index digest, today `sha256:433790e5...`), `CONTAIN=v1.1.0`,
  `CRANE=v0.22.1`, `BINFMT` digest, `DUCKDB_VERSION=v1.5.6` and
  `HTTPFS_SHA256_linux_amd64` / `_linux_arm64`. The runtime base digest
  is in `contain.yaml`, and the DuckDB engine is in `go.sum`.
- **GOTOOLCHAIN comes from go.mod** (the toolchain line, else `go` + the go
  line). When the builder's Go already
  is that release, nothing is downloaded. When someone bumps one without
  the other, the build is still correct: the declared toolchain is
  fetched and checksum-verified. The script warns, because every cold run
  then pays the download. The script reads go.mod with sed on the host,
  so the host needs no Go.
- **contain and crane run in the builder** as `go run
  github.com/turbokube/contain/cmd/contain@$CONTAIN` and `go run
  github.com/google/go-containerregistry/cmd/crane@$CRANE`, both
  sumdb-verified. So they are the same versions on every machine, with no
  setup action and no host install. contain runs on the host arch and
  only assembles layers.
- **httpfs** is fetched inside the builder with curl from
  `extensions.duckdb.org/$DUCKDB_VERSION/linux_<arch>/`, sha256-checked
  before gunzip. `selftest` fails if the extension does not match the
  embedded engine, so a DuckDB bump without new hashes cannot ship.
- **Caches** live in the docker volume `blobs-compactor-cache`, chowned
  to the invoking uid on every run. They speed up warm runs and never
  affect output. They can't be a bind mount: on Docker Desktop, Go's
  module extraction into a host directory lost files (seen 2026-09-25:
  `undefined: StackFrame` in go-errors). CI runs cold, with no cache
  action, which is the stronger reproducibility test anyway.
- **`selftest`** runs the host-arch binary inside the pinned base image
  with `--read-only` and a tmpfs `/tmp`. It loads httpfs and round-trips
  synthetic lines through JSON with zstd to parquet in `/tmp`, which
  covers binary, base and extension. It does not run the embedded `sql/`;
  `build.sh test` does. contain's assembly is deterministic and needs no
  runtime check.
- **The itest** (`test.sh`) calls `PLATFORMS=linux/<host arch>
  compactor/build.sh image target/images/compactor-oci` and loads the
  result with `y-cluster images load`. That is arm64 on this Mac,
  amd64 on Ubuntu and GHA. The pin check always builds both.

The image digest is a function of: go.mod/go.sum and the source, the
builder digest, `build.env`'s httpfs hashes, and the base digest. The tag
is not baked into the binary, so `bump` only moves the pin when content
moves. The binary logs `version()` and its module versions from
`debug.ReadBuildInfo()` at startup to identify itself.

**Where the claim stops.** The reproducibility covers our own bytes. The
engine is DuckDB's prebuilt static library, trusted as pinned by `go.sum`.
httpfs is DuckDB's signed build, trusted as pinned by `build.env`'s
hashes, and the embedded engine also checks its signature. We don't
rebuild either from source.

## Layout

```
compactor/
  go.mod go.sum
  main.go            open (the engine, httpfs), version; to come: run, fold, rollup
  selftest.go        selftest
  sqlfiles.go        go:embed of sql/, setRawFiles, loadSQL
  sql/raw.sql        the raw line contract: view raw over the variable raw_files
  sql/tables.sql     views logs, metrics, podindex over raw: the rows each table gets
  sql/pods.sql       view pods over logs, metrics, podindex: the index
  *_test.go          the fold's engine behaviour on fixtures the tests write
  build.env build.sh contain.yaml
  compact.sh         the shell prototype of one hour, over the same sql/
  deploy/release/    the images: pin; deploy/base/ and resources: [../base] to come
```

`compact.sh` is deleted once `blobs-compactor fold` writes the same
objects. `local/blobs.sh` and `test.sh` read `sql/raw.sql` and
`sql/tables.sql` (and `local/hourly.sql` for the hourly tier), so every
consumer and the compactor share one column contract and one definition
of each table's rows.

```yaml
# contain.yaml
base: gcr.io/distroless/cc-debian13:nonroot@sha256:<pinned>
# no platforms: build.sh always sets PLATFORMS
layers:
- localFile:        # first: changes on DuckDB bumps only
    pathPerPlatform:
      linux/amd64: ../target/compactor/duckdb/linux/amd64/httpfs.duckdb_extension
      linux/arm64: ../target/compactor/duckdb/linux/arm64/httpfs.duckdb_extension
    containerPath: /usr/local/lib/duckdb/httpfs.duckdb_extension
  layerAttributes: {uid: 0, gid: 0, mode: 0644}
- localFile:
    pathPerPlatform:
      linux/amd64: ../target/compactor/app/linux/amd64/blobs-compactor
      linux/arm64: ../target/compactor/app/linux/arm64/blobs-compactor
    containerPath: /usr/local/bin/blobs-compactor
  layerAttributes: {uid: 65532, gid: 65534, mode: 0755}
entrypoint: [/usr/local/bin/blobs-compactor]   # args: [run] in deploy/base
```

## What embedding buys at runtime

- Per-job timeouts are `context` deadlines that interrupt the running
  query in-process. No child process to kill.
- Row counts, `glob()` listings and the folded-key sets come back as typed
  Go values. The incremental logic (diff, decide, write) is Go around SQL
  statements, not text parsing of CLI output.
- The S3 secret is built in-process from env and never leaves memory.
- SQL is unit-tested in `go test` against the same engine that ships.

## Compaction

What a fold and the rollup compute, and the tier layout. The decisions
this design rests on:

1. **The hourly tier is one object per table per hour**,
   `hourly/{cluster}/{table}/{date}/{HH}.parquet`, for `logs`, `metrics`,
   `podindex` and `pods`. Current and past hours use the same layout, so
   there is no layout switch and no deletes. Sorting and row groups replace
   partitioning (logs: `namespace, pod, pod_uid, container, restart, time`;
   metrics: `namespace, name, pod, pod_uid, container, restart, time`; 20k-row row
   groups, which DuckDB rounds to 20,480). The daily tier is partitioned by
   namespace.
2. **Folded keys live in a sidecar** written last as the commit marker:
   `hourly/{cluster}/_folded/{date}/{HH}.csv` and
   `daily/{cluster}/_folded/{date}.csv.zst`. Every write is a `COPY`,
   which is what keeps the binary free of an S3 client.
3. **Tiered cadence and hour-first keys**
   (`raw1/{cluster}/{date}/{HH}/{node}/{MMSS}-{uuid}.jsonl.zst`). The
   current hour is folded every 2 min; once an hour, every other hour with
   raw objects, found by one glob of `raw1/{cluster}/` (lifecycle ages an
   object from its upload, so late hours are not bounded by retention).
   DuckDB's S3 glob LISTs the prefix up to the first wildcard, which is why
   the hour comes first.
4. **The first fold after H+1 plus grace is a full read** of the hour's
   raw. It repairs incremental-fold bugs, partial writes, and an
   overlapping writer: `Recreate` prevents one during rollouts, not when a
   node is unreachable and the ReplicaSet replaces a pod marked for
   deletion.

### The SQL, and the engine behaviour it relies on

The fold runs the files in `sql/`, embedded, on the engine that
`build.env` pins, which is also the CLI version the itest runs them on.
Three properties of DuckDB 1.5.5 carry the design, and
`sqlfiles_test.go` asserts each, so a DuckDB bump that changes one fails
`build.sh test`:

- **The raw view reads `raw_files` when queried.** A glob or a list of
  keys; a fold sets it to the keys it folds (`setRawFiles`).
- **A temp table takes precedence over a view of the same name**, and
  `CREATE TEMP TABLE x AS SELECT ... FROM x` reads the view, because the
  temp table exists only once the statement completes. That lets the one
  definition in `tables.sql` and `pods.sql` run over materialised rows.
- **`main.x` finds the temp table too** (temp tables live in the temp
  catalog's schema `main`). Nothing may qualify a name to mean "the view".

Session variables and temp tables belong to a connection, so each fold
runs on its own `*sql.Conn` and closes it; the temp tables go with it.

### A fold of one hour

1. **Keys.** `glob('raw1/{cluster}/{date}/{HH}/*/*.jsonl.zst')` minus the
   hour's sidecar. No files, or no sidecar, is zero keys, not an error.
   No new keys: done, nothing written. The full read at H+1 plus grace
   takes every key and skips the union in step 4.
2. **Views.** `setRawFiles(new keys)`, `loadSQL`.
3. **Read once.** `CREATE TEMP TABLE raw AS SELECT * FROM raw`: the new
   objects are read once, and the table views now read this table.
4. **The tier's rows.** For each of `logs`, `metrics`, `podindex`:
   `CREATE TEMP TABLE <t> AS SELECT DISTINCT * FROM (SELECT * EXCLUDE
   (filename) FROM <t> UNION ALL BY NAME SELECT * FROM
   read_parquet('<the hour's existing <t> object>'))`. The existing object
   is read into memory here, before anything overwrites it. `DISTINCT`
   folds retried uploads and keys folded twice.
5. **Write.** `COPY` `logs`, `metrics` and `podindex` with their sort
   orders, then `pods` (`pods.sql` now reads the three temp tables, so it
   counts deduplicated rows), each to its hourly key. The keys are the only
   SQL text Go renders, because `COPY ... TO` takes a literal.
6. **Commit.** `COPY` the sidecar, the previous keys plus the new ones,
   last.

`compact.sh` is steps 2, 3 and 5 over a whole hour, and writes the same
objects.

### The rollup of one day

The same pattern at day scale, with hourly objects as input instead of
raw, so it uses `pods.sql` but not `raw.sql` or `tables.sql`. The
changed hours are those whose sidecar has keys the daily sidecar lacks.
Their `logs`, `metrics` and `podindex` objects, united with the day's
existing objects of the namespaces they hold, become the temp tables;
`pods` is recomputed for those namespaces, the other namespaces' `pods`
rows are kept, and the daily sidecar is written last.

Further rules:

- **Request rate is a job setting.** Hetzner limits a bucket to 750
  requests/s, and a fold over ~2,500 raw objects with DuckDB's default
  thread count can burst past it. `threads` and `http_retries` are set
  per job.
- **No ordering between lifecycle rules.** A sidecar that expires while
  raw objects of its hour remain makes the next pass fold those again,
  into a new hourly object. The rollup's `DISTINCT` removes the repeat.
- **`pods` is always recomputed** from the tier's own tables, never
  merged from earlier `pods` rows, in both tiers.

State lives only in the bucket, so every start, whether an upgrade, a
drain or an OOM kill, is an ordinary tick.

## Manifests

`deploy/base/deployment.yaml`:

- `replicas: 1`, `strategy: Recreate`. A single writer is a correctness
  requirement for incremental folding.
- `automountServiceAccountToken: false`. Non-root 65532/65534,
  `readOnlyRootFilesystem`, drop ALL, seccomp RuntimeDefault.
- `envFrom` secret `blobs-compactor-s3`. It needs read on `raw1/`, and
  read and write on `hourly/` and `daily/`, no delete.
- env `CLUSTER_NAME` (as the agent names it), `S3_BUCKET`, `S3_ENDPOINT` (scheme stripped),
  `S3_REGION`, `S3_TLS`, `ROLLING_INTERVAL` (2m).
- `DUCKDB_MEMORY_LIMIT` from `resourceFieldRef: limits.memory`, with
  DuckDB given 75% of it. `/scratch` is an emptyDir for `temp_directory`.
  Starting values: requests 250m/1Gi, limit 4Gi, scratch 10Gi.
- `prometheus.io/scrape` annotation for `/metrics`
  (`blobs_compactor_last_success_timestamp_seconds{job}`, durations, rows,
  written with stdlib text exposition). The node agent scrapes it into the
  blobs, where alerting sees it. The endpoint exists for this rule, which
  goes in with the rules that phase 3 wires into the tailer:
  `time() - blobs_compactor_last_success_timestamp_seconds{job="rolling"} > 600`.

`deploy/release/kustomization.yaml`: `resources: [../base]`, and `images:`
with the tag and digest, vendored downstream as buckety-controller's is.

`deploy/` describes only the in-cluster shape. Reads are free only in
the bucket's region. On Hetzner that means `eu-central`, which may be
outside the cluster, so the same image runs on a VM there, under docker
or systemd, with the same env.

## CI

`.github/workflows/ci.yaml` has a job that only calls the script:

```yaml
compactor:
  runs-on: ubuntu-latest
  steps:
  - uses: actions/checkout@v7
  - run: compactor/build.sh host-setup
  - run: compactor/build.sh test
  - run: compactor/build.sh check
  - if: github.ref == 'refs/heads/main' && github.event_name == 'push' && github.repository_owner == 'Yolean'
    env: {REGISTRY_USER: "${{ github.actor }}", REGISTRY_TOKEN: "${{ secrets.GITHUB_TOKEN }}"}
    run: compactor/build.sh publish
```

It has no setup-go, no setup-contain and no cache actions, so what CI runs
is exactly what a developer runs. The publish step rebuilds and asserts
before pushing, so the pushed digest is the pinned one. The job needs
`packages: write`, which the workflow already grants.

## Acceptance checks

1. `build.sh check` passes against one committed pin on an arm64 Mac
   (done 2026-09-25), in GHA on `ubuntu-latest` (done, PR #9 on the
   agents remote: amd64 native, arm64 emulated, the Mac's direction
   reversed), and on an Ubuntu amd64 host (to do).
2. A cold `check` (empty cache volume, another checkout path) reproduces
   the pin: done on the Mac; every GHA run is cold.
3. `selftest` passes on both arches on native hosts: arm64 on the Mac,
   amd64 in GHA's `check`.
4. The itest's phase 2 assertions pass with the image loaded into
   y-cluster: after `fold` exists.

## Open

- The earlier object-storage cost model assumed one LIST
  per node-hour for DuckDB reads; it needs a correction.
- The node-agent and blobs-to-prometheus images keep their Dockerfile
  builds. blobs-to-prometheus could move to this script's pattern later.
