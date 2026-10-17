# operator-to-annotations

For clusters whose manifests carry prometheus-operator resources, such as
PodMonitors and PrometheusRules, but that run no prometheus-operator.
Without the CRDs those manifests fail to apply. With the CRDs alone they
apply, and nothing reads them: the agent scrapes a pod only when the pod
carries `prometheus.io/*` or `io.opentelemetry.discovery.*` annotations,
which it reads from the container runtime, at sandbox creation.

It is a **migration path**: the end state is the annotations in the
workloads' own pod templates, and the PodMonitors gone. Until then the
controller translates what it can, and reports which pods are migrated,
annotated for them, or neither. It never restarts or evicts a pod.

## CRDs

`crds/` is a kustomize base with the five prometheus-operator CRDs that
shared manifests commonly carry, all in `monitoring.coreos.com`:

| base | kind |
|---|---|
| `crds/podmonitors` | PodMonitor |
| `crds/servicemonitors` | ServiceMonitor |
| `crds/prometheusrules` | PrometheusRule |
| `crds/prometheuses` | Prometheus |
| `crds/alertmanagers` | Alertmanager |

Each CRD is a base of its own, and `crds/` is all five. They run nothing
and need nothing else, so a cluster that only needs its manifests to
apply uses them on their own. The Prometheus and Alertmanager CRDs are
larger than a client-side apply can store in its annotation, so apply
with `--server-side`:

```bash
kubectl apply --server-side -k operator-to-annotations/crds
```

**A CRD the cluster already gets elsewhere** (another chart, an operator
installed for some other purpose): take it over or leave it out. The
server-side apply fails with a conflict where another field manager owns
the CRD. Forcing it (`--force-conflicts`) leaves two owners that flip
its version on every converge. Rather compose the CRD bases you need and
leave out the ones the cluster has.

- **Only `v1`.** A manifest at any other version of these kinds fails at
  apply instead of being stored and ignored. The pinned release serves
  only `v1`; if a later one adds a version, `vendor.sh check` fails until a
  patch in that CRD's `kustomization.yaml` strips it.
- **No other prometheus-operator CRD**: Probe, ScrapeConfig,
  AlertmanagerConfig, PrometheusAgent and ThanosRuler are left out, so
  manifests that use them fail at apply. A cluster that needs them runs
  the operator.
- **Vendored as released.** The files are the release's
  `example/prometheus-operator-crd/` files, unmodified; the release tag and
  each file's sha256 are pinned in `crds/kustomization.yaml`.
  `crds/vendor.sh check` verifies the pins and that only `v1` is served
  (part of `./test.sh checks`); `crds/vendor.sh <tag>` moves to another
  release.

The itest applies the base server-side and checks that each kind applies
at `v1`, and that a PodMonitor at `v1alpha1` and a ScrapeConfig fail
(`./test.sh`, "assert: prometheus-operator CRDs").

## Controller

`controller/` is a kustomize base with a controller and mutating
admission webhook (`images/operator-to-annotations`) that translates
PodMonitors into the port-scoped OpenTelemetry annotations the agent reads
(root README, "Scrape annotations"). `kustomization.yaml` here is the CRDs
and the controller together:

```bash
kubectl apply --server-side -k operator-to-annotations
```

`controller/` alone has no CRDs: compose it with the CRD bases the
cluster lacks. It needs the PodMonitor CRD; without the ServiceMonitor
CRD it does not watch ServiceMonitors (checked at start).

It needs [cert-manager](https://cert-manager.io/): a self-signed Issuer
and a Certificate for the webhook's Service write the serving Secret, and
cert-manager injects the CA into the MutatingWebhookConfiguration
(`cert-manager.io/inject-ca-from`). The CRDs alone do not need it. The
certificate is its own CA, so each renewal (every 60 days) changes the CA:
until cert-manager has injected the new one and the controller has reloaded
the files, the API server cannot reach the webhook, and pods created in
that window are admitted unannotated (failure policy `Ignore`, below).

The **namespace is fixed** at `observability`: an overlay that sets another
must also patch what `controller/kustomization.yaml` lists.

### What it does

When a pod is **created**, the webhook finds the PodMonitors that select
it, as prometheus-operator would for a Prometheus that selects every
PodMonitor: `spec.namespaceSelector` (`any`, `matchNames`, by default the
PodMonitor's own namespace), then `spec.selector` (`matchLabels` and
`matchExpressions`). For each endpoint of each, it adds to every port the
endpoint resolves to:

```yaml
io.opentelemetry.discovery.metrics.<containerPort number>/enabled: "true"
io.opentelemetry.discovery.metrics.<containerPort number>/config: |  # when any of these is set
  metrics_path: <path>
  collection_interval: <interval>
  metric_relabel_configs: <metricRelabelings>
```

`metricRelabelings` are written as a Prometheus scrape config writes
them, which is what the agent reads (root README, "Metric relabeling"):
`sourceLabels` as `source_labels`, `targetLabel` as `target_label`, the
action in lower case, and an unset field left unset, so that
Prometheus's default applies, as when the operator generates the scrape
config. The agent's rules see a sample's own labels, not the target's
that Prometheus adds (`job`, `instance`, `namespace`, `pod`, `container`,
`endpoint`, `service`, `__meta_*`): a rule that reads one of those is
translated with a warning, since it reads the sample's label of that
name if there is one, and a `keep` on it can drop every series.

- **Only port-scoped keys, always by number.** Never the pod-level
  `io.opentelemetry.discovery.metrics/*` keys, which would enable every
  declared port, a non-metrics one and any port added later included.
  Every target comes from one PodMonitor endpoint, with its own settings.
- **The container is resolved.** An endpoint's `port` (a name) resolves to
  every container port of that name, as the operator keeps one target per
  container port; two containers that both name a port `metrics` get a
  target each, each attributed to its own container. `portNumber` is used
  as it is; when no container declares it, the pod is annotated and a
  warning says its samples will have no container and the port should be
  declared. The deprecated `targetPort` is read as a name or a number. An
  endpoint with none of the three resolves to every declared port, as in
  the operator. Containers and restartable init containers (sidecars)
  count; TCP ports only.
- **Several endpoints, several containers.** A pod whose main container
  serves `/metrics` on one port and a worker sidecar its own on another
  gets one target per container from one PodMonitor with two
  `podMetricsEndpoints`, so the main container no longer needs to proxy
  the sidecar's metrics. The itest runs this case.
- **A pod with any `prometheus.io/*` or `io.opentelemetry.discovery.*`
  annotation is skipped entirely**: its own annotations are the migrated
  state. The controller logs it at level `INFO`, naming the pod and the
  PodMonitors it would have applied, and the report counts it as
  `preannotated`. Nothing is merged, port by port or otherwise, so a pod
  that is migrated declares every port it wants scraped.
- **A port two endpoints resolve to** keeps the first, PodMonitors in
  namespace/name order and endpoints in theirs, with a warning when their
  settings differ.
- **Fields it cannot translate** are warned about, and the rest is
  translated (below).
- **A marker**, `observability-to-blobs/podmonitors: <namespace>/<name>,…`,
  lists the PodMonitors applied. It has no scrape meaning; the report
  reads it.

Warnings about a pod (its ports) go to the
controller's log, as JSON lines, and to the client that created the pod as
admission warnings (for a ReplicaSet's pods that is the controller
manager, so the log is the place to look). Fields that are not
translated are in the report, not in admission warnings. A server-side
dry run gets the same answer, but is not logged.

**Existing pods are left alone**: the runtime reads annotations only when
it creates the pod's sandbox, so a PodMonitor applies to pods created
after it. The **failure policy is `Ignore`**: a webhook that is down, or
not yet up, never blocks pod creation, and the pods created meanwhile are
not annotated until they are recreated. The report (below) lists such
pods.

**Apply a PodMonitor before the workloads it selects**, or recreate their
pods once after it. In one `kubectl apply -k` a Deployment is applied
before custom resources such as PodMonitors, so the Deployment's first
pods can be created before their PodMonitor exists; the report lists
them as created before the PodMonitor.

**Not in `kube-system` or the controller's own namespace.** The API server
calls the webhook for every pod created where the webhook configuration
selects, PodMonitor or not: it knows nothing about PodMonitors. A webhook
that hangs adds up to its timeout (5 s) to each of those pod creations, so
its `namespaceSelector` leaves out `kube-system` and `observability`;
PodMonitors of pods there do not apply.

**Where a sidecar starts before the main container**, the main
container's port is briefly a target with container `""`: until a
container runs, the agent does not know its declared ports (root README,
"What the annotations cannot do"). With numeric keys, which this
controller always writes, that window is common.

**ServiceMonitor is not supported** yet: the controller logs "ServiceMonitor
is not supported, its targets are not scraped" for each, once per
generation. Supporting it means resolving the Service's selector and each
endpoint's `targetPort` to pod ports, and annotating the backing pods the
same way.

**PrometheusRule, Prometheus and Alertmanager** are ignored. Their CRDs
exist so that manifests apply; rules are a separate concern
(`images/blobs-to-prometheus`, `tools/rules-manifest`).

### Not translated

The annotations carry a port, a path, an interval and the metric
relabelings. Everything else is warned about and left out; `scheme:
http`, `honorLabels: false`, `filterRunning: true`, `followRedirects:
true` and `enableHttp2` are what the agent does anyway, and are not.

| field | why |
|---|---|
| `scheme: https`, `tlsConfig`, `authorization`, `basicAuth`, `oauth2`, `bearerTokenSecret`, proxy settings | the agent scrapes plain http with no credentials (root README, "What the annotations cannot do"); the target is annotated and fails its scrape, `up` 0 |
| `scrapeTimeout` | the agent has one timeout for every target |
| `params` | no URL parameters in the annotations |
| `relabelings`, `honorLabels: true`, `honorTimestamps`, `trackTimestampsStaleness` | the target is the pod's address and port, and its identity is in each row's columns, so there are no target labels to relabel or to conflict with; a sample's own timestamp is not kept, each row has the scrape's time |
| spec `sampleLimit`, `targetLimit`, label limits, `bodySizeLimit`, `keepDroppedTargets`, scrape protocols, native histogram settings, `scrapeClass` | settings of a Prometheus server, not of a scrape target |

`jobLabel`, `podTargetLabels` and `attachMetadata` are not counted as
untranslated: they copy pod metadata onto each target, and the agent's rows
carry the pod's identity (namespace, pod, `pod_uid`, container) instead,
with the pod's labels in the podindex. A consumer that expects the
operator's `job` label or label copies on each sample joins for them.

**Rules the agent cannot apply.** `metricRelabelings` that do not validate
(the CRD's schema lets some through, a `replace` with no `targetLabel`
for one) are still written; the agent then applies none of the
endpoint's rules and ships every series, and logs why. The report does
not show it.

### Reporting

The controller keeps a report per PodMonitor and logs it as a JSON line
(`"msg":"report"`) whenever it changes, at level `WARN` while the
PodMonitor is not translated in full or has skipped pods:

- `condition`: `Translated`, `PartiallyTranslated` (some fields are not;
  `untranslated` lists them) or `NotTranslated` (no endpoints);
- `annotated`: the selected pods it annotated, with when (the pod's
  creation), as a count and the first 20 by name;
- `preannotated`: the selected pods with scrape annotations of their
  own, which it leaves alone, as a count and the first 20 by name;
- `skipped`: the other selected pods it did not annotate, with the
  reason: created before the PodMonitor; no endpoint resolves to a port
  of the pod; another PodMonitor took the ports; not annotated at
  admission (created before the webhook, or while it was down).

The report is not written anywhere else yet. The PodMonitor's `status`
would be the natural place, but the v1 CRD's status schema holds only
`bindings` to a Prometheus-like workload with an `Accepted` condition, and
the API server drops anything else. **TODO**, one of:

- a binding in the operator's own shape, named after the controller, with
  `Accepted`, a reason (`Translated`, `PartiallyTranslated`,
  `NotTranslated`) and the details in its message; the binding's
  `resource` must then name a Prometheus that does not exist;
- an annotation on the PodMonitor with the report as JSON, which writes to
  metadata that people or GitOps manage;
- Events on the PodMonitor, and a ConfigMap with the reports.

### Configuration

`WATCH_NAMESPACES` (comma-separated) limits the PodMonitors and
ServiceMonitors that count to those namespaces; empty is all. Pods are
watched in every namespace, since a PodMonitor can select across them.
One replica: a second would log every report twice.

RBAC: list and watch on pods, PodMonitors and ServiceMonitors; nothing
else, and nothing on Secrets (the serving certificate is a mounted
volume).
