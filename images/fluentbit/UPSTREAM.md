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
