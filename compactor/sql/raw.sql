-- The raw tier's line contract: every line of every blob named by the
-- session variable raw_files, with the column types every consumer pins.
-- Fields not named here are ignored on read; fields named here but absent
-- on a line are NULL. The itest asserts this list against what the agent
-- emits, in both directions.
--
-- raw_files is a glob or a list of keys: the itest and local/blobs.sh
-- pass a glob, a compactor fold the list of keys it has not folded yet.
--
--   SET VARIABLE raw_files = 's3://<bucket>/raw1/<cluster>/*/*/*/*.jsonl.zst';
--   .read compactor/sql/raw.sql
--
-- The view reads the variable when it is queried, so a fold only sets it
-- to the keys it folds.
CREATE OR REPLACE VIEW raw AS
  SELECT * FROM read_json(getvariable('raw_files'), format='newline_delimited', compression='zstd', filename=true,
    columns={
      -- every line
      "table":'VARCHAR',            -- logs | metrics | podindex
      time:'TIMESTAMP_NS',          -- the source's own stamp, ns; absent on journal rows
      ts:'TIMESTAMP_NS',            -- event time rendered by out_s3, us, on every line
      cluster:'VARCHAR',
      node:'VARCHAR',
      -- logs
      namespace:'VARCHAR',          -- absent on journal rows
      pod:'VARCHAR',
      pod_uid:'VARCHAR',            -- the incarnation; also on metrics and podindex
      container:'VARCHAR',
      restart:'INTEGER',            -- kubelet restart count: logs from the log path, metrics from
                                    -- the scraped container (absent when the target names none)
      stream:'VARCHAR',             -- stdout | stderr | journal | file (host mode, HOST_MODE.md)
      unit:'VARCHAR',               -- journal rows: systemd unit, syslog identifier or transport;
                                    -- file rows: the file's path
      priority:'INTEGER',           -- journal rows: syslog priority
      message:'VARCHAR',
      -- metrics: one line per scrape
      job:'VARCHAR',                -- pod | node
      instance:'VARCHAR',           -- ip:port
      samples:'STRUCT(name VARCHAR, type VARCHAR, labels MAP(VARCHAR, VARCHAR), value DOUBLE)[]',
      -- podindex: pod-observed | pod-checkpoint | pod-deleted | container | container-exit
      kind:'VARCHAR',
      container_id:'VARCHAR',       -- container rows: the runtime's id of this run, bare
                                    -- (containerStatuses[].containerID without <runtime>://)
      image:'VARCHAR',              -- container rows, as written in the pod spec
      labels:'MAP(VARCHAR, VARCHAR)', -- pod-* rows: pod labels
      pod_started:'TIMESTAMP_NS',   -- pod-* rows: sandbox creation, constant across restarts
      qos_class:'VARCHAR',          -- pod-* rows: besteffort | burstable | guaranteed
      pod_ip:'VARCHAR',             -- pod-* rows
      image_id:'VARCHAR',           -- container rows: the resolved digest
      container_started:'TIMESTAMP_NS',
      memory_limit:'BIGINT',        -- container rows: limits in force, absent when unlimited
      cpu_quota:'BIGINT',
      cpu_shares:'BIGINT',
      exit_code:'INTEGER',          -- container-exit rows
      exit_reason:'VARCHAR',
      exit_message:'VARCHAR'
    });
