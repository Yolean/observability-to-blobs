# Host mode: the agent as a systemd service

Status: specification, not built. The DaemonSet (`agent/`) ships what a Kubernetes node knows about
its workloads. Host mode ships what the host itself records, from outside the cluster, to a bucket
of its own that the host can add objects to but never read, change or delete. The two run side by
side on a node and share no credential.

## What it ships

| Source | How | Notes |
|---|---|---|
| The journal | `systemd` input, every unit, with a denylist | A denylist fails toward collecting too much: an allowlist of units misses messages logged by PID 1 about a unit, such as a stop. Defaults deny nothing; the configuration names noisy units to leave out. |
| The audit log, `/var/log/audit/audit.log` | `tail`, following the file by inode across rotation, never the numbered files | auditd's `audit(timestamp:serial)` identifies each record. Reading it needs auditd's `log_group` set to a group the service is in. |
| Package changes: `/var/log/dpkg.log`, `/var/log/apt/history.log`, `/var/log/unattended-upgrades/*.log` | `tail` | |
| Further files, configured | `tail` | For example a Kubernetes API server audit log written on the host. |
| A heartbeat | every 60 s, a metrics line with the boot id and a counter | The counter is kept in the state directory: it continues across restarts of the service and starts again at a new boot id. A missing counter value or a silence longer than the interval shows a gap at the receiver. |
| Node metrics and the restart flag | `node_exporter_metrics` (see Metrics) | |

## Schema: a subset of `raw1`

Host lines use the raw tier's line contract (`compactor/sql/raw.sql`) and add no column, so every
reader of `raw1` reads them:

- **Journal entries** are `logs` rows exactly as the DaemonSet's journal rows: `stream` `journal`,
  `unit`, `priority`, `message`, `ts`, `cluster`, `node`.
- **Lines of tailed files** are `logs` rows with `stream` `file` and `unit` the file's path, for
  example `/var/log/audit/audit.log`, with `message`, `ts`, `cluster` and `node`. These two values
  are the only extension, documented in `raw.sql`; the audit log is read from its file because
  journald's copy of audit events is best effort.
- **Metrics** are `metrics` rows exactly as the DaemonSet's node scrape (`job` `node`, `instance`,
  `samples`): the node exporter series, the restart flag, the agent's own metrics, and the
  heartbeat as one sample, `observability_host_heartbeat{boot_id="..."}` with the counter as value.
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
  response was lost; consumers deduplicate on the record's own identity (journal cursor fields,
  the audit serial, the heartbeat's boot id and counter).

## Buffering

- `storage.type: filesystem` on every input and `storage.sync: full`, so a record the input has read
  survives a crash or a power loss of the host, not only a restart of the service.
- The inputs keep their positions in the state directory (`db` for `tail`, the journal cursor for
  `systemd`), so after a restart or a reboot delivery resumes where it stopped: records arrive once,
  not from "now" and not twice.
- The output buffers in the state directory up to a configured size and retries without a budget
  (`retry_limit` set explicitly, see `images/fluentbit/UPSTREAM.md`), as the DaemonSet does.

## Packaging

- The binary is the agent image's fluent-bit (same version and patches, `images/fluentbit/`), built
  in a stage for the host's distribution (Ubuntu 24.04 first) so it links against the host's C
  library, without the `cri_scrape` plugin, which needs the container runtime.
- The release is a tarball with a published SHA-256: `bin/fluent-bit`, the pipeline
  (`etc/host.yaml`), the heartbeat script and the systemd unit. A configuration management tool
  installs it, verifies the checksum, writes the environment file and the credential, and keeps the
  installed files from drifting. The repository does not install anything on a host by itself.

## The systemd unit

- A system user of its own, `SupplementaryGroups=systemd-journal adm` for the journal and the logs.
- `StateDirectory=observability-host`: positions, storage and the output buffer in
  `/var/lib/observability-host`, never the DaemonSet's `/var/lib/node-agent`.
- `LoadCredential=s3:<path>`: the bucket key from a root-only file, handed to the service in its
  credentials directory (`AWS_SHARED_CREDENTIALS_FILE=%d/s3`); it is never in the environment
  file, the unit or the command line.
- `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `NoNewPrivileges=yes`, write access
  only to the state directory.
- `Restart=always`. Stopping or disabling the unit is itself a journal record, which the agent ships
  before it stops, and the heartbeat's silence shows it after.

## Metrics

The same signals SRE staff know from the DaemonSet's nodes, under the same names: fluent-bit's
`node_exporter_metrics` input, configured as in `agent/fluent-bit.yaml` (same collectors, same
interval), so a host's `node_*` series look like a Kubernetes node's and existing dashboards and
alert rules apply. `timex` gives the clock offset (`node_timex_offset_seconds`,
`node_timex_sync_status`), `systemd` the unit states, and `textfile` the restart flag below.

They are served on `HOST_METRICS_LISTEN` (an address on the node, default port 2023), together with
the service's own metrics (input and output records, errors, retries, buffer use). Where to:

- **On a Kubernetes node** the DaemonSet already exports `node_*` for the machine. The host agent
  then serves only the series the DaemonSet cannot see (the restart flag, its own metrics), so no
  series is doubled, and the DaemonSet scrapes `HOST_METRICS_LISTEN` as a static target: they reach
  the node data bucket and the DaemonSet's re-serving endpoint like any other target, and no key
  crosses between the two buckets.
- **On a host without Kubernetes** nothing else exports them: the host agent ships all of them as
  the `metrics` table (the DaemonSet's schema) under its own root, and serves them for a local
  Prometheus.

## Restart needed

Packages that need a reboot (a kernel, libc, systemd) leave `/var/run/reboot-required` and the
list `/var/run/reboot-required.pkgs`. The host agent does not reboot anything; it exposes the flag
as metrics, in the names the Prometheus community's textfile collector scripts use, so a common
alert rule fits:

- `node_reboot_required` 1 when the flag exists, else 0;
- `node_reboot_required_since_seconds` the flag's modification time, so an alert on age survives a
  restart of whatever evaluates it (a rule's `for:` does not);
- `node_reboot_required_packages{package="..."}` 1 per package in the list, so a rule can treat a
  kernel or a package with a known exploited vulnerability differently from the rest.

The heartbeat script writes them every interval to a `.prom` file in the state directory, which the
`textfile` collector reads. This repository defines the metrics; the alert rules (how old a
pending reboot may get) belong to whoever runs the alerting.
## What the itest verifies (host phase, on the qemu node)

The host agent installed from the tarball as a systemd unit, writing to versitygw. What the storage
enforces (Object Lock, retention, which key may do what) is the storage's responsibility and not
tested here; the itest verifies that the agent writes in a way that makes those guarantees possible:

- **Survival:** an authentication event on the host, its local log deleted by root after it was
  read; the event is in the bucket, and the time from event to object is measured.
- **Exactly once:** delivery stopped, events created, the service restarted, and the node rebooted:
  every event arrives, none twice (deduplicated on record identity, as above).
- **Gap:** the service stopped for longer than the heartbeat interval; the counter and the time
  show the gap.
- **Put only:** the agent works with a key allowed nothing but PutObject on its root, and the
  gateway's request log shows no list, read, head or delete from it.
- **Content-MD5:** every upload carries a Content-MD5 header that matches the body.
- **Fresh keys:** an upload that fails and is retried lands under a new key, and no key is written
  twice.
- **Schema:** every host line reads through `compactor/sql/raw.sql` with no field outside its
  columns, and `stream` and `unit` take the values above.
- **Coverage:** one record each of a login, a failed login, an account change, a privileged
  command, a configuration file change, a service stopped and a package installed.

## Not decided here

The receiver: what reads the bucket, the gap and tampering checks and their alarms. The host agent
only makes them possible.
