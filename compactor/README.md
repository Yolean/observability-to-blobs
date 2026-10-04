# blobs-compactor: build, image and deployment

> The design of this iteration, final as of 2026-09-25,
> and what is built from it. One Go binary with
> DuckDB embedded, built reproducibly by one script that runs the same on
> an Ubuntu host, a Mac with Docker, and GitHub Actions. This document is
> what the compactor computes, how it is built, shipped and run, and how
> the fold uses the SQL in `sql/`. Built: the build (`build.sh`, the pins,
> CI), `selftest`, the embedded SQL with the tests of the engine behaviour
> the fold relies on, `fold`, `rollup`, the loop (`run`) with optional raw
> deletion and `/metrics`, optional Parquet encryption of the tiers, the
> put endpoint and spool mode, `deploy/base` with the release pin and
> `deploy/spool`. The itest runs it in a cluster, encrypted, and a second
> one in spool mode fed by stock Fluent Bit (root `README.md`).

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

## Spike results (2026-09-25, on an arm64 Mac with Docker 29.8)

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

Not yet shown: that an Ubuntu amd64 host produces the same digests as
an arm64 Mac. That is acceptance check 1 below.

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
  result with `y-cluster images load`. That is arm64 on an Apple
  silicon Mac, amd64 on Ubuntu and GHA. The pin check always builds both.

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
  main.go            the commands, open (the engine, httpfs, the secret), version
  config.go          the environment
  keys.go            the key layout of the tiers
  encryption.go      parquet keys: loading, the missing-key check, which object, per-key reads
  store.go           RawSource and TierStore, and a local directory as both
  s3.go              the bucket as both: ListObjectsV2, HeadObject, CopyObject,
                     DeleteObjects, SigV4
  put.go             the put endpoint: PutObject, SigV4 verification, S3 errors
  spool.go           the spool: a RawSource on a local directory, durable writes
  folded.go          the put endpoint's cache of the hours' folded keys
  fold.go            fold of one hour, the sidecar
  rollup.go          rollup of one day
  loop.go            run: the rolling and past passes
  metrics.go         /metrics
  selftest.go        selftest
  sqlfiles.go        go:embed of sql/, setRawFiles, loadSQL
  sql/raw.sql        the raw line contract: view raw over the variable raw_files
  sql/tables.sql     views logs, metrics, podindex over raw: the rows each table gets
  sql/pods.sql       view pods over logs, metrics, podindex: the index
  *_test.go          the engine behaviour, and the fold, rollup and passes on
                     jsonl.zst fixtures the tests write to a directory store
  build.env build.sh contain.yaml
  deploy/base/       the Deployment
  deploy/release/    resources: [../base] and the images: pin
  deploy/spool/      a component: spool mode, its volume and the put Service
  deploy/spool-networkpolicy/
                     an optional component: the put port open to the agents' namespace only
```

The shell prototype `compact.sh` is gone: `blobs-compactor fold` writes
the same objects. `local/blobs.sh` and `test.sh` read `sql/raw.sql` and
`sql/tables.sql` (`local/blobs.sh` builds its hourly views itself, per
parquet key), so every
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
- Row counts and the folded-key sets come back as typed Go values. The
  incremental logic (diff, decide, write) is Go around SQL statements, not
  text parsing of CLI output.
- The S3 secret is built in-process from env and never leaves memory; an
  engine error about it is logged with the secret blanked out.
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
   `daily/{cluster}/_folded/{date}.csv.zst`. Every object write is a
   `COPY` to a temporary key, moved into place once it reads back whole
   (below, "Writes replace an object whole"). The binary's own S3 calls
   are four that DuckDB cannot make: ListObjectsV2 (keys with their upload
   times), HeadObject, CopyObject for the move, and DeleteObjects for the
   temporary objects and the optional raw deletion.
3. **Tiered cadence and hour-first keys**
   (`raw1/{cluster}/{date}/{HH}/{node}/{MMSS}-{uuid}.jsonl.zst`). The
   current hour is folded every 2 min; once an hour, every other hour with
   raw objects, found by listing `raw1/{cluster}/` hour by hour (lifecycle
   ages an object from its upload, so late hours are not bounded by
   retention). A listing pages through every key under its prefix, which
   is why the hour comes first: an hour's fold lists only that hour
   (measured with DuckDB's glob, which lists the same way).
   The past pass finds the dates and their hours with
   delimiter listings, one entry per prefix, and then lists one hour at a
   time, so the compactor holds at most one hour's keys, never the tier's.
4. **The first fold after H+1 plus grace is a full read** of the hour's
   raw. It repairs incremental-fold bugs, partial writes, and an
   overlapping writer: `Recreate` prevents one during rollouts, not when a
   node is unreachable and the ReplicaSet replaces a pod marked for
   deletion.

The tiers' keys:

```
hourly/{cluster}/{logs,metrics,podindex,pods}/{date}/{HH}.parquet
hourly/{cluster}/_folded/{date}/{HH}.csv              key,final: the raw keys folded in, written last
daily/{cluster}/{logs,metrics}/{date}/namespace={namespace}/data_0.parquet
daily/{cluster}/{podindex,pods}/{date}.parquet
daily/{cluster}/_folded/{date}.csv.zst                key: the raw keys rolled into the day, written last
{hourly,daily}/{cluster}/_tmp/{random}.tmp             a write in progress, or one a failed job left
```

With encryption on (below), a tier object's `.parquet` is `.k-{id}.parquet`,
`{id}` the parquet key it is encrypted with; sidecars are always plain.

The namespace in a daily key is path-escaped the way DuckDB's hive
partitioning writes it, and the column is in the file too, so readers can
use `hive_partitioning` to skip the other namespaces' objects or ignore
the path. Journal rows are in `_node`.

### The SQL, and the engine behaviour it relies on

The fold runs the files in `sql/`, embedded, on the engine that
`build.env` pins, which is also the CLI version the itest runs them on.
Three properties of DuckDB 1.5 carry the design, and
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
runs on its own `*sql.Conn` and closes it; the engine keeps no idle
connections, so the temp tables go with it. DuckDB's external file cache
is off: the compactor rereads objects it has just replaced.

### A fold of one hour

1. **Keys.** The listing of `raw1/{cluster}/{date}/{HH}/`, minus the
   hour's sidecar. No objects, or no sidecar, is zero keys, not an error.
   No new keys: done, nothing written.
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
   counts deduplicated rows), each to its hourly key. All four are written
   even when one is empty. The keys are the only SQL text Go renders,
   because `COPY ... TO` takes a literal.
6. **Commit.** `COPY` the sidecar, the previous keys plus the new ones,
   last.

Every `COPY` of steps 5 and 6 writes as described under "Writes replace
an object whole" below. A crash between steps 5 and 6 leaves rows of keys
the sidecar does not list; the next fold reads those keys again and `DISTINCT` folds the
repeat, the same edge an upload retry produces.

**The full read and the final hour.** The first fold at or after
H+1h+`GRACE` takes every listed key, not only the new ones, and skips the
union in step 4, so the hour's objects are rebuilt from raw alone. Its
sidecar marks those keys `final = true`, and an hour is **final** when any
row of its sidecar is: the marker is in the commit object itself, so it
commits with the fold that earns it. A final hour is never read in full
again. Keys that arrive later (a backlog shipped after an outage lands
under its first record's hour) are folded incrementally by the past pass,
with `final = false`. If some key the sidecar lists is no longer in the
listing (lifecycle expired it), or there is no sidecar while the hour's
objects exist (it expired, or the hour's first fold crashed before it),
the full read unions like an incremental fold instead of replacing: the
objects may hold rows of keys raw no longer has, and replacing would lose
them.

### Writes replace an object whole

A `COPY` that its job's deadline (`JOB_TIMEOUT`) interrupts can still
complete its multipart upload when httpfs closes the file: the key then
holds a parquet file without its footer (reproduced on versitygw). At a
tier's key that would fail every later fold and rollup that reads the
object, and with raw deletion on, the rows behind it would be gone. So no
`COPY` writes a tier's key. Each one, the hourly objects, the daily
partitions, `podindex`, `pods` and both sidecars, goes to a new key under
the tier's `_tmp/` (`hourly/{cluster}/_tmp/` or `daily/{cluster}/_tmp/`,
suffix `.tmp`, so no reader's `*.parquet` or `*.csv` glob matches it).
The object there must read back, its footer and row count through the
engine, with as many rows as the `COPY` reported; only then is it moved to
its key: a server-side CopyObject, which replaces the key whole, and a
delete of the temporary key (a rename for a local directory). A reader of
the key sees the previous object or the new one, never a part.

A failed write deletes its temporary object. What that misses, or what a
killed process leaves, the past pass deletes: every object under the
`_tmp/` prefixes uploaded more than `JOB_TIMEOUT` ago, which no running
job can own. An operator can also give those two prefixes a short
lifecycle rule (a day, say), and the bucket a rule that aborts incomplete
multipart uploads, which an interrupted `COPY` that httpfs aborts leaves.
CopyObject takes objects up to 5 GiB, which bounds one namespace's daily
`logs` or `metrics` object.

### The rollup of one day

The same pattern at day scale, with hourly objects as input instead of
raw, so it uses `pods.sql` but not `raw.sql` or `tables.sql` (except for
their empty views, which give every table its schema when an input is
missing). The changed hours are those whose sidecar has keys the daily
sidecar lacks. Their `logs`, `metrics` and `podindex` objects, united
with the day's existing objects of the namespaces they hold (and the
day's whole `podindex`), `DISTINCT`, become the temp tables. Each of
those namespaces' `logs` and `metrics` objects is rewritten; the other
namespaces' objects are not touched. `pods` is recomputed for those
namespaces from the day's tables, an aggregate per `pod_uid`, `container`,
`restart` over the whole day, never a concatenation of hourly rows; the
other namespaces' `pods` rows are kept. The daily sidecar, the union of
the day's keys, is written last. Hourly objects are not clean time
partitions (an object keyed by its first record carries records past the
hour), which is why the rollup unions rows and never replaces an hour by
a `time` filter.

Further rules:

- **Request rate is a job setting.** Hetzner limits a bucket to 750
  requests/s, and a fold over ~2,500 raw objects with DuckDB's default
  thread count can burst past it. Every job's connection sets `threads`
  (`DUCKDB_THREADS`, default 4) and `http_retries`
  (`DUCKDB_HTTP_RETRIES`, default 5).
- **No ordering between lifecycle rules.** A sidecar that expires while
  raw objects of its hour remain makes the next pass fold those again,
  into a new hourly object. The rollup's `DISTINCT` removes the repeat.
- **`pods` is always recomputed** from the tier's own tables, never
  merged from earlier `pods` rows, in both tiers.

### The loop

`blobs-compactor run`:

- every `ROLLING_INTERVAL` (default 2m): fold the current hour, and the
  previous hour until its final fold has committed. When that final fold
  is of a day's last hour, roll the day up: its first rollup, which finds
  all of its hours changed.
- every `PAST_INTERVAL` (default 1h), and on the first tick after a
  start: list `raw1/{cluster}/` hour by hour and fold every other hour that has
  keys its sidecar lacks or is past H+1h+`GRACE` and not final (so an
  hour whose final fold a downtime skipped still gets it); then check
  every past day that has hourly sidecars for a rollup, which does
  nothing unless an hour has keys the daily sidecar lacks.
- `GRACE` (default 10m) is how long after its end an hour waits for its
  last uploads before the full read.
- The configuration refuses `GRACE` above 30m, `ROLLING_INTERVAL` above
  5m and `PAST_INTERVAL` above 1h. With raw deletion on, an hour's raw
  objects go at its final fold, the first rolling tick after
  H+1h+`GRACE`, so a raw object lives at most 1h + `GRACE` +
  `ROLLING_INTERVAL` from its upload, 1h35m, plus that fold; a late key,
  one that arrives after its hour is final, goes at the next past pass,
  at most `PAST_INTERVAL` plus that pass. Both stay under 2h while a fold
  takes under 25m, and when one does not, the raw-age alert says so.
  `ROLLING_INTERVAL` at most 5m also keeps the rolling alert (600 s
  without a success) quiet between healthy passes.

State lives only in the bucket, so every start, whether an upgrade, a
drain or an OOM kill, is an ordinary tick. A SIGTERM ends a pass between
its jobs: the fold or rollup in flight finishes under its own
`JOB_TIMEOUT` (default 30m), and no other starts, so a stop takes one job
and a stopped compactor leaves a quiet bucket. A job that outlasts
`terminationGracePeriodSeconds` is killed, which is a crash like any
other: its temporary objects are swept and its keys folded again. `fold <date> <HH>` and `rollup <date>`
run one job by hand with the same environment, while no loop runs.

### Raw deletion

Off by default. With `RAW_DELETE=after-final`, once an hour's final fold
has committed, the raw keys its sidecar lists are deleted; a late key in
a final hour is deleted once the fold that took it has committed. A key
that no committed sidecar lists is never deleted, and nothing is deleted
by age. A deletion that a crash interrupted resumes at the hour's next
fold. A raw object then lives under 2h from its upload, a bound the
configuration enforces (above, "The loop"), and
`blobs_compactor_raw_oldest_age_seconds` is what an alert on that bound
reads. The secret then needs delete on
`raw1/`; lifecycle rules still bound everything else. In spool mode
(below) the raw tier is the spool, and this deletion is always on.

## Encryption

Off by default. With it on, the hourly and daily tiers' Parquet objects
are encrypted with keys the operator provides (DuckDB's Parquet
encryption: footer and columns, AES-GCM, one key per file). The sidecars
stay plain: they list raw keys, which carry cluster, date, hour, node and
upload time, and no row data. Raw objects are not touched.

**Keys.** `PARQUET_KEYS_DIR` is a directory, normally a mounted Secret,
with one file per key: the file name is the key id (`[a-z0-9][a-z0-9-]*`,
at most 32 characters), the content the standard base64 of 32 random
bytes, for example `openssl rand -base64 32`. Entries starting with a dot
(a Secret mount's own) are skipped; any other file that is not a valid
key stops the compactor. Only 32 byte keys: DuckDB decodes a key's base64
only when the text is not itself 16, 24 or 32 characters long, and the
base64 of a 16 or 24 byte key is 24 or 32 characters, so it would be taken
as the key verbatim and mean different bytes to DuckDB than to any other
reader. Encryption is on when `PARQUET_KEY_ID` is set: the id of the key
new objects get, which must have a file (the compactor will not start
otherwise). Writing encrypted Parquet needs httpfs's crypto (without it
DuckDB's is read-only), so the compactor loads httpfs whenever
`PARQUET_KEYS_DIR` is set, also with `BLOBS_DIR` and without an active key
(encrypted objects are still rewritten under their keys), and at start
writes and reads back one encrypted object in scratch. A key's content
never changes; a new key is a new id. The key check decrypts one stored
object per key id, so a key file whose content changed under the same id
refuses folds like a missing one, instead of failing every fold of its
hours. Rows the engine spills to `temp_directory` during a fold are not
encrypted: the scratch volume holds decrypted data while a job runs, so
give it the protection the node's disks have.

**Layout.** The key id is in the object's name, so that a reader can
select one key's objects with a glob, and the tier stays the first prefix
segment, so lifecycle rules keep matching literal prefixes:

```
hourly/{cluster}/{table}/{date}/{HH}.k-{id}.parquet
daily/{cluster}/{logs,metrics}/{date}/namespace={namespace}/data_0.k-{id}.parquet
daily/{cluster}/{podindex,pods}/{date}.k-{id}.parquet
```

Plain objects keep their names. A plain reader's globs are
`[0-9][0-9].parquet`, `data_0.parquet` and
`[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9].parquet`, which match no
encrypted name (`?` would do on a disk, but in an `s3://` URL it starts
the query string); one key's are `*.k-{id}.parquet`.

**Rotation without deletes.** An existing object is always rewritten
under the name, so the key, it was first written with; only new objects
(a new hour's four, a namespace's first partition of a day, a day's first
`podindex` and `pods`) get the active key. So an hour never has two
objects, a rotation needs no S3 delete, and changing `PARQUET_KEY_ID`
(add the new key's file to the Secret first, then the env, which restarts
the pod) moves the tiers to the new key as hours and days pass. Turning
encryption on for a bucket with plain objects works the same way: they
stay plain, new ones are encrypted. With a keys directory the compactor
finds an object by listing its stem (`{HH}.`, `{date}.`), one
ListObjectsV2 per table and fold where a plain compactor makes a
HeadObject; a stem with two objects fails the job rather than guess.
Turning encryption off (unset `PARQUET_KEY_ID`, keep the directory) writes
new objects plain and keeps the encrypted ones under their keys.

**Retention never outlasts a key, verified.** An old key stays in the
directory until every object under it has expired. At start, on every
past pass, and on every rolling tick while it refuses, the compactor
lists `hourly/{cluster}/` and `daily/{cluster}/` (a few pages: some
thousand keys for a month), collects the key ids in the object names
plus `PARQUET_KEY_ID`, and if any has no key file it refuses to fold or
roll up: it logs an error naming the ids, sets
`blobs_compactor_parquet_keys_missing` to their number, counts the passes
as failures (so the rolling-staleness rule fires too) and keeps serving
`/metrics`. It resumes at the first check that finds every key again,
which a Secret update reaches without a restart. A compactor with no keys
directory over encrypted objects refuses the same way, rather than write
a plain object beside an encrypted one. `fold` and `rollup` by hand check
first too. The check compares files, not the engine: keys stay loaded
when their file goes.

**Readers.** Whoever reads the tiers needs the keys: one `read_parquet`
takes one footer key, a glob over objects of two keys fails ("Computed AES
tag differs"), a keyed read of a plain object fails and so does a plain
read of an encrypted one. So a reader reads the plain objects in one
`read_parquet` and each key's in its own, and unites them with `UNION ALL
BY NAME`. `union_by_name = true` on an encrypted read is an internal
error in DuckDB 1.5.5 and 1.5.6, which also invalidates the database, so
an encrypted read never sets it: within one key's glob the first object's
columns win, and an object written before a raw1 column was added can
fail such a read. The compactor itself reads each encrypted object in a
`read_parquet` of its own, united by name, so it never meets that. The
readers built this way are the fold's union with the hour's object, the
rollup's reads of hours, partitions, `podindex` and `pods`, the read-back
of every write's temporary object (with the key its final name carries),
`local/blobs.sh` (`PARQUET_KEYS_DIR`, the same directory layout) and the
itest's assertions (`tier` in `test.sh`). `sqlfiles_test.go` pins each of
these engine facts, so a DuckDB bump that changes one fails `build.sh
test`.

The unit tests write encrypted objects without httpfs, with
`force_mbedtls_unsafe`, which the compactor never sets; `selftest`
writes and reads back encrypted Parquet with the image's httpfs, and the
itest runs the compactor encrypted with a key rotation.

## The put endpoint and spool mode

Off by default. With it on, the agents upload their raw objects to the
compactor instead of to the bucket, with Fluent Bit's stock `out_s3`, and
the compactor keeps them on a local volume, the spool, until their hour's
final fold has committed. The raw tier is then never stored in object
storage. There is one compactor and no high availability: while it is
down, `out_s3` buffers and retries, within the agent's buffer limits
(`agent/fluent-bit.yaml`).

**What the endpoint implements** is what stock Fluent Bit v5.1.3's
`out_s3` sends with `use_put_object on`, read from its source and
captured off the wire, and nothing more: a path-style `PUT
/{bucket}/{key}` (with a custom `endpoint`, `out_s3` always addresses the
bucket in the path), signed with AWS Signature Version 4 over the
payload's SHA-256, with `Content-Encoding: zstd` (stored as it comes) and,
with `send_content_md5 on`, a `Content-MD5`. It makes no other call, at
startup or around an upload. Any other method, a query string
(multipart uploads, ACLs, tagging) or `aws-chunked` is answered with S3's
XML error document, 501 `NotImplemented`. `PUT_LISTEN` (`:9000` in
`deploy/spool`) turns it on; it needs `RAW_SOURCE=spool`. `run` binds it,
and `METRICS_ADDR`, before its first pass: an address it cannot bind ends
the process at once, as does a listener that fails later.

**What it checks**, each refusal an S3 error document. Until the
signature is verified, every refusal but the anonymous one is the same
403 `SignatureDoesNotMatch` with S3's generic message, so a caller without
the key pair learns neither the key id, nor the region, nor the server's
clock; the log line says which check failed:

| | |
|---|---|
| no `Authorization` | 403 `AccessDenied` |
| not `AWS4-HMAC-SHA256`, a credential not `{id}/{date}/{region}/s3/aws4_request`, a key id other than `PUT_ACCESS_KEY_ID`, a region other than `S3_REGION`, no valid `x-amz-date`, a credential date other than `x-amz-date`'s, `SignedHeaders` not sorted, or a signature other than the one `PUT_SECRET_ACCESS_KEY` makes | 403 `SignatureDoesNotMatch` |
| then: `x-amz-date` more than 15 min from the clock, as S3 allows | 403 `RequestTimeTooSkewed` |
| `host`, `x-amz-date` or `x-amz-content-sha256` not signed, or any other `x-amz-*` header present and not signed | 403 `AccessDenied` |
| `x-amz-content-sha256` not a SHA-256 (`UNSIGNED-PAYLOAD`, the streaming forms: `out_s3` sends neither) | 403 `AccessDenied` |
| a bucket other than `S3_BUCKET` | 404 `NoSuchBucket` |
| a key that is not `raw1/{CLUSTER_NAME}/{date}/{HH}/{node}/{name}.jsonl.zst`, node and name of `[A-Za-z0-9._-]` starting with a letter or a digit | 403 `AccessDenied` |
| no `Content-Length` | 411 `MissingContentLength` |
| more than `PUT_MAX_BYTES` (128 MiB), with a message that names it and `total_file_size` | 400 `EntityTooLarge` |
| a `Content-MD5` that is not 16 bytes | 400 `InvalidDigest` |
| a body shorter than `Content-Length`, whose SHA-256 is not `x-amz-content-sha256`, or whose MD5 is not `Content-MD5` | 400 `IncompleteBody`, `XAmzContentSHA256Mismatch`, `BadDigest` |
| a failed write or sync, or a failed read of the key's hour's sidecar (below) | 500 `InternalError` |

Each refusal is logged with its code and the reason, at most 10 a minute,
the next line counting those dropped (`refusals_not_logged`): anyone who
reaches the port can send them. `EntityTooLarge` is logged as an error,
since `out_s3` retries such an upload forever: **keep `out_s3`'s
`total_file_size` well below `PUT_MAX_BYTES`**. `out_s3` uploads its
buffer once it reaches `total_file_size` (64M in the agent), compressed,
so an object is normally far smaller, but a buffer can pass the size by
the last chunk it took, and incompressible lines do not shrink. The body
is streamed to its file as it is hashed, never held in memory, so the
limit bounds the spool's disk, per upload in flight, not the compactor's
memory.

The signature covers the headers and the claimed payload hash, and the
body is held to that hash, so a signed request cannot carry another body.
The Go tests verify against two signers other than this repository's: the
PUT example of the S3 API reference, and a request captured from stock
`out_s3` v5.1.3. The endpoint is plain HTTP: SigV4 authenticates the
agent and protects the request's integrity, not its confidentiality, so
where the network between the agents and the compactor is not trusted,
put TLS in front of it (a mesh, or a TLS-terminating proxy) and point
`out_s3` at that with `tls on`.

**Durable before the answer.** The body goes to a new file in
`{SPOOL_DIR}/_tmp/`, hashed as it is written, and only once it matches is
the file synced, renamed to `{SPOOL_DIR}/{key}` (every directory that
creates is synced in its parent) and the key's directory synced; only then
is the answer 200, with the body's MD5, quoted, as `ETag`. A crash before
the rename leaves a file in `_tmp/` that was never acknowledged, and a
start deletes those. A failed sync answers 500 and removes what the put
made, the key's file or a new directory, and `out_s3` uploads again. No
200 rests on a directory entry that was not synced: a put syncs the parent
of every directory on the key's path that this process has not synced
itself (one an earlier process made, or one whose removal after a failed
sync failed too), and a put of a key that exists syncs the key's
directory before answering.

**A key is stored once.** While its file is in the spool, a key keeps its
first content: a PUT of the same content is a 200 that writes nothing (a
retry whose answer was lost), and other content under the key is answered
200 too, with the stored object's ETag, and counted
(`blobs_compactor_put_requests_total{result="conflict"}`, and an error in
the log). Once its hour's final fold has deleted the file, the content
cannot be compared any more, but the hour's sidecar still lists the key: a
PUT of it, whatever its content, is answered 200 with the body's MD5,
stores nothing, and is counted as `already_folded`, with a warning in the
log. Storing it would not fold it either: the next fold finds the key in
the sidecar and deletes the file unread. So the hour keeps the rows of the
key's first content. `out_s3` makes a new key for every upload (`$UUID` in
`s3_key_format`, also on a retry), so a conflict or `already_folded` means
a retry that came after the fold, or a bug elsewhere, such as two
shippers with one node name. The first content was acknowledged, so it
stays; an error answer would make a shipper whose key format has no
`$UUID` retry the same key forever.

The endpoint knows the folded keys without a read per PUT: every fold
hands it the sidecar it read or committed, before it deletes any file, and
a PUT for an hour it has no sidecar of (after a start, or an hour no fold
has had since) reads that sidecar from the bucket once (a HEAD when there
is none) and keeps it, for the 36 hours most recently used. While that
read fails the PUT is answered 500 and `out_s3` retries.

**The spool as the raw source.** `RAW_SOURCE=spool` (default `bucket`)
and `SPOOL_DIR`, an absolute path. A fold lists an hour's keys under
`{SPOOL_DIR}/raw1/{cluster}/{date}/{HH}/` (a file's modification time is
its upload time) and DuckDB reads the local paths; the past pass finds
dates and hours by listing directories. Everything else is as in bucket
mode: the sidecars list the same raw keys, the tiers are in the bucket.
Raw deletion is always on (`RAW_DELETE=after-final`, the default here; `off`
is refused): once an hour's final fold has committed its files are
deleted, a late file once the fold that took it has, and the directories
they leave empty go with them. So a file lives under 2h from its upload,
as raw objects do with deletion on (above, "The loop"), and the raw gauges
and their alert apply as they are. `fold` and `rollup` by hand read the
spool too, so they run where the volume is mounted, while no loop runs.

**Metrics**, besides the others: `blobs_compactor_put_requests_total{result}`
(`stored`, `duplicate`, `conflict`, `already_folded`, or the S3 error code answered),
`blobs_compactor_put_bytes_total`, and from a walk of the spool at every
scrape `blobs_compactor_spool_files`, `blobs_compactor_spool_bytes` and
`blobs_compactor_spool_oldest_age_seconds`. A put is counted after it is
answered, and the walk holds no lock a put waits for, so a large spool or
a slow disk slows scrapes, not acknowledgements.

**Deployment.** The component `deploy/spool` adds to `deploy/base` (or
`deploy/release`): a PersistentVolumeClaim `blobs-compactor-spool`
(ReadWriteOnce, 10Gi), mounted at `/spool`; a Service
`blobs-compactor-put` on port 9000; the env above; `envFrom` the Secret
`blobs-compactor-put` (`PUT_ACCESS_KEY_ID`, `PUT_SECRET_ACCESS_KEY`), which
the overlay provides; `fsGroup` 65534 with `fsGroupChangePolicy:
OnRootMismatch`, so a start does not walk a full spool to change its
files' group; a readiness probe on the put port; and `replicas: 1` with
`strategy: Recreate`, which the volume needs as much as the fold does.
The optional component `deploy/spool-networkpolicy`, added after
`deploy/spool`, is a NetworkPolicy that admits connections to port 9000
only from the agents' namespace (`observability`; an overlay patches the
`namespaceSelector` for its own) and leaves port 9090 open as before. It
needs a network plugin that enforces NetworkPolicy and agents that upload
from pod networking; the endpoint authenticates every request either way. **Data the endpoint has acknowledged lives only on
that volume until its hour is folded**, about two hours of every agent's
uploads, more while folds fail: losing the volume loses them. Give it the
durability the raw tier would have had: a replicated storage class, or a
node whose disk is trusted, and an alert on
`blobs_compactor_spool_oldest_age_seconds`. The bucket's secret then needs
nothing on `raw1/`. The agents point `out_s3` at the Service: `endpoint
http://blobs-compactor-put.<namespace>.svc.cluster.local:9000`, `bucket`
the compactor's `S3_BUCKET`, `tls off` (or on, behind TLS), the put key
pair as `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, and their usual
`s3_key_format`. `itest/compactor-put` is an overlay that does this, with
stock Fluent Bit as the shipper.

## Manifests

`deploy/base/deployment.yaml`, `args: [run]`:

- `replicas: 1`, `strategy: Recreate`. A single writer is a correctness
  requirement for incremental folding. `terminationGracePeriodSeconds:
  300`, so a stop lets the job in flight commit; the loop starts no job
  after a SIGTERM.
- `automountServiceAccountToken: false`. Non-root 65532/65534,
  `readOnlyRootFilesystem`, drop ALL, seccomp RuntimeDefault.
- `envFrom` secret `blobs-compactor-s3` (`AWS_ACCESS_KEY_ID`,
  `AWS_SECRET_ACCESS_KEY`). It needs list and read on `raw1/`, and list,
  read and write on `hourly/` and `daily/`, and delete on
  `hourly/{cluster}/_tmp/` and `daily/{cluster}/_tmp/`; delete on `raw1/`
  only with `RAW_DELETE=after-final`. The compactor deletes nothing else
  in the tiers.
- env, set by overlays where the base has none: `CLUSTER_NAME` (as the
  agent names it), `S3_BUCKET`, `S3_ENDPOINT` (host:port; a scheme is
  stripped), `S3_REGION` (us-east-1), `S3_TLS` (on), `ROLLING_INTERVAL`
  (2m, at most 5m), `GRACE` (10m, at most 30m), `PAST_INTERVAL` (1h, at
  most 1h), `RAW_DELETE`
  (off), `DUCKDB_THREADS` (4), `DUCKDB_HTTP_RETRIES` (5). Not in the base,
  with their defaults: `JOB_TIMEOUT` (30m), `METRICS_ADDR` (:9090),
  `BLOBS_DIR`, a local directory in place of the bucket, and
  `PARQUET_KEYS_DIR` and `PARQUET_KEY_ID` (unset: no encryption); an
  overlay that turns encryption on mounts the keys Secret and sets both,
  as `itest/compactor` does. Spool mode (above): `RAW_SOURCE` (bucket),
  `SPOOL_DIR`, `PUT_LISTEN` (unset: no endpoint), `PUT_ACCESS_KEY_ID`,
  `PUT_SECRET_ACCESS_KEY` and `PUT_MAX_BYTES` (128 MiB), which
  `deploy/spool` sets.
- `DUCKDB_MEMORY_LIMIT` from `resourceFieldRef: limits.memory`, with
  DuckDB given 75% of it. `/scratch` is an emptyDir for `temp_directory`
  (`DUCKDB_TEMP_DIRECTORY`), `/tmp` a small one. Starting values:
  requests 250m/1Gi, limit 4Gi, scratch 10Gi.
- `prometheus.io/scrape` and `prometheus.io/port: "9090"` for `/metrics`,
  written with stdlib text exposition:
  `blobs_compactor_last_success_timestamp_seconds{job}` and
  `blobs_compactor_last_duration_seconds{job}` (jobs `rolling`, `past`,
  `rollup`), `blobs_compactor_runs_total{job,result}`,
  `blobs_compactor_rows{tier,table}` (rows of the last object written),
  `blobs_compactor_raw_keys_read_total`, `blobs_compactor_raw_deleted_total`,
  `blobs_compactor_raw_objects` and `blobs_compactor_raw_oldest_age_seconds`
  (by upload time, aging between listings), and
  `blobs_compactor_parquet_keys_missing`, and in spool mode the put and
  spool families (above). The raw gauges are per hour as its
  latest listing saw it, less what was deleted since: every fold lists its
  hour, the rolling pass's folds too, and the past pass lists every hour,
  so a deletion by either pass shows at once.
  The node agent scrapes it into the blobs, where alerting sees it (the
  itest asserts that it arrives). The endpoint exists for these rules,
  which go in with the rules that phase 3 wires into the tailer:
  `time() - blobs_compactor_last_success_timestamp_seconds{job="rolling"} > 600`,
  with raw deletion on, `blobs_compactor_raw_oldest_age_seconds > 7200`,
  and `blobs_compactor_parquet_keys_missing > 0`.

`deploy/release/kustomization.yaml`: `resources: [../base]`, and `images:`
with the tag and digest (written by `build.sh bump`), to be vendored
downstream. `itest/compactor` is an overlay; `itest/compactor-put` adds
the `deploy/spool` component.

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
   (done 2026-09-25), in GHA on `ubuntu-latest` (done: amd64 native,
   arm64 emulated, the Mac's direction reversed), and on an Ubuntu amd64
   host (to do).
2. A cold `check` (empty cache volume, another checkout path) reproduces
   the pin: done on an arm64 Mac; every GHA run is cold.
3. `selftest` passes on both arches on native hosts: arm64 on a Mac,
   amd64 in GHA's `check`.
4. The itest's compactor assertions pass with the image loaded into
   y-cluster: done 2026-10-03 on an arm64 Mac (docker backend).

## Open

- The earlier object-storage cost model assumed one LIST
  per node-hour for DuckDB reads; it needs a correction.
- The node-agent and blobs-to-prometheus images keep their Dockerfile
  builds. blobs-to-prometheus could move to this script's pattern later.
- Raw deletion from the bucket is covered by the Go tests and by
  `TestS3Bucket` (a bucket on versitygw, run by hand), not by the itest:
  an hour is final only at H+1h+`GRACE` of wall clock, and the itest's raw
  assertions need the raw objects. Deletion from the spool is in the
  itest, through objects uploaded for hours already past.
- Spool mode: the itest's shipper is stock Fluent Bit v5.1.3; the node
  agent (the fork, at v5.1.2) keeps uploading to the bucket there, so its
  own `out_s3` is not run against the endpoint. One compactor holds the
  spool, so the put endpoint is down while it restarts, and the spool's
  durability is its volume's.
- A raw object the engine cannot read (truncated, not JSON lines) fails
  its hour's fold on every pass: `blobs_compactor_runs_total{result="failure"}`
  shows it, and nothing skips the object.
- Encrypted tiers: a reader's per-key glob cannot use `union_by_name`
  (an engine error in DuckDB 1.5), so a view over one key's objects fails
  on an object that lacks a column a later raw1 version added; the
  compactor reads object by object and is not affected. Revisit on a
  DuckDB bump (`TestEngineParquetEncryption` fails when it is fixed).
- The current day has no daily tier until its last hour is final; the
  hourly tier is the recent tier. `rollup <date>` makes one by hand.
