-- The cluster's objects as they were at a time, from the objects table
-- (K8SAGENT.md): the raw view of compactor/sql/tables.sql, or the hourly
-- tier's objects object of the hour and the hour before it read into a
-- table or view named objects.
--
--   .read local/objects_at.sql
--   SELECT * FROM objects_at(TIMESTAMP '2026-10-07 10:12:00');
--
-- Per resource, the base is the latest complete snapshot at or before t:
-- the object-snapshot rows with the time of that resource's latest
-- snapshot row. The events after it, up to t, are laid over it; per object
-- the latest row wins, and an object whose latest row is its deletion is
-- gone. An object the agent never saw deleted, because it was not running,
-- is gone too, from the first snapshot without it. A resource with no
-- complete snapshot at or before t is unknown at t and has no rows: it was
-- not watched yet, or the hour before t is missing.
--
-- A resource is (api_group, resource): when the server's preferred version
-- of a resource changes, the snapshot after the change is the base.
CREATE OR REPLACE MACRO objects_at(t) AS TABLE
  WITH snapshots AS (
    SELECT api_group, resource, max(time) AS snapshot
    FROM objects WHERE kind = 'snapshot' AND time <= t
    GROUP BY ALL
  ), candidates AS (
    SELECT o.* FROM objects o JOIN snapshots s USING (api_group, resource)
    WHERE (o.kind = 'object-snapshot' AND o.time = s.snapshot)
       OR (o.kind IN ('object-added', 'object-modified', 'object-deleted') AND o.time > s.snapshot AND o.time <= t)
  ), latest AS (
    SELECT *, row_number() OVER (
      PARTITION BY api_group, resource, namespace, name
      -- a deletion observed at the same instant as a change is the later
      -- event; resourceVersion breaks the remaining ties
      ORDER BY time DESC, kind = 'object-deleted' DESC, TRY_CAST(resource_version AS UBIGINT) DESC NULLS LAST
    ) AS n
    FROM candidates
  )
  SELECT time, kind, api_group, api_version, resource, namespace, name, object_uid, resource_version, object
  FROM latest WHERE n = 1 AND kind <> 'object-deleted';
