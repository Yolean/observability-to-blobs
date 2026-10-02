-- One view per table over `raw` (raw.sql): the rows the compactor writes,
-- before DISTINCT and sorting, plus filename, the key a row came from,
-- which the compactor drops. logs.time is coalesce(time, ts): journal rows
-- have only the microsecond ts. Rows with no namespace (the node's journal,
-- node-level scrapes) get the pseudo-namespace _node, which no Kubernetes
-- namespace can be. metrics is one row per sample.
--
-- Names resolve when a query runs, and a temp table takes precedence over
-- a view of the same name, even qualified as main.<name> (temp tables live
-- in the temp catalog's schema main). A fold uses that: CREATE TEMP TABLE
-- raw AS SELECT * FROM raw reads the view once, because the temp table
-- does not exist until the statement completes, and these views then read
-- that table instead of the objects.
CREATE OR REPLACE VIEW logs AS
  SELECT coalesce(time, ts) AS time, cluster, node, coalesce(namespace, '_node') AS namespace,
         pod, pod_uid, container, restart, stream, unit, priority, message, filename
  FROM raw WHERE "table" = 'logs';

CREATE OR REPLACE VIEW metrics AS
  SELECT time, cluster, node, job, instance, coalesce(namespace, '_node') AS namespace,
         pod, pod_uid, container, restart, s.name AS name, s.type AS type, s.labels AS labels, s.value AS value,
         filename
  FROM (SELECT *, unnest(samples) AS s FROM raw WHERE "table" = 'metrics');

CREATE OR REPLACE VIEW podindex AS
  SELECT time, cluster, node, kind, namespace, pod, pod_uid, container, container_id, image, labels, pod_started,
         qos_class, pod_ip, restart, image_id, container_started, memory_limit, cpu_quota, cpu_shares,
         exit_code, exit_reason, exit_message, filename
  FROM raw WHERE "table" = 'podindex';
