package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

func podMonitor(t *testing.T, ns, name, spec string) *monitoringv1.PodMonitor {
	t.Helper()
	pm := &monitoringv1.PodMonitor{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := yaml.UnmarshalStrict([]byte(spec), &pm.Spec); err != nil {
		t.Fatal(err)
	}
	return pm
}

func testPod(t *testing.T, ann map[string]string, spec string) *corev1.Pod {
	t.Helper()
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "p", Labels: map[string]string{"app": "x"}, Annotations: ann}}
	if err := yaml.UnmarshalStrict([]byte(spec), &p.Spec); err != nil {
		t.Fatal(err)
	}
	return p
}

// the model case: a main container and a worker sidecar, each serving its
// own /metrics, one PodMonitor with an endpoint for each
const mainAndWorker = `
containers:
- name: main
  ports: [{name: metrics, containerPort: 9201}, {name: http, containerPort: 8080}]
- name: worker
  ports: [{name: worker-metrics, containerPort: 9202}]
`

func TestTranslateOneTargetPerContainer(t *testing.T) {
	pm := podMonitor(t, "app", "pm", `
selector: {matchLabels: {app: x}}
podMetricsEndpoints:
- port: metrics
  interval: 1m30s
- port: worker-metrics
  path: /worker
`)
	res := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{pm})
	want := map[string]string{
		"io.opentelemetry.discovery.metrics.9201/enabled": "true",
		"io.opentelemetry.discovery.metrics.9201/config":  "collection_interval: 1m30s\n",
		"io.opentelemetry.discovery.metrics.9202/enabled": "true",
		"io.opentelemetry.discovery.metrics.9202/config":  "metrics_path: /worker\n",
		Marker: "app/pm",
	}
	if !reflect.DeepEqual(res.Annotations, want) {
		t.Fatalf("annotations %#v", res.Annotations)
	}
	if len(res.Warnings) != 0 || res.Preexisting {
		t.Fatalf("%+v", res)
	}
	if c := res.Contributions; len(c) != 1 || !reflect.DeepEqual(c[0].Ports, []Port{{9201, "main"}, {9202, "worker"}}) {
		t.Fatalf("contributions %+v", c)
	}
	for k := range res.Annotations {
		if strings.HasPrefix(k, "io.opentelemetry.discovery.metrics/") {
			t.Fatalf("pod-level key %s", k)
		}
	}
}

// Two containers that both name a port "metrics": one target each, by
// number, as prometheus-operator keeps one target per container port.
func TestTranslateSharedPortNameResolvesPerContainer(t *testing.T) {
	pm := podMonitor(t, "app", "pm", `{selector: {}, podMetricsEndpoints: [{port: metrics}]}`)
	pod := testPod(t, nil, `
containers:
- {name: a, ports: [{name: metrics, containerPort: 9201}]}
- {name: b, ports: [{name: metrics, containerPort: 9301}]}
initContainers:
- {name: sidecar, restartPolicy: Always, ports: [{name: metrics, containerPort: 9401}]}
- {name: init, ports: [{name: metrics, containerPort: 9501}]}
`)
	res := Translate("app/p", pod, []*monitoringv1.PodMonitor{pm})
	got := res.Contributions[0].Ports
	if !reflect.DeepEqual(got, []Port{{9201, "a"}, {9301, "b"}, {9401, "sidecar"}}) {
		t.Fatalf("ports %+v", got)
	}
	if _, ok := res.Annotations["io.opentelemetry.discovery.metrics.9501/enabled"]; ok {
		t.Fatal("a non-restartable init container's port")
	}
}

func TestTranslatePreexistingAnnotationsWin(t *testing.T) {
	pm := podMonitor(t, "app", "pm", `{selector: {}, podMetricsEndpoints: [{port: metrics}]}`)
	for _, k := range []string{"prometheus.io/scrape", "io.opentelemetry.discovery.metrics.9201/enabled", "io.opentelemetry.discovery.logs/enabled"} {
		res := Translate("app/p", testPod(t, map[string]string{k: "false"}, mainAndWorker), []*monitoringv1.PodMonitor{pm})
		if res.Annotations != nil || !res.Preexisting {
			t.Fatalf("%s: %+v", k, res)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "app/p") || !strings.Contains(res.Warnings[0], "app/pm") {
			t.Fatalf("%s: warnings %q", k, res.Warnings)
		}
	}
}

func TestTranslatePortNumberUndeclaredWarns(t *testing.T) {
	pm := podMonitor(t, "app", "pm", `{selector: {}, podMetricsEndpoints: [{portNumber: 9999}, {portNumber: 9202}]}`)
	res := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{pm})
	if res.Annotations["io.opentelemetry.discovery.metrics.9999/enabled"] != "true" || res.Annotations["io.opentelemetry.discovery.metrics.9202/enabled"] != "true" {
		t.Fatalf("annotations %v", res.Annotations)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "9999") || !strings.Contains(res.Warnings[0], "declare") {
		t.Fatalf("warnings %q", res.Warnings)
	}
	if got := res.Contributions[0].Ports; !reflect.DeepEqual(got, []Port{{9999, ""}, {9202, "worker"}}) {
		t.Fatalf("ports %+v", got)
	}
}

func TestTranslateTargetPortAndNoPort(t *testing.T) {
	byName := intstr.FromString("worker-metrics")
	byNumber := intstr.FromInt32(9201)
	for _, tc := range []struct {
		ep   monitoringv1.PodMetricsEndpoint
		want []Port
	}{
		{monitoringv1.PodMetricsEndpoint{TargetPort: &byName}, []Port{{9202, "worker"}}},
		{monitoringv1.PodMetricsEndpoint{TargetPort: &byNumber}, []Port{{9201, "main"}}},
		// neither: every declared port, as the operator's relabeling keeps them
		{monitoringv1.PodMetricsEndpoint{}, []Port{{9201, "main"}, {8080, "main"}, {9202, "worker"}}},
	} {
		pm := &monitoringv1.PodMonitor{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "pm"}}
		pm.Spec.PodMetricsEndpoints = []monitoringv1.PodMetricsEndpoint{tc.ep}
		res := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{pm})
		if got := res.Contributions[0].Ports; !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%+v: ports %+v", tc.ep, got)
		}
	}
}

func TestTranslateNothingResolves(t *testing.T) {
	pm := podMonitor(t, "app", "pm", `{selector: {}, podMetricsEndpoints: [{port: nope}]}`)
	res := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{pm})
	if res.Annotations != nil {
		t.Fatalf("annotations %v", res.Annotations)
	}
	if r := res.Contributions[0].Reason; !strings.Contains(r, `"nope"`) {
		t.Fatalf("reason %q", r)
	}
}

// A port two PodMonitors resolve to keeps the first by namespace/name,
// the same whichever order they come in; a different setting warns.
func TestTranslatePortTakenByFirstPodMonitor(t *testing.T) {
	a := podMonitor(t, "app", "a", `{selector: {}, podMetricsEndpoints: [{port: metrics, path: /a}]}`)
	b := podMonitor(t, "app", "b", `{selector: {}, podMetricsEndpoints: [{portNumber: 9201, path: /b}, {port: worker-metrics}]}`)
	for _, order := range [][]*monitoringv1.PodMonitor{{a, b}, {b, a}} {
		res := Translate("app/p", testPod(t, nil, mainAndWorker), order)
		if res.Annotations["io.opentelemetry.discovery.metrics.9201/config"] != "metrics_path: /a\n" {
			t.Fatalf("annotations %v", res.Annotations)
		}
		if res.Annotations[Marker] != "app/a,app/b" {
			t.Fatalf("marker %q", res.Annotations[Marker])
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "PodMonitor app/a endpoint 0") {
			t.Fatalf("warnings %q", res.Warnings)
		}
	}
	// identical settings: no warning
	c := podMonitor(t, "app", "c", `{selector: {}, podMetricsEndpoints: [{port: metrics, path: /a}]}`)
	res := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{a, c})
	if len(res.Warnings) != 0 || res.Annotations[Marker] != "app/a" || !strings.Contains(res.Contributions[1].Reason, "taken by PodMonitor app/a") {
		t.Fatalf("%+v", res)
	}
}

func TestTranslateIntervals(t *testing.T) {
	for in, want := range map[string]string{"30s": "30s", "1m": "1m0s", "1d": "24h0m0s", "500ms": "500ms"} {
		pm := podMonitor(t, "app", "pm", `{selector: {}, podMetricsEndpoints: [{port: metrics, interval: "`+in+`"}]}`)
		res := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{pm})
		got := res.Annotations["io.opentelemetry.discovery.metrics.9201/config"]
		if got != "collection_interval: "+want+"\n" {
			t.Fatalf("%s: %q", in, got)
		}
		if _, err := time.ParseDuration(want); err != nil {
			t.Fatalf("the agent cannot parse %q", want)
		}
	}
}

func TestSelects(t *testing.T) {
	labels := map[string]string{"app": "x", "tier": "web"}
	for _, tc := range []struct {
		spec string
		ns   string
		want bool
	}{
		{`{selector: {matchLabels: {app: x}}}`, "app", true},
		{`{selector: {matchLabels: {app: x}}}`, "other", false},
		{`{selector: {}}`, "app", true},
		{`{selector: {matchLabels: {app: y}}}`, "app", false},
		{`{selector: {matchExpressions: [{key: tier, operator: In, values: [web, api]}]}}`, "app", true},
		{`{selector: {matchExpressions: [{key: tier, operator: NotIn, values: [web]}]}}`, "app", false},
		{`{selector: {}, namespaceSelector: {any: true}}`, "other", true},
		{`{selector: {}, namespaceSelector: {matchNames: [a, other]}}`, "other", true},
		{`{selector: {}, namespaceSelector: {matchNames: [a]}}`, "app", false},
	} {
		pm := podMonitor(t, "app", "pm", tc.spec)
		if got, err := Selects(pm, tc.ns, labels); err != nil || got != tc.want {
			t.Errorf("%s in %s: %v %v", tc.spec, tc.ns, got, err)
		}
	}
}

// metricRelabelings become the metric_relabel_configs of the config hint,
// as a Prometheus scrape config writes them: snake_case, the action in
// lower case, unset fields left to Prometheus's defaults.
func TestTranslateMetricRelabelings(t *testing.T) {
	pm := podMonitor(t, "app", "pm", `
selector: {}
podMetricsEndpoints:
- port: metrics
  path: /stats/prometheus
  metricRelabelings:
  - sourceLabels: [__name__, envoy_cluster_name]
    separator: ';'
    regex: envoy_cluster_membership_.*;(a|b)
    action: Drop
  - regex: worker
    action: labeldrop
  - sourceLabels: [__name__]
    targetLabel: __name__
    replacement: ""
    regex: gone
`)
	res := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{pm})
	want := `metric_relabel_configs:
- action: drop
  regex: envoy_cluster_membership_.*;(a|b)
  separator: ;
  source_labels:
  - __name__
  - envoy_cluster_name
- action: labeldrop
  regex: worker
- regex: gone
  replacement: ""
  source_labels:
  - __name__
  target_label: __name__
metrics_path: /stats/prometheus
`
	if got := res.Annotations["io.opentelemetry.discovery.metrics.9201/config"]; got != want {
		t.Fatalf("config:\n%s\nwant:\n%s", got, want)
	}
	if spec, eps := Untranslated(pm); len(spec) != 0 || len(eps[0]) != 0 {
		t.Fatalf("untranslated %v %v", spec, eps)
	}
}

// A rule on a label that Prometheus gives the target, not the sample, is
// translated with a warning: the agent's rules see the sample's labels.
func TestTranslateWarnsOnTargetLabels(t *testing.T) {
	pm := podMonitor(t, "app", "pm", `
selector: {}
podMetricsEndpoints:
- port: metrics
  metricRelabelings:
  - sourceLabels: [namespace]
    regex: prod
    action: keep
  - sourceLabels: [__name__, __meta_kubernetes_pod_label_app]
    action: drop
    regex: x;y
  - sourceLabels: [__name__, code]
    action: drop
    regex: x;500
`)
	res := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{pm})
	if res.Annotations["io.opentelemetry.discovery.metrics.9201/config"] == "" {
		t.Fatalf("not translated: %v", res.Annotations)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "read __meta_kubernetes_pod_label_app, namespace,") {
		t.Fatalf("warnings %q", res.Warnings)
	}
}

func TestUntranslated(t *testing.T) {
	pm := podMonitor(t, "app", "pm", `
selector: {}
selectorMechanism: RoleSelector
jobLabel: team
podTargetLabels: [version]
sampleLimit: 1000
podMetricsEndpoints:
- port: metrics
  path: /m
  interval: 30s
  scheme: HTTP
  honorLabels: false
  filterRunning: true
  followRedirects: true
  enableHttp2: false
- port: metrics
  scheme: HTTPS
  scrapeTimeout: 5s
  honorLabels: true
  params: {module: [x]}
  relabelings: [{action: drop, sourceLabels: [__meta_x]}]
  metricRelabelings: [{action: drop, sourceLabels: [__name__]}]
  tlsConfig: {insecureSkipVerify: true}
  basicAuth: {username: {name: s, key: u}}
`)
	spec, eps := Untranslated(pm)
	// jobLabel and podTargetLabels are covered by the pod's identity
	if want := []string{"sampleLimit"}; !reflect.DeepEqual(spec, want) {
		t.Fatalf("spec %v", spec)
	}
	if len(eps[0]) != 0 {
		t.Fatalf("endpoint 0 %v", eps[0])
	}
	want := []string{"basicAuth", "honorLabels", "params", "relabelings", "scheme", "scrapeTimeout", "tlsConfig"}
	if !reflect.DeepEqual(eps[1], want) {
		t.Fatalf("endpoint 1 %v", eps[1])
	}
}

func TestAnnotationPatch(t *testing.T) {
	add := map[string]string{"io.opentelemetry.discovery.metrics.9201/enabled": "true", Marker: "app/pm"}
	ops := annotationPatch(map[string]string{"x": "y"}, add)
	if len(ops) != 2 || ops[0].Path != "/metadata/annotations/io.opentelemetry.discovery.metrics.9201~1enabled" ||
		ops[1].Path != "/metadata/annotations/observability-to-blobs~1podmonitors" || ops[0].Operation != "add" {
		t.Fatalf("%+v", ops)
	}
	ops = annotationPatch(nil, add)
	if len(ops) != 1 || ops[0].Path != "/metadata/annotations" || !reflect.DeepEqual(ops[0].Value, map[string]any{
		"io.opentelemetry.discovery.metrics.9201/enabled": "true", Marker: "app/pm"}) {
		t.Fatalf("%+v", ops)
	}
}
