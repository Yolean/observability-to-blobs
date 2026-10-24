-- the host agent's own rows: the heartbeat, whose silence is a stopped agent
CREATE SCHEMA IF NOT EXISTS host;

CREATE OR REPLACE VIEW host.heartbeats AS
  SELECT (line->>'time')::TIMESTAMPTZ AS occurred_at, cluster, node,
    line->>'$.samples[0].labels.boot_id' AS boot_id,
    (line->>'$.samples[0].value')::DOUBLE AS counter
  FROM ingest.rows WHERE "table" = 'metrics'
    AND line->>'$.samples[0].name' = 'observability_host_heartbeat';
