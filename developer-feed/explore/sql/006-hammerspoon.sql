-- Hammerspoon's activity log (developer-feed/collectors/collectors.d/
-- hammerspoon.yaml, the configuration of productivity-workspaces): which app
-- and window had focus, with titles, Spaces, idle time, lock and sleep
CREATE SCHEMA IF NOT EXISTS hammerspoon;

CREATE OR REPLACE VIEW hammerspoon.events AS
  SELECT (e->>'ts')::TIMESTAMPTZ AS occurred_at, cluster, node,
    e->>'event' AS event,
    e->>'app' AS app,
    e->>'bundle_id' AS bundle_id,
    e->>'title' AS title,
    (e->>'space')::BIGINT AS space,
    (e->>'idle_s')::DOUBLE AS idle_s,
    (e->>'screen_active')::BOOLEAN AS screen_active,
    e
  FROM (SELECT cluster, node, (line->>'json')::JSON AS e FROM ingest.rows WHERE "table" = 'hammerspoon');

-- every focus and title change, lasting until the machine's next event of
-- any kind (a lock or sleep ends it as well)
CREATE OR REPLACE VIEW hammerspoon.focus AS
  SELECT * FROM (
    SELECT occurred_at, cluster, node, event, app, bundle_id, title, space,
      lead(occurred_at) OVER (PARTITION BY cluster, node ORDER BY occurred_at) - occurred_at AS duration
    FROM hammerspoon.events
    WHERE event NOT IN ('sample', 'code_window_focused', 'code_window_title_changed')
  )
  WHERE event IN ('window_focused', 'window_title_changed');
