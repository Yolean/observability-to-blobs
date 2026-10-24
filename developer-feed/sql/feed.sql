-- The developer feed's line contract: every line of every object named by
-- the session variable raw_files, under a feed's root (dev1/), with the
-- columns its readers pin. The host agent's rows (heartbeat, files) have
-- the raw tier's columns (compactor/sql/raw.sql); the feed's collectors
-- add the columns below them. Fields not named here are ignored on read;
-- fields named here but absent on a line are NULL.
--
--   SET VARIABLE raw_files = 's3://<bucket>/dev1/<cluster>/*/*/*/*.jsonl.zst';
--   .read developer-feed/sql/feed.sql
CREATE OR REPLACE VIEW feed AS
  SELECT * FROM read_json(getvariable('raw_files'), format='newline_delimited', compression='zstd', filename=true,
    columns={
      -- every line
      "table":'VARCHAR',            -- logs | metrics (the host agent's) | claude | git | cmux | hammerspoon
      time:'TIMESTAMP_NS',          -- the source's own stamp, ns, where it has one
      ts:'TIMESTAMP_NS',            -- when the agent read the line, us, on every line
      cluster:'VARCHAR',            -- the feed
      node:'VARCHAR',               -- the machine
      -- the host agent's rows, as in raw.sql
      stream:'VARCHAR',
      unit:'VARCHAR',
      priority:'INTEGER',
      message:'VARCHAR',
      scrape:'VARCHAR',
      instance:'VARCHAR',
      samples:'STRUCT(name VARCHAR, type VARCHAR, labels MAP(VARCHAR, VARCHAR), value DOUBLE)[]',
      -- claude: a line of Claude Code's, verbatim
      kind:'VARCHAR',               -- transcript | subagent | history | memory
      path:'VARCHAR',               -- the file the line is from
      project:'VARCHAR',            -- transcript, subagent: the projects/ directory, the session's cwd with / as -
      session_id:'VARCHAR',         -- transcript, subagent: from the file's name; history: in the line
      json:'VARCHAR',               -- the line; its format is Claude Code's own and changes between versions
      -- claude memory: a snapshot of a memory file (developer-feed/bin/dev-feed-snapshot)
      content:'VARCHAR',            -- the file's whole content, when new or changed
      sha256:'VARCHAR',
      deleted:'BOOLEAN',            -- true: the file is gone, and the row has no content
      -- git (kind remote | ref-update | commit; developer-feed/git/main.go)
      repo:'VARCHAR',               -- the repository's top level on the machine
      remote:'VARCHAR',             -- remote rows
      fetch_url:'VARCHAR',
      push_url:'VARCHAR',
      ref:'VARCHAR',                -- ref-update rows: refs/heads/... or refs/remotes/...
      sha:'VARCHAR',                -- ref-update: the new tip; commit: the commit
      reflog_time:'TIMESTAMP',      -- ref-update rows: the reflog entry's time, UTC
      parents:'VARCHAR[]',          -- commit rows
      author_name:'VARCHAR',
      author_email:'VARCHAR',
      author_time:'TIMESTAMPTZ',
      committer_name:'VARCHAR',
      committer_email:'VARCHAR',
      committer_time:'TIMESTAMPTZ',
      -- message (above): a ref-update's reflog message, a commit's whole message
      -- cmux: a row of cmux's agent journal (developer-feed/bin/dev-feed-cmux); kind is
      -- cmux's, such as agent.session.started or agent.turn.completed, and session_id
      -- (above) the agent's session
      sequence:'BIGINT',            -- the journal's own order
      event_id:'VARCHAR',
      schema_version:'INTEGER',
      occurred_at:'TIMESTAMP',      -- UTC, ms
      committed_at:'TIMESTAMP',
      source:'VARCHAR',             -- the agent: claude, ...
      agent_key:'VARCHAR',
      workspace_id:'VARCHAR',       -- the cmux workspace
      surface_id:'VARCHAR',         -- the cmux pane's surface
      unattributed_reason:'VARCHAR',
      is_subagent:'BOOLEAN',
      pending_work:'BOOLEAN',
      native_event:'VARCHAR',       -- the hook it came from: SessionStart, UserPromptSubmit, PreToolUse, Stop, ...
      declared_phase:'VARCHAR'
    });
