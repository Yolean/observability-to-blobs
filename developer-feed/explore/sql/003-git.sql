-- git (developer-feed/git/main.go): remotes, reflog entries, commits
CREATE SCHEMA IF NOT EXISTS git;

CREATE OR REPLACE VIEW git.remotes AS
  SELECT * FROM (
    SELECT ts, cluster, node, line->>'repo' AS repo, line->>'remote' AS remote,
      line->>'fetch_url' AS fetch_url, line->>'push_url' AS push_url
    FROM ingest.rows WHERE "table" = 'git' AND kind = 'remote'
  )
  QUALIFY row_number() OVER (PARTITION BY cluster, repo, remote ORDER BY ts DESC) = 1;

-- a push from this clone is "update by push", someone's push that this
-- clone fetched is "fetch ..."
CREATE OR REPLACE VIEW git.ref_updates AS
  SELECT (line->>'reflog_time')::TIMESTAMPTZ AS occurred_at, cluster, node,
    line->>'repo' AS repo, line->>'ref' AS ref, line->>'sha' AS sha,
    line->>'message' AS message
  FROM ingest.rows WHERE "table" = 'git' AND kind = 'ref-update';

CREATE OR REPLACE VIEW git.commits AS
  SELECT (line->>'committer_time')::TIMESTAMPTZ AS occurred_at, cluster, node,
    line->>'repo' AS repo, line->>'sha' AS sha,
    from_json(line->'parents', '["VARCHAR"]') AS parents,
    line->>'author_name' AS author_name, line->>'author_email' AS author_email,
    (line->>'author_time')::TIMESTAMPTZ AS author_time,
    line->>'message' AS message,
    split_part(line->>'message', chr(10), 1) AS subject,
    contains(line->>'message', 'Co-Authored-By: Claude') AS claude
  FROM ingest.rows WHERE "table" = 'git' AND kind = 'commit';
