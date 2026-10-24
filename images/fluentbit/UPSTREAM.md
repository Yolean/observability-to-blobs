# Notes for upstream

Findings against fluent-bit that are not specific to this fork.

## http client: header lookup matches inside other header names (patch 0001)

`header_lookup()` uses `strcasestr` on the whole response, so
`Content-Length` matches inside `x-goog-stored-content-length` (GCS sends
that one first). Carried over from kubernetes-logs-datalake; still present
in v5.1.2.

## out_s3: `retry_limit no_limits` is capped to 5

`cb_s3_init` logs "retry_limit set to unlimited, capping to 5" and sets
`ins->retry_limit = MAX_UPLOAD_ERRORS`. With the upload queue's retry
cadence of `2 s * attempts` the fifth failure comes ~30 s after the first,
and the chunk is then quarantined (default `retry_exhausted_action`).
So an S3 output configured for unlimited retries loses every chunk that
was due during an outage longer than half a minute, silently apart from
one warn line. Measured 2026-09-22 on v5.1.2 with a 100 s outage: two
chunks quarantined, the rest shipped. An explicit large integer is
honoured, which is what this fork uses; the cap should either go, or the
documentation of `retry_limit` should say that out_s3 ignores
`no_limits`.

## out_s3: possible use-after-free when a queued chunk exhausts retries

Found on v5.1.1 with `format parquet` when every conversion failed: a chunk
sat in `upload_queue` more than once (each flush that found it due and
unlocked added another entry) and `s3_chunk_retry_exhausted_cleanup()`
freed it without removing the other entries; fluent-bit then aborted at
exit in `s3_context_destroy` -> `s3_store_file_delete` -> `mk_list_del`.
v5.1.2 reordered the caller (`remove_from_queue` before the cleanup) but
`add_to_queue()` still does not check for an existing entry. This fork
runs `retry_limit 100000` (out_s3 caps `no_limits` to 5, above), so the
exhaustion path is not taken in practice; the report is worth
re-reproducing on stock v5.1.2 with a failing endpoint and a small
`retry_limit` before filing.

## out_s3: upload timer logs at info every tick

`cb_s3_upload()` logs "Running upload timer callback (cb_s3_upload).." at
info; with `upload_timeout 30s` that is every 6 s.

## in_node_exporter_metrics: one error line per missing sysfs file per scrape

The cpufreq collector logs `could not open '/host/sys/devices/system/cpu/
cpu0/cpufreq/cpuinfo_cur_freq'` and five siblings per CPU on every scrape
when the tree is absent (VMs, many cloud nodes). Once per missing file, or
debug level, would do. Worked around by leaving the collector out of
`metrics`.

## in_node_exporter_metrics: the textfile collector never forgets a series

Each scrape decodes the `.prom` files and concatenates the result into the
input's long-lived metrics context, so a series that disappears from a file
(a label set that is no longer written) stays in the exposition, at its last
value, until the process restarts. Seen on v5.1.2 with the host agent's
`node_reboot_required_packages{package=...}` after the flag file was removed
without a restart. Replacing the context per scrape, or per file, would do.

## in_node_exporter_metrics: a directory path with a dot in it is not a directory

`textfile_update()` looks for the last `.` in `collector.textfile.path`; when
what follows is not `.prom` it `stat()`s the path and only a regular file is
treated as such, so a directory like `/var/lib/example.d/metrics` falls through
with `use_directory_pattern` left false and is scanned as one file. Read in the
source of v5.1.2, not reproduced; the host agent's state directory has no dot.

## in_tail: an unreadable directory in a glob is an error every refresh

A `path` glob such as `/var/log/unattended-upgrades/*.log` whose directory the
process may not read logs `read error, check permissions` and `error scanning
path` at every `refresh_interval` (5 s here), where a glob that matches nothing
is silent. Seen on v5.1.2 as an unprivileged user.

## YAML config: a list of more than 10 items in a processor's settings is refused

A processor setting given as a YAML sequence, such as `record_modifier`'s
`allowlist_key` with 11 keys, fails the whole configuration with "unable
to add key to list map" and "yaml error", attributed to the including file
rather than the one with the list. `flb_cf_yaml.c` (v5.1.2, around line
2928) makes the variant array of a sequence with `cfl_array_create(10)` and
never calls `cfl_array_resizable()`, as the code at line 730 does for its
own array; the 11th `cfl_array_append` fails. Found with
developer-feed's collectors; worked around there by not listing more than
10. The fix is one line: make the array resizable.
