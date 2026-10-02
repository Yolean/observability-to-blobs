# alerts-cf

The alerting stack as one always-on Cloudflare container next to the
registry: stock Prometheus (receiver mode, the local/ demo rules) +
Alertmanager + the blobs-to-prometheus tailer, reading the test- R2
bucket. test.sh always ships to versitygw; to ship to R2 instead, apply
the R2 variant over a `./test.sh up` cluster (a later `./test.sh` run
re-applies itest/k8s and switches back):

```
export KUBECONFIG=$PWD/itest/cluster/kubeconfig   # from the repo root
kubectl --context=node-agent apply -k itest/k8s-r2  # itest/k8s-docker-r2 on the docker backend
```

The tailer is not wired to the current bucket layout yet (README.md,
"Known gaps"): it reads the PoC `{cluster}/{node}/.../metrics-N.parquet` keys,
and the agent now writes `raw1/{cluster}/...jsonl.zst`, so against a
bucket the current agent ships to it pushes nothing and no rule fires.
Everything below that mentions backfill or firing alerts holds only for
a PoC-era bucket.

## One-time setup

Two credentials, dashboard-minted (registry conventions: per-project
tokens, gitignored `.env`, no `wrangler login`):

1. Worker deploy token into `.env` (copy `.env.example`; the exact
   dashboard path and the two permissions to add are in its comment).
2. R2 Account API token, Object Read & Write, scoped to ONLY the
   node-agent-itest bucket: into `../itest/r2/.env.r2` for the agent,
   with the account's endpoint as `S3_ENDPOINT` (and in `wrangler.jsonc`),
   and the same pair as Worker secrets for the tailer.

```
pnpm install
pnpm exec wrangler r2 bucket create node-agent-itest --jurisdiction eu
pnpm exec wrangler secret put R2_ACCESS_KEY_ID
pnpm exec wrangler secret put R2_SECRET_ACCESS_KEY
```

EU-jurisdiction buckets are invisible to plain `wrangler r2 bucket list`;
add `--jurisdiction eu`.

## Iterate

```
pnpm exec wrangler deploy
```

A config or rules change is an image rebuild and redeploy; every redeploy
is a cold-start drill (backfill from the bucket, `for:` clauses re-arm).
The first request starts the instance, and

```
curl https://y-alerts.<your-subdomain>.workers.dev/watchdog/arm
```

arms the 60 s keepalive + health loop. The worker root is the Prometheus
UI -- no auth yet, so nothing but demo data goes behind it until
Cloudflare Access fronts it.

Local smoke test, no tokens needed (from the repo root, against the
versitygw NodePort of a running `./test.sh up` cluster). Against such a
bucket it shows only that the container comes up: the tailer finds no PoC
keys there, so Prometheus gets no data and no alert fires:

```
docker build -f alerts-cf/container/Dockerfile -t alerts-cf:dev .
docker run --rm -p 9090:9090 \
  -e S3_ENDPOINT=http://host.docker.internal:30070 -e S3_TLS=off \
  -e S3_BUCKET=node-agent -e CLUSTER_NAME=itest \
  -e AWS_ACCESS_KEY_ID=demoaccess -e AWS_SECRET_ACCESS_KEY=demosecret \
  alerts-cf:dev
```

## Clean up everything

The schemas are not final; stopping the local stack ends an iteration and
everything is removable. Two scripts, because the halves differ in what
they cost and what they destroy:

```
./teardown.sh                 # worker, DOs, container application, images
./teardown-bucket.sh          # check the mirror, change nothing
./teardown-bucket.sh --yes    # purge and delete the bucket
```

`teardown.sh` is the one that stops the bill. The container is always-on by
construction -- `onActivityExpired()` is an empty override and the watchdog
renews activity every 60 s -- so `sleepAfter` never applies and memory and
disk bill provisioned for the whole month, ~$12-14 at `basic`. There is no
scaling it down short of deletion: `max_instances` only caps the ceiling,
and the watchdog alarm lives in DO storage, so it survives a redeploy.

Note that **`wrangler delete` does not remove the container application**.
Measured 2026-08-28: after the Worker was gone the application was still
`active` with a `running` instance, and still billing. `wrangler containers
delete <id>` is a separate call, and images are a third; `teardown.sh` does
all three and prints the before/after.

`teardown-bucket.sh` is irreversible -- R2 has no undelete -- so it refuses
unless `local/mirror` already holds at least as many objects as the bucket.
Run the `s3 sync` in its header first; that mirror becomes the only copy.
Stop the agent before it, or the writer re-creates objects between the purge
and the delete and the delete fails on a non-empty bucket.

## Not yet

- Cloudflare Access in front of the UI, and Alertmanager UI routing (9093)
- SELECTORS_FILE manifest filtering and multi-cluster tailers
- Heartbeats to opsgenie/ilert, real rules, instance sizing from the feed
