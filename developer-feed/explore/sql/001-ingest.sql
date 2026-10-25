-- The explore database's ingest (developer-feed/explore/README.md): every
-- line of every object explore.sh reads, raw, and the objects it has read.
-- Lines stay JSON, so a change of the feed's line contract never breaks
-- ingestion; the schemas of the later files are views over them.
CREATE SCHEMA IF NOT EXISTS ingest;
CREATE TABLE IF NOT EXISTS ingest.files (filename VARCHAR PRIMARY KEY, ingested_at TIMESTAMPTZ);
CREATE TABLE IF NOT EXISTS ingest.lines (filename VARCHAR, line JSON);
CREATE TABLE IF NOT EXISTS ingest.migrations (version VARCHAR PRIMARY KEY, applied_at TIMESTAMPTZ);
-- an ingest each: from what hour, of which clusters, and how long it took
-- (an object's ingested_at is the start of its day's transaction)
CREATE TABLE IF NOT EXISTS ingest.runs (started_at TIMESTAMPTZ, finished_at TIMESTAMPTZ,
  from_hour VARCHAR, clusters VARCHAR, days INTEGER, new_objects BIGINT);

-- the columns every line has (developer-feed/sql/feed.sql)
CREATE OR REPLACE VIEW ingest.all_rows AS
  SELECT filename,
    line->>'table' AS "table",
    line->>'kind' AS kind,
    line->>'cluster' AS cluster,
    line->>'node' AS node,
    (line->>'ts')::TIMESTAMPTZ AS ts,
    line->>'backfill' AS backfill,
    line
  FROM ingest.lines;

-- every line once: a line can be shipped twice, by the agent and by
-- dev-feed-backfill where their ranges meet (developer-feed/backfill/), or
-- by a collector stopped between printing it and saving its position. Its
-- identity is what the source wrote, not when it was read; the first ts
-- wins. Memory and remote rows are state: the same value again is a
-- version, and their views take the latest.
CREATE OR REPLACE VIEW ingest.rows AS
  SELECT * EXCLUDE (identity) FROM (
    SELECT *,
      CASE
        WHEN "table" = 'claude' AND kind IN ('transcript', 'subagent', 'history')
          THEN concat_ws(chr(31), kind, line->>'path', line->>'json')
        WHEN "table" = 'hammerspoon' THEN concat_ws(chr(31), line->>'path', line->>'json')
        WHEN "table" = 'cmux' THEN line->>'event_id'
        WHEN "table" = 'git' AND kind = 'ref-update'
          THEN concat_ws(chr(31), kind, line->>'repo', line->>'ref', line->>'sha', line->>'reflog_time', line->>'message')
        WHEN "table" = 'git' AND kind = 'commit' THEN concat_ws(chr(31), kind, line->>'repo', line->>'sha')
      END AS identity
    FROM ingest.all_rows
  )
  QUALIFY identity IS NULL
    OR row_number() OVER (PARTITION BY cluster, node, "table", md5(identity) ORDER BY ts, filename) = 1;
