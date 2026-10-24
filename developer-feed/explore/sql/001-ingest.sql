-- The explore database's ingest (developer-feed/explore/README.md): every
-- line of every object explore.sh reads, raw, and the objects it has read.
-- Lines stay JSON, so a change of the feed's line contract never breaks
-- ingestion; the schemas of the later files are views over them.
CREATE SCHEMA IF NOT EXISTS ingest;
CREATE TABLE IF NOT EXISTS ingest.files (filename VARCHAR PRIMARY KEY, ingested_at TIMESTAMPTZ);
CREATE TABLE IF NOT EXISTS ingest.lines (filename VARCHAR, line JSON);
CREATE TABLE IF NOT EXISTS ingest.migrations (version VARCHAR PRIMARY KEY, applied_at TIMESTAMPTZ);

-- the columns every line has (developer-feed/sql/feed.sql)
CREATE OR REPLACE VIEW ingest.rows AS
  SELECT filename,
    line->>'table' AS "table",
    line->>'kind' AS kind,
    line->>'cluster' AS cluster,
    line->>'node' AS node,
    (line->>'ts')::TIMESTAMPTZ AS ts,
    line
  FROM ingest.lines;
