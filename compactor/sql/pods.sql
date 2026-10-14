-- pods, the investigation index (compactor/README.md): one row per
-- (pod_uid, container, restart) seen in any table; the incarnations of a
-- name are separate rows. Defined over whatever logs, metrics and podindex
-- resolve to when queried: in a fold, the temp tables of those names that
-- hold the tier's deduplicated rows; in a session with only raw.sql and
-- tables.sql, the raw-derived views (a retried upload's duplicate lines
-- then count twice). local/hourly.sql names the hourly tier's own pods view
-- the same, so a session reads one or the other. pods is recomputed from
-- the tables every time, never merged from earlier pods rows.
CREATE OR REPLACE VIEW pods AS
  WITH
  l AS (
    SELECT pod_uid, container, restart, any_value(namespace) AS namespace, any_value(pod) AS pod,
           any_value(node) AS node, min(time) AS first_log, max(time) AS last_log,
           count(*) AS lines, sum(length(message)) AS bytes
    FROM logs WHERE pod_uid IS NOT NULL GROUP BY ALL),
  -- restart is absent on scrapes of a target that names no running
  -- container; those rows keep a NULL restart and join null-safely
  m AS (
    SELECT pod_uid, container, restart, any_value(namespace) AS namespace, any_value(pod) AS pod,
           any_value(node) AS node, list(DISTINCT name ORDER BY name) AS metric_names
    FROM metrics WHERE pod_uid IS NOT NULL AND pod_uid <> '' GROUP BY ALL),
  p AS (
    SELECT pod_uid, any_value(namespace) AS namespace, any_value(pod) AS pod, any_value(node) AS node,
           max_by(labels, time) FILTER (labels IS NOT NULL) AS labels,
           min(pod_started) AS pod_started,
           any_value(qos_class) AS qos_class, any_value(pod_ip) AS pod_ip,
           max(time) AS last_seen,
           max(time) FILTER (kind = 'pod-deleted') AS pod_deleted
    FROM podindex WHERE kind LIKE 'pod-%' GROUP BY pod_uid),
  c AS (
    SELECT pod_uid, container, restart, any_value(container_id) AS container_id,
           any_value(image) AS image, any_value(image_id) AS image_id,
           min(container_started) AS container_started,
           any_value(memory_limit) AS memory_limit, any_value(cpu_quota) AS cpu_quota,
           any_value(cpu_shares) AS cpu_shares,
           max(time) FILTER (kind = 'container-exit') AS container_exited,
           max_by(exit_code, time) FILTER (kind = 'container-exit') AS exit_code,
           max_by(exit_reason, time) FILTER (kind = 'container-exit') AS exit_reason,
           max_by(exit_message, time) FILTER (kind = 'container-exit') AS exit_message
    FROM podindex WHERE kind LIKE 'container%' GROUP BY ALL),
  -- per pod: did any container's last exit fail (for phase)
  x AS (
    SELECT pod_uid, bool_or(last_exit <> 0) AS any_failed
    FROM (SELECT pod_uid, container, max_by(exit_code, time) AS last_exit
          FROM podindex WHERE kind = 'container-exit' GROUP BY ALL)
    GROUP BY pod_uid),
  keys AS (
    SELECT pod_uid, container, restart FROM l
    UNION SELECT pod_uid, container, restart FROM c
    UNION SELECT pod_uid, container, restart FROM m),
  -- a pod whose only rows are its own (a pod-checkpoint of a pod that
  -- logged nothing, started no container and was not scraped in the
  -- range) is one row without a container: absent, it would read as a pod
  -- that did not run
  pod_keys AS (
    SELECT * FROM keys
    UNION ALL
    SELECT pod_uid, NULL::VARCHAR AS container, NULL::INTEGER AS restart FROM p
    ANTI JOIN keys USING (pod_uid))
  SELECT coalesce(p.namespace, l.namespace, m.namespace) AS namespace,
         coalesce(p.pod, l.pod, m.pod) AS pod, k.pod_uid,
         coalesce(p.node, l.node, m.node) AS node,
         -- workload and owner kind, derived from labels and name shape: good
         -- enough for search, exact owner refs need the enricher
         CASE
           WHEN p.labels['tekton.dev/pipeline'] IS NOT NULL THEN p.labels['tekton.dev/pipeline']
           WHEN p.labels['tekton.dev/task'] IS NOT NULL THEN p.labels['tekton.dev/task']
           WHEN p.labels['job-name'] IS NOT NULL THEN regexp_replace(p.labels['job-name'], '-\d{8,10}$', '')
           WHEN p.labels['statefulset.kubernetes.io/pod-name'] IS NOT NULL THEN regexp_replace(coalesce(p.pod, l.pod, m.pod), '-\d+$', '')
           WHEN p.labels['pod-template-hash'] IS NOT NULL THEN regexp_replace(coalesce(p.pod, l.pod, m.pod), '-' || p.labels['pod-template-hash'] || '-[a-z0-9]{5}$', '')
           WHEN p.labels['controller-revision-hash'] IS NOT NULL THEN regexp_replace(coalesce(p.pod, l.pod, m.pod), '-[a-z0-9]{5}$', '')
           ELSE coalesce(p.pod, l.pod, m.pod)
         END AS workload,
         CASE
           WHEN p.labels['tekton.dev/taskRun'] IS NOT NULL THEN 'TaskRun'
           WHEN p.labels['job-name'] IS NOT NULL THEN
             CASE WHEN regexp_matches(p.labels['job-name'], '-\d{8,10}$') THEN 'CronJob' ELSE 'Job' END
           WHEN p.labels['statefulset.kubernetes.io/pod-name'] IS NOT NULL THEN 'StatefulSet'
           WHEN p.labels['pod-template-hash'] IS NOT NULL THEN 'Deployment'
           WHEN p.labels['controller-revision-hash'] IS NOT NULL THEN 'DaemonSet'
           WHEN p.labels IS NULL THEN NULL
           ELSE 'Pod'
         END AS owner_kind,
         CASE
           WHEN p.pod_uid IS NULL THEN NULL
           WHEN p.pod_deleted IS NULL THEN 'running'
           WHEN x.any_failed IS NULL THEN 'unknown'
           WHEN x.any_failed THEN 'failed'
           ELSE 'succeeded'
         END AS phase,
         k.container, k.restart, c.container_id, c.image, c.image_id, p.labels, p.qos_class, p.pod_ip,
         p.pod_started, p.last_seen, c.container_started,
         c.memory_limit, c.cpu_quota, c.cpu_shares,
         l.first_log, l.last_log, l.lines, l.bytes,
         c.container_exited, c.exit_code, c.exit_reason, c.exit_message, p.pod_deleted, m.metric_names
  FROM pod_keys k
  LEFT JOIN l USING (pod_uid, container, restart)
  LEFT JOIN c USING (pod_uid, container, restart)
  LEFT JOIN p USING (pod_uid)
  LEFT JOIN x USING (pod_uid)
  LEFT JOIN m ON m.pod_uid = k.pod_uid AND m.container IS NOT DISTINCT FROM k.container
    AND m.restart IS NOT DISTINCT FROM k.restart
  ORDER BY namespace, pod, pod_started, pod_uid, container, restart;
