-- An example of your own schema in explore (developer-feed/macos/README.md,
-- Your own sources), for the worklog example's rows: copy to
-- ~/.config/observability-host/v1/explore/. explore.sh applies it after its
-- own schemas, at every start, so it must be repeatable: CREATE OR REPLACE.
--
-- An agent analyzing the feed reads this file to learn what the rows mean
-- (explore/ANALYZE.md): say it here. A COMMENT ON VIEW does not reach a
-- client over quack.
--
-- worklog: notes the developer appends to ~/worklog.jsonl while working,
-- one JSON object a line: at (when, RFC 3339), text (what).
CREATE SCHEMA IF NOT EXISTS worklog;

CREATE OR REPLACE VIEW worklog.entries AS
  SELECT (e->>'at')::TIMESTAMPTZ AS occurred_at, cluster, node,
    e->>'text' AS text,
    e
  FROM (SELECT cluster, node, (line->>'json')::JSON AS e FROM ingest.rows WHERE "table" = 'worklog');
