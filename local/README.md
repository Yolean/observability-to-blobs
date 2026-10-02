# Local ad-hoc and alerts stacks

Both stacks read the bucket that `./test.sh up` (or a completed
`./test.sh itest`) leaves reachable on NodePort 30070 (`S3_PORT`), and
run on this machine, outside the cluster: a receiver-mode Prometheus in
plain docker (published port, no host networking, so macOS works) plus
the tailer as a host process. Commands assume the repo root as cwd.

**The tailer is not wired to the current bucket layout yet** (README.md,
"Known gaps"). It reads the PoC keys `{cluster}/{node}/{date}/{HH}/{MM}/metrics-N.parquet`
and discovers nodes by listing `{cluster}/`; today's agent writes
`raw1/{cluster}/...jsonl.zst`, so against a current `./test.sh up` bucket
it finds no nodes and pushes nothing, and Prometheus stays empty. The two
stacks below work only against a PoC-era bucket.

## Ad-hoc (everything the agents ship)

    docker run --rm --name local-prom -p 9090:9090 -v $PWD/local:/etc/prometheus \
      docker.io/prom/prometheus:v3.14.0 \
    --config.file=/etc/prometheus/prometheus.yml --web.enable-remote-write-receiver

    (cd images/blobs-to-prometheus && S3_ENDPOINT=http://127.0.0.1:30070 \
      S3_TLS=off S3_BUCKET=node-agent CLUSTER_NAME=itest \
      AWS_ACCESS_KEY_ID=demoaccess AWS_SECRET_ACCESS_KEY=demosecret go run .)

Against a PoC-era bucket, http://localhost:9090/graph then has the last
hour backfilled (BACKFILL, default 1h; prometheus.yml's
out_of_order_time_window must cover it) and new windows arriving every
POLL=15s, autocomplete included. Every query
is logged (prometheus.yml `query_log_file`) -- a trace of what each
chart evaluated:

    docker exec local-prom tail -f /prometheus/query.log

## Alerts (only what rules need)

rules/demo.yml is written against the sample workloads and is already
loaded by prometheus.yml above. With a PoC-era bucket, ScrapeTargetDown
shows up firing on http://localhost:9090/alerts once the backfill lands;
with a current one there is no data, so nothing fires.

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
plus `hourly_logs`, `hourly_metrics` and `pods` once the compactor has
written them. It defaults to the itest bucket; export `S3_ENDPOINT`,
`S3_TLS`, `S3_REGION` and the AWS keys for another one.

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
