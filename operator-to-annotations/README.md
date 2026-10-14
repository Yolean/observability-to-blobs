# operator-to-annotations

For clusters whose manifests carry prometheus-operator resources, such as
PodMonitors and PrometheusRules, but that run no prometheus-operator.
Without the CRDs those manifests fail to apply. With the CRDs alone they
apply, and nothing reads them: the agent scrapes a pod only when the pod
carries `prometheus.io/*` or `io.opentelemetry.discovery.*` annotations,
which it reads from the container runtime, at sandbox creation.

## CRDs

`crds/` is a kustomize base with the five prometheus-operator CRDs that
shared manifests commonly carry, all in `monitoring.coreos.com`:

| CRD | kind |
|---|---|
| `podmonitors` | PodMonitor |
| `servicemonitors` | ServiceMonitor |
| `prometheusrules` | PrometheusRule |
| `prometheuses` | Prometheus |
| `alertmanagers` | Alertmanager |

It runs nothing and needs nothing else, so a cluster that only needs its
manifests to apply uses it on its own. The Prometheus and Alertmanager
CRDs are larger than a client-side apply can store in its annotation, so
apply with `--server-side`:

```bash
kubectl apply --server-side -k operator-to-annotations/crds
```

- **Only `v1`.** A manifest at any other version of these kinds fails at
  apply instead of being stored and ignored. The pinned release serves
  only `v1`; if a later one adds a version, `vendor.sh check` fails until a
  patch in `crds/kustomization.yaml` strips it.
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
