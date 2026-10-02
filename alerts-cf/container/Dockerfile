# Build context is the repo root (wrangler.jsonc image_build_context), so
# the tailer and the demo rules come from their canonical locations.
FROM docker.io/library/golang:1.26 AS tailer
WORKDIR /src
COPY images/blobs-to-prometheus/ ./
RUN CGO_ENABLED=0 go build -o /blobs-to-prometheus .

FROM docker.io/prom/prometheus:v3.14.0 AS prometheus
FROM docker.io/prom/alertmanager:v0.28.1 AS alertmanager

FROM docker.io/library/debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/*
COPY --from=prometheus /bin/prometheus /bin/prometheus
COPY --from=alertmanager /bin/alertmanager /bin/alertmanager
COPY --from=tailer /blobs-to-prometheus /bin/blobs-to-prometheus
COPY local/rules/ /etc/alerts/rules/
COPY alerts-cf/container/prometheus.yml alerts-cf/container/alertmanager.yml /etc/alerts/
COPY alerts-cf/container/entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
