-- Claude Code: session and subagent transcripts, prompt history, memories
-- (developer-feed/collectors/collectors.d/claude.yaml, claude-memory.yaml).
-- The transcript format is Claude Code's own and changes between versions:
-- entries keep the whole line as JSON.
CREATE SCHEMA IF NOT EXISTS claude;

CREATE OR REPLACE VIEW claude.lines AS
  SELECT ts, cluster, node, kind,
    line->>'path' AS path,
    line->>'project' AS project,
    line->>'session_id' AS session_id,
    (line->>'json')::JSON AS entry,
    filename
  FROM ingest.rows
  WHERE "table" = 'claude' AND kind IN ('transcript', 'subagent', 'history');

-- a transcript's or subagent transcript's entries
CREATE OR REPLACE VIEW claude.entries AS
  SELECT cluster, node, kind, project,
    coalesce(session_id, entry->>'sessionId') AS session_id,
    entry->>'type' AS type,
    entry->>'uuid' AS uuid,
    entry->>'parentUuid' AS parent_uuid,
    (entry->>'timestamp')::TIMESTAMPTZ AS occurred_at,
    entry->>'cwd' AS cwd,
    entry->>'gitBranch' AS git_branch,
    entry
  FROM claude.lines
  WHERE kind IN ('transcript', 'subagent');

-- what the developer typed: transcripts' user entries whose content is text
-- (not tool results, not the harness's meta entries), and the history's
-- prompts, which overlap them
CREATE OR REPLACE VIEW claude.prompts AS
  SELECT occurred_at, cluster, node, session_id, cwd AS project_path, git_branch,
    entry->'message'->>'content' AS prompt, 'transcript' AS source
  FROM claude.entries
  WHERE type = 'user' AND kind = 'transcript'
    AND json_type(entry->'message'->'content') = 'VARCHAR'
    AND coalesce((entry->>'isMeta')::BOOLEAN, false) = false
  UNION ALL
  SELECT to_timestamp((entry->>'timestamp')::BIGINT / 1000), cluster, node,
    entry->>'sessionId', entry->>'project', NULL,
    entry->>'display', 'history'
  FROM claude.lines
  WHERE kind = 'history';

-- every tool call, with its input: Write and Edit carry files written,
-- Bash the commands (git commit, git push, gh pr create)
CREATE OR REPLACE VIEW claude.tool_calls AS
  SELECT occurred_at, cluster, node, kind, session_id, cwd,
    c->>'name' AS tool, c->>'id' AS tool_use_id, c->'input' AS input
  FROM (
    SELECT occurred_at, cluster, node, kind, session_id, cwd,
      unnest(json_extract(entry, '$.message.content[*]')) AS c
    FROM claude.entries
    WHERE type = 'assistant'
  )
  WHERE c->>'type' = 'tool_use';

CREATE OR REPLACE VIEW claude.sessions AS
  SELECT session_id, any_value(cluster) AS cluster, any_value(project) AS project,
    min(occurred_at) AS first_at, max(occurred_at) AS last_at, count(*) AS entries,
    max(entry->>'aiTitle') AS title
  FROM claude.entries
  GROUP BY session_id;

-- every version of every memory file, and the current ones
CREATE OR REPLACE VIEW claude.memory_versions AS
  SELECT ts, cluster, node,
    line->>'path' AS path,
    line->>'project' AS project,
    line->>'sha256' AS sha256,
    (line->>'deleted')::BOOLEAN AS deleted,
    line->>'content' AS content
  FROM ingest.rows
  WHERE "table" = 'claude' AND kind = 'memory';

CREATE OR REPLACE VIEW claude.memories AS
  SELECT * EXCLUDE (deleted) FROM (
    SELECT * FROM claude.memory_versions
    QUALIFY row_number() OVER (PARTITION BY cluster, path ORDER BY ts DESC) = 1
  )
  WHERE NOT deleted;
