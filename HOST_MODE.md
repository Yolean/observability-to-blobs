# Host mode: the agent as a systemd service

Status: built. `images/fluentbit/host/` makes the release tarball (the pipeline `host.yaml`, the
heartbeat script, the systemd unit), `itest/host/` is the itest. The DaemonSet (`agent/`) ships what
a Kubernetes node knows about its workloads. Host mode ships what the host itself records, from
outside the cluster, to a bucket of its own that the host can add objects to but never read, change
or delete. The two run side by side on a node and share no credential.

## What it ships

| Source | How | Notes |
|---|---|---|
| The journal | `systemd` input, every unit, with a denylist | A denylist fails toward collecting too much: an allowlist of units misses messages logged by PID 1 about a unit, such as a stop. Defaults deny nothing; `HOST_JOURNAL_DENY`, a regular expression on a row's `unit`, names noisy units to leave out. |
| The audit log, `/var/log/audit/audit.log` | `tail`, following the file by inode across rotation, never the numbered files | auditd's `audit(timestamp:serial)` identifies each record. Reading it needs auditd's `log_group` set to a group the service is in (`adm`), and auditd restarted or reloaded. A reload is a SIGHUP, `systemctl kill -s HUP auditd`: Ubuntu 24.04's unit has no `ExecReload`, so `systemctl reload auditd` is refused. On a reload, auditd 3.1.2 (Ubuntu 24.04's) reopens the log when `log_group` changed and sets the group of the directory and the files (`reconfigure()` in `src/auditd-event.c`; read from the source, not run on a host). A pristine Ubuntu 24.04 has no auditd: `apt-get update`, then `apt-get install auditd`. |
| Package changes: `/var/log/dpkg.log`, `/var/log/apt/history.log`, `/var/log/unattended-upgrades/*.log` | `tail` | A host without unattended-upgrades has no such directory, which is no error (see the next row). |
| Further files, configured | `tail`, `HOST_EXTRA_FILES` | Comma-separated paths or globs, for example a Kubernetes API server audit log written on the host. A glob whose directory may be missing needs one letter of the directory in brackets, as `files` has `/var/log/unattended-upgrade[s]/*.log`: tail aborts a glob at a missing directory named as it is, and logs a warning and an error at every scan, which the journal then ships. With the bracket the directory is a pattern too, and a missing one is no match. |
| A heartbeat | every 60 s, a metrics line with the boot id, the collectors running and a counter | The counter is kept in the state directory: it continues across restarts of the service and starts again at a new boot id. A missing counter value or a silence longer than the interval shows a gap at the receiver, and the `collectors` label a host whose sources changed. |
| Node metrics and the restart flag | `node_exporter_metrics` (see Metrics) | Served on the node, not shipped (see Metrics). |

## Schema: a subset of `raw1`

Host lines use the raw tier's line contract (`compactor/sql/raw.sql`) and add no column, so every
reader of `raw1` reads them:

- **Journal entries** are `logs` rows exactly as the DaemonSet's journal rows: `stream` `journal`,
  `unit`, `priority`, `message`, `ts`, `cluster`, `node`. They use the DaemonSet's processors, plus
  the denylist.
- **Lines of tailed files** are `logs` rows with `stream` `file` and `unit` the file's path, for
  example `/var/log/audit/audit.log`, with `message`, `ts`, `cluster` and `node`. These two values
  are the only extension, documented in `raw.sql`; the audit log is read from its file because
  journald's copy of audit events is best effort. `ts` is the time the agent read the line; the
  line's own stamp (the audit record's `audit(...)`) is in the message.
- **Metrics** are `metrics` rows as the DaemonSet's node scrape (`scrape` `node`, `instance`,
  `samples`): today the heartbeat, as one sample,
  `observability_host_heartbeat{boot_id="...",collectors="files,heartbeat,journal,node,self"}`
  (type `counter`) with the counter as value, and `instance` the node's name and the metrics port.
  `collectors` are the collectors running, comma-separated, as the launcher started them: a
  receiver can tell from the bucket alone that a host stopped running `files` or `journal`. A
  change of them, like a new boot id, starts another series.
  Rows carry `time` (the script's stamp to the nanosecond, as the node agent's scrapes do) and `ts`
  (the same, rendered at microseconds), and no `namespace`, `pod` or `container`. The node exporter
  series, the restart flag and the agent's own metrics are served on the node; shipping them is
  not built (see Metrics).
- No `podindex` rows.

## Where it writes

- One object per upload window (default 30 s, `UPLOAD_TIMEOUT`), JSON lines, zstd, with PutObject only
  (`use_put_object`) and `send_content_md5: true`, which buckets with Object Lock require.
- Key: `/${KEY_ROOT}/${CLUSTER_NAME}/%Y-%m-%d/%H/${NODE_NAME}/%M%S-$UUID.jsonl.zst`. `KEY_ROOT`
  defaults to `host1` and is configurable (for example `security1`). The layout below the root is
  the DaemonSet's `raw1/`, at the same depth, so the same globs and readers work on either root;
  hour before node, so a consumer lists one hour for the whole cluster. The root says what the
  objects are: the host agent never writes under the DaemonSet's root, and the two never share
  one. No marker such as `_host` in the path: a segment only some objects have splits every
  fixed-depth glob, and readers in the Hadoop tradition skip paths that start with `_`.
- The agent never lists, reads, overwrites or deletes an object. Its key can be a write-only one,
  and the bucket can carry Object Lock and a default retention.
- Every upload attempt gets a fresh key. A retry under the same key would create a second version
  of an existing object on a versioned bucket, which a check for keys with more than one version
  reads as tampering. The cost is a possible duplicate object when an upload succeeded but its
  response was lost; consumers deduplicate on the record's own identity (the audit serial, the
  heartbeat's boot id and counter; for a journal row `ts`, `unit` and `message`, because the rows
  carry no cursor).

### How the fresh key comes about

Read in fluent-bit v5.1.2's `out_s3` (`plugins/out_s3/s3.c`, `src/aws/flb_aws_util.c`): with
`use_put_object` every attempt goes through `s3_put_object()`, which calls `flb_get_s3_key()`
afresh, and that draws `$UUID` again: eight characters from `[0-9a-zA-Z]`, from random bytes
(`flb_sts_session_name()`). So `$UUID` is per attempt, not per chunk, and no patch is needed. What
stays the same across the attempts of one chunk is everything else in the key: the time, which is
the chunk's first record (`%Y-%m-%d/%H` and `%M%S`), so a retried object lands in the same hour
directory with the same `MMSS`, under another suffix. Two attempts draw the same suffix with a
probability of 62^-8; the itest checks that no key is written twice and that a refused upload
reappears under a new suffix. This holds only for PutObject: a multipart upload fixes its key when
it is created, which is one more reason for `use_put_object`.

## Buffering

- `storage.type: filesystem` on every input that feeds the bucket and `storage.sync: full`, so a
  record the input has read survives a crash or a power loss of the host, not only a restart of
  the service. The positions are written with `db.sync: full` as well.
- The inputs keep their positions in the state directory (`db` for `tail`, the journal cursor for
  `systemd`), so after a restart or a reboot delivery resumes where it stopped: records arrive once,
  not from "now" and not twice. On the first start the journal is read from its end and the files
  from their start.
- The output buffers in the state directory up to `HOST_BUFFER_LIMIT` (default 4G) in each of its two
  buffers, and retries without a budget (`retry_limit` set explicitly, see
  `images/fluentbit/UPSTREAM.md`), as the DaemonSet does. Beyond the limit the oldest input chunks
  are dropped.
- A shutdown flushes what was read (`grace` 10 s); what the bucket does not take stays on disk for
  the next start.
- fluent-bit warns at every start: "Please use 'store_dir_limit_size' with s3 output instead of
  'storage.total_limit_size'". Both are set on purpose, one for each buffer.

## Packaging

- The binary is the agent image's fluent-bit (same version and patches, `images/fluentbit/`), built
  in a stage for the host's distribution (Ubuntu 24.04 first) so it links against the host's C
  library, without the `cri_scrape` plugin, which needs the container runtime, and without the
  Kafka output. It has the `exec` input, which the heartbeat uses and the agent image leaves out.
  The Dockerfile's `host-package` stage refuses any base other than `HOST_DISTRO`.
- `images/fluentbit/host/build.sh` builds the tarball
  `observability-to-blobs-host-<version>-linux-<arch>.tar.gz` and its `.sha256` (the version is
  `HOST_VERSION`, the arch the build machine's, `amd64` or `arm64`). A release tag `v<version>`
  attaches both arches' tarballs and checksums to the GitHub release, with y-obs's binaries, built
  by CI with this same script (`.github/workflows/release.yaml`). The tarball holds, relative to the install prefix
  `/opt/observability-host`:

  | path | what |
  |---|---|
  | `bin/fluent-bit` | the binary, stripped |
  | `bin/observability-host-heartbeat` | the heartbeat script (POSIX `sh`, coreutils and `sed`) |
  | `bin/observability-host-launch` | the launcher (POSIX `sh`, see Collectors), which the unit runs |
  | `etc/host.yaml` | the pipeline's service section and S3 output, with the defaults of their settings |
  | `etc/collectors.d/<name>.yaml` | one collector each: its inputs and their processors |
  | `etc/parts.d/<name>.yaml` | what collectors share: `serve-metrics`, the metrics port |
  | `etc/collectors.defaults` | the collectors that run when nothing says otherwise |
  | `etc/observability-host.env.example` | the environment file, commented |
  | `lib/systemd/system/observability-host.service` | the unit; goes to `/etc/systemd/system/` |
  | `share/fluent-bit-LICENSE` | Fluent Bit's licence |

- The binary needs from the host's packages (Ubuntu 24.04): `libc6`, `libgcc-s1`, `libstdc++6`,
  `libssl3t64`, `libyaml-0-2` and `libsystemd0`, and `ca-certificates` for TLS. The build fails if
  the binary links anything else.
- A configuration management tool installs it: downloads the tarball, verifies the checksum,
  extracts it to the prefix, creates the user, writes the environment file and the credential, links
  the unit and keeps the installed files from drifting. The repository does not install anything on
  a host by itself; `itest/host/run.sh` shows the steps.
- The files of an install, all `root:root`. The service runs as `observability-host`, so it must be
  able to read every one of them but the credential, which systemd hands it:

  | path | mode |
  |---|---|
  | `/opt/observability-host/` | as extracted (`tar --no-same-owner`) |
  | `/etc/observability-host/` | `0755` |
  | `/etc/observability-host/env`, `args`, `required` | `0644`: no secret is in them |
  | `/etc/observability-host/collectors/` (Your own collectors) | `0755`, its files `0644` |
  | `/etc/observability-host/s3-credentials` | `0600` |

### Upgrading

The state directory carries over: positions, the journal cursor, the heartbeat's counter and the
buffers. An upgrade resumes where the previous version stopped:

```sh
systemctl stop observability-host
mkdir /opt/observability-host.new
tar xzf observability-to-blobs-host-<version>-linux-<arch>.tar.gz -C /opt/observability-host.new --no-same-owner
rm -rf /opt/observability-host && mv /opt/observability-host.new /opt/observability-host
install -m 0644 /opt/observability-host/lib/systemd/system/observability-host.service /etc/systemd/system/
systemctl daemon-reload
systemctl start observability-host
```

- **A new prefix, not one extracted over the old:** a file that a version no longer has, such as
  a collector, would stay and still be read.
- **The unit again, every time:** a unit the release changed is otherwise left as it was. From
  0.2.1, the first release, this is required. Its unit runs `fluent-bit -c etc/host.yaml`, and
  the new `etc/host.yaml` alone has no inputs: the agent would run and ship nothing. A unit linked
  with `systemctl link /opt/observability-host/lib/systemd/system/observability-host.service`
  follows the prefix, and needs only the `daemon-reload`.
- **Or a prefix per version:** extracted to `/opt/observability-host-<version>`, with
  `/opt/observability-host` a symbolic link to it, swapped with the agent stopped. The previous
  version stays, for a rollback.

## The systemd unit

- A system user of its own, `observability-host`, created by the installer
  (`useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin observability-host`),
  and `SupplementaryGroups=systemd-journal adm` for the journal and the logs.
- `StateDirectory=observability-host`: positions, storage and the output buffer in
  `/var/lib/observability-host`, never the DaemonSet's `/var/lib/node-agent`.
- `LoadCredential=s3:/etc/observability-host/s3-credentials`: the bucket key from a root-only file in
  the AWS shared credentials format, handed to the service in its credentials directory
  (`AWS_SHARED_CREDENTIALS_FILE=%d/s3`); it is never in the environment file, the unit or the
  command line. fluent-bit's credential chain reads, in this order, the `AWS_ACCESS_KEY_ID`
  variables and then that file (`src/aws/flb_aws_credentials.c` and `flb_aws_credentials_profile.c`,
  profile `default`), so the variables must stay unset. It reads the file when it first needs
  credentials and keeps what it read: a changed key takes a restart of the unit.
- `EnvironmentFile=/etc/observability-host/env`: `CLUSTER_NAME`, `NODE_NAME` (default: the host
  name), `S3_BUCKET`, `S3_ENDPOINT`, `S3_REGION`, `S3_TLS`, `KEY_ROOT`, and the settings below. The
  unit refuses to start without `CLUSTER_NAME`, `S3_BUCKET` and `S3_ENDPOINT`.
- `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `NoNewPrivileges=yes`, an empty
  capability set, write access only to the state directory.
- `Restart=always` without a start limit. Stopping or disabling the unit is itself a journal record;
  the agent ships it when it next starts at the latest (the journal position survives, and the
  itest checks that the record arrives), and the heartbeat's silence shows the stop.

### Settings

`CLUSTER_NAME`, `S3_BUCKET` and `S3_ENDPOINT` are required. The rest are optional; the defaults
are in `etc/host.yaml` and `etc/collectors.d/`.

| setting | default | |
|---|---|---|
| `CLUSTER_NAME`, `NODE_NAME` | `NODE_NAME`: the host name | in every line and every key |
| `S3_BUCKET`, `S3_ENDPOINT` | | `S3_ENDPOINT` with its scheme |
| `S3_REGION`, `S3_TLS` | `us-east-1`, `on` | |
| `KEY_ROOT` | `host1` | |
| `UPLOAD_TIMEOUT` | `30s` | |
| `HOST_EXTRA_FILES` | none | comma-separated paths or globs to tail |
| `HOST_JOURNAL_DENY` | matches nothing | regular expression on the row's `unit`; matches are left out |
| `HOST_NODE_COLLECTORS` | see Metrics | `textfile` alone on a Kubernetes node |
| `HOST_METRICS_LISTEN`, `HOST_METRICS_PORT` | `127.0.0.1`, `2023` | the address and the port the metrics are served on |
| `HOST_BUFFER_LIMIT` | `4G` | each of the output's two on-disk buffers |
| `HOST_TLS_CA_FILE` | `/etc/ssl/certs/ca-certificates.crt` | the CA bundle the S3 output trusts; `/etc/ssl/cert.pem` on macOS |
| `HOST_LOG_LEVEL` | `info` | fluent-bit's log level |
| `HOST_COLLECTOR_DIRS` | none | more directories of collectors, colon-separated (Collectors) |

### Collectors

Each source is a collector: a file in `etc/collectors.d/` with its inputs and their processors. The
unit runs `bin/observability-host-launch`. The launcher picks the collectors, writes
`/var/lib/observability-host/fluent-bit.yaml`, which includes `etc/host.yaml`, the enabled
collectors and the parts they need, and execs fluent-bit with it. A collector that does not run
adds nothing to the process: no input, no port, no command.

| collector | default | what |
|---|---|---|
| `heartbeat` | on, cannot be turned off | the heartbeat row (see What it ships); its silence is how a receiver tells a stopped agent from a quiet one |
| `journal` | on | the journal, with `HOST_JOURNAL_DENY` |
| `files` | on | the audit log, the package logs and `HOST_EXTRA_FILES` |
| `node` | on | `node_exporter_metrics`, served on the metrics port (see Metrics) |
| `self` | on | the agent's own metrics, served on the metrics port |

The defaults are the pipeline as it was before collectors existed. The metrics port, the part
`parts.d/serve-metrics.yaml`, is opened only when a collector that runs needs it, `node` or
`self`: with both off, nothing listens. `HOST_NODE_COLLECTORS` remains the list of
node_exporter's own collectors within `node`.

The flags are node_exporter's (its README, "Collectors"):

- `--collector.<name>` enables a collector, and `--no-collector.<name>` disables one.
- `--collector.disable-defaults` disables every collector that is not enabled explicitly, wherever
  it is in the flags.
- For one collector the last flag wins.
- An unknown collector or flag, or `--no-collector.heartbeat`, is refused, and the unit then does
  not start.
- A collector the host requires is refused as `--no-collector.<name>` too, and stays on with
  `--collector.disable-defaults`. The installer lists them in `/etc/observability-host/required`,
  beside the args file (`HOST_REQUIRED_FILE`), names one or more a line with `#` for comments: for
  example `files` and `journal`, where they are the sources of a security log. The args file then
  cannot stop them, and the heartbeat's `collectors` label shows that they run (Schema).

The flags come from `/etc/observability-host/args`, one or more per line with `#` for comments
(`HOST_ARGS_FILE`; a missing file is no flags), followed by the launcher's own command line. For
example, on a Kubernetes node, where the DaemonSet exports the machine's `node_*` (see Metrics),
`HOST_NODE_COLLECTORS=textfile` keeps `node` for the restart flag alone. A host that only
forwards its journal would have:

```
# /etc/observability-host/args
--collector.disable-defaults
--collector.journal
```

Two more settings of the launcher:

- **`HOST_COLLECTOR_DIRS`** names more directories of collectors, separated by colons. Each has a
  `collectors.d/` and may have `parts.d/` and a `collectors.defaults`, whose collectors add to the
  defaults. This lets a separate package add collectors without changing the tarball's files.
  The machine's own, `collectors/` beside the args file, comes after them (Your own collectors).
  A collector name may be in only one directory. `--collector.disable-defaults` turns off every
  directory's defaults; a machine that wants another directory's collectors but not the host's
  own turns those off with `--no-collector.<name>`.
- **The state directory** is the launcher's `STATE_DIRECTORY`, which the unit sets to
  `/var/lib/observability-host`. The launcher passes it to fluent-bit as `HOST_STATE_DIR`,
  which every path in `host.yaml` and the collectors uses. It also passes it to the heartbeat as
  `HEARTBEAT_STATE_DIR`, unless that is set already.

A change of collectors takes a restart of the unit, as a change of the environment file does.
`observability-host-launch --list` shows what would run, and `--print` the configuration it would
write.

### Your own collectors

A machine adds sources of its own in `/etc/observability-host/collectors/`, beside the args file,
laid out as the tarball's `etc/`:

```
/etc/observability-host/collectors/
  collectors.d/<name>.yaml   a collector: fluent-bit YAML, its inputs with their processors
  collectors.defaults        the names that run without a flag, one a line
  parts.d/<part>.yaml        if a collector needs one, named on its "# needs: <part>" line
```

The launcher reads it when it has a `collectors.d/`, and its collectors take the same flags. An
upgrade of the tarball leaves it alone. For log files alone no collector is needed:
`HOST_EXTRA_FILES` adds them to `files`.

**What a collector writes.** Host lines are `raw1` lines (Schema), and the compactor's guard
refuses a line whose table it does not fold, or with a field `compactor/sql/raw.sql` does not
name (`compactor/guard.go`). So a collector:

- tags its inputs `blob`, the S3 output's tag;
- writes `logs` rows, with `stream`, `unit` and `message`, `cluster` and `node`, or `metrics`
  rows as the heartbeat's;
- keeps every other field out with a `record_modifier` allowlist;
- keeps a source's line whole in `message`, with no `parser`. A parser lifts the line's fields
  out of `log`, the allowlist then drops them, and the row arrives without a `message`: the guard
  lets it through, and the content is lost. Readers take fields out of `message`, for example
  `json_extract_string(message, '$.event')`;
- sets a tail's `buffer_max_size` above the longest line it will see, with `skip_long_lines: on`.
  The default is 32 KB: a longer line is then dropped with a warning, and without
  `skip_long_lines` tail gives up on the whole file ("lines are too long. Skipping file.") and
  later lines never arrive;
- keeps its state, such as a tail's `db`, in a file of its own under `${HOST_STATE_DIR}`;
- buffers on disk: `storage.type: filesystem`.

A source whose rows do not fit `raw1` belongs under a key root of its own (`KEY_ROOT`, Where it
writes), from an agent of its own, with its own contract and readers.

For example, an application's log files as a collector of their own, switched apart from
`files`, with longer lines:

```yaml
# /etc/observability-host/collectors/collectors.d/myapp.yaml
pipeline:
  inputs:
    - name: tail
      alias: myapp
      tag: blob
      path: /var/log/myapp/*.log
      # the path is the row's unit
      path_key: unit
      # what the files hold at the first start, too; later starts resume from db
      read_from_head: true
      db: ${HOST_STATE_DIR}/myapp.db
      db.sync: full
      db.locking: true
      # above the longest line, or it is dropped (the default is 32k)
      buffer_max_size: 8M
      skip_long_lines: on
      storage.type: filesystem
      processors:
        logs:
          - name: modify
            add:
              - table logs
              - cluster ${CLUSTER_NAME}
              - node ${NODE_NAME}
              - stream file
            rename:
              - log message
          - name: record_modifier
            allowlist_key: [table, cluster, node, stream, unit, message]
```

It runs once `myapp` is in `/etc/observability-host/collectors/collectors.defaults`, or with
`--collector.myapp` in the args file.

**Files already in `HOST_EXTRA_FILES` stay there.** A collector of their own has a `db` of its
own and reads them from their start, so every line would be shipped again. File rows have no
identity to drop the second by, and their `ts` is the time they were read. Moving them anyway
takes `read_from_head: false` in the new collector, and an application that does not write
between the stop of the agent and its start: the new collector starts at the files' ends.

**What a collector can read.** The unit runs fluent-bit as `observability-host`, in the groups
`systemd-journal` and `adm`. `ProtectSystem=strict` leaves it nothing to write but the state
directory, and `ProtectHome=yes` hides `/home`, `/root` and `/run/user`.

- A file needs to be readable by that user or one of its groups, and every directory above it
  traversable. Group `adm` and mode `0640` are enough, without a restart: the next scan finds
  it.
- Rotation must keep the group and the mode, e.g. logrotate's `create 0640 myapp adm`.
- A file the agent cannot read does not stop the unit. fluent-bit logs that it cannot open it,
  at every scan (`refresh_interval`, 60 s by default), and ships nothing from it. Look in the
  unit's journal after adding a source.

A collector that needs more gets it from a drop-in, which adds to the unit's lists, and a
restart:

```
# systemctl edit observability-host
[Service]
SupplementaryGroups=myapp
# only for a source under a home directory
ProtectHome=read-only
```

An `exec` collector's command runs as the same user, in the same sandbox. Give it with its full
path.

**Applying a change:** `systemctl restart observability-host`. The launcher logs the collectors
it runs (`collectors: ...`) to the unit's journal before it starts fluent-bit, and refuses an
unknown name, so the unit then does not start.

The collectors have two tests:

- `images/fluentbit/host/test-launch.sh`, run by `./test.sh checks`, covers the flags and the
  configuration written.
- `itest/host-docker/run.sh` runs the tarball in Ubuntu containers against versitygw, with no
  systemd and no qemu, so it also runs where the host phase can't. A user that a bucket policy
  allows only PutObject under the root writes, and the test checks for each set of flags what
  the process does:
  - the collectors it logs;
  - whether the metrics port is open, and what it serves;
  - the rows in the bucket, read through `compactor/sql/raw.sql`;
  - that `--no-collector.heartbeat` stops the launcher with exit 2 before fluent-bit starts, and so
    does `--no-collector.files` where `/etc/observability-host/required` lists `files`;
  - the heartbeat's `collectors` label, for each set of flags;
  - `files` with `HOST_EXTRA_FILES` unset: the package logs arrive, and fluent-bit warns of
    nothing but `store_dir_limit_size` (Buffering), with `/var/log/unattended-upgrades/` missing
    or there;
  - the example of Your own collectors, as written here, from `/etc/observability-host/collectors/`.

  Run it with `TARBALL=target/host/<tarball> itest/host-docker/run.sh`. CI runs it on amd64 and
  arm64 (`.github/workflows/ci.yaml`, `host-docker`). The itest's host phase passes the defaults
  through fluent-bit's dry run and runs them as the unit.

## Metrics

The same signals SRE staff know from the DaemonSet's nodes, under the same names: fluent-bit's
`node_exporter_metrics` input, configured as in `agent/fluent-bit.yaml` (same collectors, same
interval), so a host's `node_*` series look like a Kubernetes node's and existing dashboards and
alert rules apply. The host adds `timex` (the clock offset, `node_timex_offset_seconds`, and
`node_timex_sync_status`), `systemd` (the unit states) and `textfile` (the restart flag below) to the
DaemonSet's collectors; `HOST_NODE_COLLECTORS` is the list.

They are served by a `prometheus_exporter` output on `HOST_METRICS_LISTEN:HOST_METRICS_PORT`, an
address on the node (default `127.0.0.1:2023`), together with the service's own metrics (input and
output records, errors, retries, buffer use). Where to:

- **On a Kubernetes node** the DaemonSet already exports `node_*` for the machine. Set
  `HOST_NODE_COLLECTORS=textfile`: the host agent then serves only the series the DaemonSet cannot
  see (the restart flag, its own metrics), so no series is doubled, and the DaemonSet scrapes
  `HOST_METRICS_LISTEN` (the node's address) as a static target of `cri_scrape`: they reach the node
  data bucket and the DaemonSet's re-serving endpoint like any other target, and no key crosses
  between the two buckets. The static target is not part of `agent/`, which does not know whether
  a host agent runs beside it: an overlay sets the DaemonSet's `STATIC_TARGETS` to
  `node@127.0.0.1:2021/metrics,host@$(NODE_IP):2023/metrics` (Kubernetes expands `$(NODE_IP)`,
  the node's address from the downward API, but only from an env entry above it: a strategic merge
  patch puts its own entries first, so it repeats `NODE_IP` ahead of `STATIC_TARGETS`), and the
  host's series arrive with `scrape="host"`.
  The host agent must then listen on the node's address, not 127.0.0.1, and the host's firewall
  must admit the port from the pods only.
- **On a host without Kubernetes** nothing else exports them. The host agent serves all of them for
  a local Prometheus. Shipping them to the bucket as the `metrics` table (the DaemonSet's schema)
  under its own root is **not built yet**: it needs a scraper in the host build, and `cri_scrape`,
  the only one the agent has, needs the container runtime. Until then only the heartbeat is shipped
  as a metrics row.

## Restart needed

Packages that need a reboot (a kernel, libc, systemd) leave `/var/run/reboot-required` and the
list `/var/run/reboot-required.pkgs`. The host agent does not reboot anything; it exposes the flag
as metrics, in the names the Prometheus community's textfile collector scripts use, so a common
alert rule fits:

- `node_reboot_required` 1 when the flag exists, else 0;
- `node_reboot_required_since_seconds` the flag's modification time, so an alert on age survives a
  restart of whatever evaluates it (a rule's `for:` does not); 0 when there is no flag, so a rule on
  the age multiplies by `node_reboot_required`;
- `node_reboot_required_packages{package="..."}` 1 per package in the list, so a rule can treat a
  kernel or a package with a known exploited vulnerability differently from the rest.

The heartbeat script writes them every interval to `reboot-required.prom` in the state directory,
which the `textfile` collector reads. The collector keeps every series it has read until the agent
restarts, so a flag removed without a reboot leaves its package series at 1 until then; a reboot,
which is what clears the flag, restarts the agent. This repository defines the metrics; the alert
rules (how old a pending reboot may get) belong to whoever runs the alerting.

## What the itest verifies (host phase, on the qemu node)

The host agent installed from the tarball as a systemd unit on the node, writing through a recording
proxy to its own versitygw bucket. What the storage enforces (Object Lock, retention, which key may
do what) is the storage's responsibility and not tested here; the itest verifies that the agent
writes in a way that makes those guarantees possible. `itest/host/run.sh` runs after the node
agent's assertions in `./test.sh` (qemu backend only, the docker node has no systemd) and alone
with `./test.sh host`.

The gateway is a second versitygw (`itest/host/`) with an IAM directory and an access log; the
agent's key is a user allowed `s3:PutObject` under `host1/` by a bucket policy, and the phase first
checks, with a second user under the same policy, that nothing else works with it. The recorder
(`itest/host/s3-recorder.py`) sits between the agent and the gateway on the node: it keeps one JSON
line per request, forwards it unchanged, and refuses requests when told to, which is how the phase
stops delivery and makes uploads fail.

- **Survival:** an authentication event on the host (a login of a user that does not exist), its
  local logs deleted by root after the agent has read them (the audit log emptied, the journal
  rotated and vacuumed, the syslog file removed); the event is in the bucket as a journal row and
  as an audit log row, and the time from event to object is measured and printed.
- **Exactly once:** delivery stopped, events created (journal lines and lines of a configured file),
  the service restarted, more events created, the node rebooted, still more events created, delivery
  restored: every event arrives, once. The journal is persistent on the node, as on a production
  host.
- **Gap:** the service stopped for longer than the heartbeat interval (130 s); the heartbeat's
  counter continues and its time shows the gap, the counter goes up by one within a boot and never
  repeats, and the reboot starts a new boot id and the counter at 1. A line logged while the service
  was stopped arrives once after the start, and the service's own stop record arrives as well.
- **Put only:** the agent works with a key allowed nothing but PutObject on its root, and the
  gateway's request log shows nothing but PutObject answered 200 from it, all under the root: no
  list, read, head or delete, no multipart upload, no denied request.
- **Content-MD5:** every upload the recorder saw carries a Content-MD5 header that matches the body.
- **Fresh keys:** uploads that are refused and retried land under new keys, no key is written twice
  (in the recorder's log of every attempt and in the gateway's log), and a refused upload reappears
  in the same hour and `MMSS` under another suffix.
- **Schema:** every host line reads through `compactor/sql/raw.sql` with no field outside its
  columns; the keys of journal, file and heartbeat lines are exactly the ones above; `table` is
  `logs` or `metrics`; `stream` and `unit` take the values above.
- **Coverage:** one record each of a login, a failed login, an account change, a privileged
  command, a configuration file change, a service stopped and a package installed, from the sources
  the spec names (the journal, the audit log, dpkg's log and apt's history).
- The node serves the restart flag and the agent's own metrics on the metrics port and, with
  `HOST_NODE_COLLECTORS=textfile`, no `node_*` of the machine.

## Not decided here

The receiver: what reads the bucket, the gap and tampering checks and their alarms. The host agent
only makes them possible.
