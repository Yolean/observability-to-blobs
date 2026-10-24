-- cmux's agent journal (developer-feed/bin/dev-feed-cmux): which agent
-- session ran in which workspace and pane, and when it worked
CREATE SCHEMA IF NOT EXISTS cmux;

CREATE OR REPLACE VIEW cmux.journal AS
  SELECT (line->>'occurred_at')::TIMESTAMPTZ AS occurred_at, cluster, node,
    kind, (line->>'sequence')::BIGINT AS sequence, line->>'event_id' AS event_id,
    line->>'source' AS source, line->>'session_id' AS session_id,
    line->>'workspace_id' AS workspace_id, line->>'surface_id' AS surface_id,
    line->>'native_event' AS native_event, line->>'declared_phase' AS declared_phase,
    (line->>'is_subagent')::BOOLEAN AS is_subagent
  FROM ingest.rows WHERE "table" = 'cmux';
