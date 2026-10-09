# Local ad-hoc and alerts stacks

Both stacks read the bucket that `./test.sh up` (or a completed
`./test.sh itest`) leaves reachable on NodePort 30070 (`S3_PORT`), and
run on this machine, outside the cluster: a receiver-mode Prometheus in
plain docker (published port, no host networking, so macOS works) plus
the tailer as a host process. Commands assume the repo root as cwd.

The tailer reads the raw tier, `raw1/{cluster}/{date}/{HH}/{node}/*.jsonl.zst`
(images/blobs-to-prometheus/main.go), so it sees what the raw tier still
holds: everything of a `./test.sh up` bucket, whose compactor keeps raw
objects, and the last one to two hours where the compactor runs with
RAW_DELETE=after-final.

## Ad-hoc (everything the agents ship)

    docker run --rm --name local-prom -p 9090:9090 -v $PWD/local:/etc/prometheus \
      docker.io/prom/prometheus:v3.14.0 \
    --config.file=/etc/prometheus/prometheus.yml --web.enable-remote-write-receiver

    (cd images/blobs-to-prometheus && S3_ENDPOINT=http://127.0.0.1:30070 \
      S3_TLS=off S3_BUCKET=node-agent CLUSTER_NAME=itest \
      AWS_ACCESS_KEY_ID=demoaccess AWS_SECRET_ACCESS_KEY=demosecret go run .)

http://localhost:9090/graph then has the raw objects of the last
hours backfilled (LOOKBACK, default 3h, from the start of its first hour;
prometheus.yml's out_of_order_time_window must cover it plus that hour)
and new objects arriving every POLL=15s, autocomplete included. Every query
is logged (prometheus.yml `query_log_file`) -- a trace of what each
chart evaluated:

    docker exec local-prom tail -f /prometheus/query.log

## Alerts (only what rules need)

rules/demo.yml is written against the sample workloads and is already
loaded by prometheus.yml above. ScrapeTargetDown shows up firing on
http://localhost:9090/alerts once the backfill lands.

The alerts stack differs from ad-hoc in ONE input: the tailer gets a
manifest of the selectors those rules evaluate, and pushes nothing else:

    (cd tools/rules-manifest && go run . ../../local/rules) > /tmp/manifest.json

then add `SELECTORS_FILE=/tmp/manifest.json` to the tailer command from
the ad-hoc stack. With the filter on, /graph autocompletes only `up` and
`app_requests_total`: the difference between the two stacks made
visible. Point the tool at any rules tree (for example a checkout of
real production rules) to size what an alerts-only consumer would pull.

## Looking at the bucket

`local/blobs.sh` opens a DuckDB session with a view per table: `raw`,
`logs`, `metrics` (samples unnested) and `podindex` over the raw tier,
plus `hourly_logs`, `hourly_metrics`, `hourly_podindex` and `pods` once the compactor has
written them. It defaults to the itest bucket; export `S3_ENDPOINT`,
`S3_TLS`, `S3_REGION` and the AWS keys for another one. For encrypted
tiers export `PARQUET_KEYS_DIR`, a directory of key files laid out as the
compactor mounts them (the itest's: `target/itest/parquet-keys`); each
hourly view then unites a read of the plain objects with one per key
(`compactor/README.md`, "Encryption"), and objects under a key the
directory lacks are left out with a warning.

    local/blobs.sh                                   # interactive, whole cluster
    local/blobs.sh itest/2026-09-22/14/<node>        # raw views scoped to one node-hour
    local/blobs.sh -c "SELECT pod, count(*) FROM logs GROUP BY 1 ORDER BY 2 DESC LIMIT 10"
    local/blobs.sh -ui                               # the DuckDB UI, views in the sidebar

The raw tier is not a query tier: an unscoped view LISTs and opens every
object of the cluster. Scope to a node-hour for anything exploratory, and
read the hourly views for anything else.

Ctrl-C both processes above to tear down. Prometheus state is ephemeral
by design: the bucket is the store, and cold start from it is a drill,
not an incident.
