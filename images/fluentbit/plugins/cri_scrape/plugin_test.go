package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func container(name, ports string) *runtime.Container {
	c := &runtime.Container{Metadata: &runtime.ContainerMetadata{Name: name}, Annotations: map[string]string{}}
	if ports != "" {
		c.Annotations[criContainerPorts] = ports
	}
	return c
}

func declared(cs ...*runtime.Container) []declaredPort {
	return declaredPorts(cs)
}

func TestExpandTargetsClassicNumeric(t *testing.T) {
	d := declared(
		container("app", `[{"name":"m","containerPort":9101,"protocol":"TCP"}]`),
		container("other", `[{"name":"web","containerPort":80,"protocol":"TCP"}]`),
	)
	got := expandTargets(map[string]string{annScrape: "true", annPort: "9101"}, d)
	if len(got) != 1 || got[0].container != "app" || got[0].port != 9101 || got[0].path != "/metrics" {
		t.Fatalf("unexpected %+v", got)
	}
	// a numeric port nobody declares is still scraped, unattributed
	got = expandTargets(map[string]string{annScrape: "true", annPort: "9999"}, d)
	if len(got) != 1 || got[0].container != "" || got[0].port != 9999 {
		t.Fatalf("unexpected %+v", got)
	}
	// a named port resolves to the first container declaring it
	got = expandTargets(map[string]string{annScrape: "true", annPort: "web"}, d)
	if len(got) != 1 || got[0].container != "other" || got[0].port != 80 {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestExpandTargetsClassicFallbackAllDeclaredPorts(t *testing.T) {
	d := declared(
		container("app", `[{"name":"m","containerPort":9101,"protocol":"TCP"},{"name":"udp","containerPort":53,"protocol":"UDP"}]`),
		container("side", `[{"containerPort":9102,"protocol":"TCP"}]`),
	)
	got := expandTargets(map[string]string{annScrape: "true", annPath: "/custom"}, d)
	if len(got) != 2 || got[0].port != 9101 || got[0].container != "app" || got[1].port != 9102 || got[1].container != "side" {
		t.Fatalf("unexpected %+v", got)
	}
	if got[0].path != "/custom" {
		t.Fatalf("path %+v", got[0])
	}
}

func TestExpandTargetsOtelPortScoped(t *testing.T) {
	d := declared(
		container("app", `[{"name":"app-m","containerPort":9101,"protocol":"TCP"}]`),
		container("side", `[{"name":"side-m","containerPort":9102,"protocol":"TCP"}]`),
		container("noport", ""),
	)
	got := expandTargets(map[string]string{
		"io.opentelemetry.discovery.metrics.9101/enabled":   "true",
		"io.opentelemetry.discovery.metrics.side-m/enabled": "true",
		"io.opentelemetry.discovery.metrics.side-m/config":  "metrics_path: /side" + "\n" + "collection_interval: 30s",
	}, d)
	if len(got) != 2 {
		t.Fatalf("unexpected %+v", got)
	}
	if got[0].container != "app" || got[0].port != 9101 || got[0].path != "/metrics" || got[0].interval != 0 {
		t.Fatalf("unexpected %+v", got[0])
	}
	if got[1].container != "side" || got[1].port != 9102 || got[1].path != "/side" || got[1].interval != 30*time.Second {
		t.Fatalf("unexpected %+v", got[1])
	}
}

func TestExpandTargetsOtelPodLevelWithPortOverrides(t *testing.T) {
	d := declared(
		container("app", `[{"containerPort":9101,"protocol":"TCP"},{"containerPort":9102,"protocol":"TCP"}]`),
	)
	got := expandTargets(map[string]string{
		"io.opentelemetry.discovery.metrics/enabled":      "true",
		"io.opentelemetry.discovery.metrics/config":       "metrics_path: /all",
		"io.opentelemetry.discovery.metrics.9102/enabled": "false",
	}, d)
	if len(got) != 1 || got[0].port != 9101 || got[0].path != "/all" {
		t.Fatalf("unexpected %+v", got)
	}
	// a non-prometheus scraper hint excludes the port
	got = expandTargets(map[string]string{
		"io.opentelemetry.discovery.metrics.9101/enabled": "true",
		"io.opentelemetry.discovery.metrics.9101/scraper": "nginx",
	}, d)
	if len(got) != 0 {
		t.Fatalf("unexpected %+v", got)
	}
}

// Under a pod-level hint, a port-scoped entry that only carries config
// still wins field by field, for ports addressed by number and by name.
func TestExpandTargetsOtelPodLevelWithPortScopedConfig(t *testing.T) {
	d := declared(
		container("app", `[{"name":"a","containerPort":9101,"protocol":"TCP"},{"name":"b","containerPort":9102,"protocol":"TCP"},{"name":"c","containerPort":9103,"protocol":"TCP"}]`),
	)
	got := expandTargets(map[string]string{
		"io.opentelemetry.discovery.metrics/enabled":     "true",
		"io.opentelemetry.discovery.metrics/config":      "metrics_path: /all" + "\n" + "collection_interval: 20s",
		"io.opentelemetry.discovery.metrics.9101/config": "metrics_path: /a",
		"io.opentelemetry.discovery.metrics.b/config":    "collection_interval: 30s",
		"io.opentelemetry.discovery.metrics.c/config":    "metrics_path: /by-name",
		"io.opentelemetry.discovery.metrics.9103/config": "metrics_path: /by-number",
	}, d)
	if len(got) != 3 {
		t.Fatalf("unexpected %+v", got)
	}
	if got[0].port != 9101 || got[0].path != "/a" || got[0].interval != 20*time.Second {
		t.Fatalf("by number: %+v", got[0])
	}
	if got[1].port != 9102 || got[1].path != "/all" || got[1].interval != 30*time.Second {
		t.Fatalf("by name: %+v", got[1])
	}
	if got[2].port != 9103 || got[2].path != "/by-number" {
		t.Fatalf("number over name: %+v", got[2])
	}
}

func TestExpandTargetsOtelWinsOverClassicOnSamePort(t *testing.T) {
	d := declared(container("app", `[{"containerPort":9101,"protocol":"TCP"}]`))
	got := expandTargets(map[string]string{
		annScrape: "true", annPort: "9101", annPath: "/classic",
		"io.opentelemetry.discovery.metrics.9101/enabled": "true",
		"io.opentelemetry.discovery.metrics.9101/config":  "metrics_path: /otel",
	}, d)
	if len(got) != 1 || got[0].path != "/otel" {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestExpandTargetsNothingWithoutOptIn(t *testing.T) {
	d := declared(container("app", `[{"name":"metrics","containerPort":9101,"protocol":"TCP"}]`))
	if got := expandTargets(map[string]string{}, d); len(got) != 0 {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestParseStaticTarget(t *testing.T) {
	tg, job, err := parseStaticTarget("node@127.0.0.1:2021/metrics")
	if err != nil || job != "node" || tg.host != "127.0.0.1" || tg.port != 2021 || tg.path != "/metrics" {
		t.Fatalf("%+v %q %v", tg, job, err)
	}
	if _, _, err := parseStaticTarget("127.0.0.1:2021/metrics"); err == nil {
		t.Fatal("expected error without job")
	}
}

func configGetter(set map[string]string) func(key, def string) string {
	return func(key, def string) string {
		if v, ok := set[key]; ok {
			return v
		}
		return def
	}
}

func TestParseConfigDefaults(t *testing.T) {
	c, err := parseConfig(configGetter(map[string]string{
		"scrape_interval": "30",
		"static_targets":  "node@127.0.0.1:2021/metrics, agent@127.0.0.1:2020/api/v2/metrics/prometheus",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.interval != 30*time.Second || c.timeout != 10*time.Second || c.discoveryInterval != 10*time.Second ||
		c.podindexCheckpoint != time.Hour || c.bodyLimit != 8<<20 || c.queueMax != 100000 {
		t.Fatalf("defaults: %+v", c)
	}
	if len(c.statics) != 2 || c.statics[0].job != "node" || c.statics[1].target.path != "/api/v2/metrics/prometheus" {
		t.Fatalf("statics: %+v", c.statics)
	}
}

// Bad config is an init error, never a crash of fluent-bit, and it is found
// before anything is started: a bad static target after a good one too.
func TestParseConfigRejects(t *testing.T) {
	for _, set := range []map[string]string{
		{"scrape_interval": "0"},
		{"scrape_interval": "-15s"},
		{"scrape_timeout": "0s"},
		{"discovery_interval": "0"},
		{"podindex_checkpoint": "-1"},
		{"scrape_interval": "soon"},
		{"body_size_limit": "8M"},
		{"queue_max_records": "0"},
		{"static_targets": "node@127.0.0.1:2021/metrics,127.0.0.1:2020/metrics"},
	} {
		if _, err := parseConfig(configGetter(set)); err == nil {
			t.Errorf("%v: accepted", set)
		}
	}
}

func TestAppendFamilyFlattensLikeTextFormat(t *testing.T) {
	text := `# TYPE http_requests_total counter
http_requests_total{code="200",method="get"} 1027
# TYPE rpc_duration_seconds histogram
rpc_duration_seconds_bucket{le="0.1"} 3
rpc_duration_seconds_bucket{le="+Inf"} 5
rpc_duration_seconds_sum 1.5
rpc_duration_seconds_count 5
# TYPE go_gc_duration_seconds summary
go_gc_duration_seconds{quantile="0.5"} 0.002
go_gc_duration_seconds_sum 0.1
go_gc_duration_seconds_count 7
`
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	var rows []sample
	for _, n := range []string{"http_requests_total", "rpc_duration_seconds", "go_gc_duration_seconds"} {
		rows = appendFamily(rows, fams[n])
	}
	type row struct {
		name, typ string
		labels    map[string]string
		value     float64
	}
	var got []row
	for _, r := range rows {
		got = append(got, row{r.Name, r.Type, r.Labels, r.Value})
	}
	want := []row{
		{"http_requests_total", "counter", map[string]string{"code": "200", "method": "get"}, 1027},
		{"rpc_duration_seconds_bucket", "histogram", map[string]string{"le": "0.1"}, 3},
		{"rpc_duration_seconds_bucket", "histogram", map[string]string{"le": "+Inf"}, 5},
		{"rpc_duration_seconds_sum", "histogram", map[string]string{}, 1.5},
		{"rpc_duration_seconds_count", "histogram", map[string]string{}, 5},
		{"go_gc_duration_seconds", "summary", map[string]string{"quantile": "0.5"}, 0.002},
		{"go_gc_duration_seconds_sum", "summary", map[string]string{}, 0.1},
		{"go_gc_duration_seconds_count", "summary", map[string]string{}, 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestTypeName(t *testing.T) {
	if typeName(dto.MetricType_COUNTER) != "counter" || typeName(dto.MetricType(99)) != "untyped" {
		t.Fatal("type names")
	}
}

func TestPodTargetsCarryTheRestartInTheKey(t *testing.T) {
	sb := sandbox("uid1", "default", "p1", time.Now(), nil)
	expanded := []target{{container: "app", port: 8080}, {port: 9999}}
	targets := func(restart int) []target {
		running := []*runtime.Container{criContainer("c", "app", restart, runtime.ContainerState_CONTAINER_RUNNING)}
		return podTargets(sb, running, expanded, "10.42.0.7")
	}
	first, second := targets(0), targets(1)
	app, undeclared := first[0], first[1]
	if app.restart == nil || *app.restart != 0 || app.podUID != "uid1" || app.host != "10.42.0.7" {
		t.Fatalf("app target: %+v", app)
	}
	if undeclared.restart != nil {
		t.Fatalf("a target naming no container has a restart: %+v", undeclared)
	}
	if second[0].restart == nil || *second[0].restart != 1 || second[0].key == app.key {
		t.Fatalf("after a restart the target must change key and count: %+v, was %+v", second[0], app)
	}
	if second[1].key != undeclared.key {
		t.Fatalf("a target naming no container kept its key across the restart: %q, was %q", second[1].key, undeclared.key)
	}
}

func TestRunningRestartIsTheRunningRun(t *testing.T) {
	r := runningRestart([]*runtime.Container{
		criContainer("c1", "agent", 0, runtime.ContainerState_CONTAINER_EXITED),
		criContainer("c2", "agent", 1, runtime.ContainerState_CONTAINER_RUNNING),
	})
	if r == nil || *r != 1 {
		t.Fatalf("runningRestart = %v, want 1", r)
	}
	if runningRestart([]*runtime.Container{criContainer("c1", "agent", 0, runtime.ContainerState_CONTAINER_EXITED)}) != nil {
		t.Fatal("no running run should give no restart")
	}
}

// Port-scoped keys by number and by the same port's name: the number wins
// field by field, the same at every call; with no pod-level hint too.
func TestExpandTargetsOtelNumberOverNameDeterministic(t *testing.T) {
	d := declared(container("app", `[{"name":"http","containerPort":8080,"protocol":"TCP"}]`))
	ann := map[string]string{
		"io.opentelemetry.discovery.metrics.8080/enabled": "true",
		"io.opentelemetry.discovery.metrics.8080/config":  "metrics_path: /by-number",
		"io.opentelemetry.discovery.metrics.http/enabled": "true",
		"io.opentelemetry.discovery.metrics.http/config":  "metrics_path: /by-name" + "\n" + "collection_interval: 30s",
	}
	for i := 0; i < 100; i++ {
		got := expandTargets(ann, d)
		if len(got) != 1 || got[0].path != "/by-number" || got[0].interval != 30*time.Second {
			t.Fatalf("call %d: %+v", i, got)
		}
	}
	for _, pod := range []string{"", "true"} {
		ann := map[string]string{
			"io.opentelemetry.discovery.metrics.8080/enabled": "false",
			"io.opentelemetry.discovery.metrics.http/enabled": "true",
		}
		if pod != "" {
			ann["io.opentelemetry.discovery.metrics/enabled"] = pod
		}
		for i := 0; i < 100; i++ {
			if got := expandTargets(ann, d); len(got) != 0 {
				t.Fatalf("pod-level %q, disabled by number: %+v", pod, got)
			}
		}
	}
}

// A port-scoped entry without a scraper falls back to the pod-level one,
// as its other fields do.
func TestExpandTargetsOtelPortScopedScraperFallsBack(t *testing.T) {
	d := declared(container("app", `[{"containerPort":9101,"protocol":"TCP"}]`))
	got := expandTargets(map[string]string{
		"io.opentelemetry.discovery.metrics/scraper":      "nginx",
		"io.opentelemetry.discovery.metrics.9101/enabled": "true",
	}, d)
	if len(got) != 0 {
		t.Fatalf("unexpected %+v", got)
	}
}

// Under a pod-level hint, a port-scoped number that no container declares
// is scraped only when its own enabled says so.
func TestExpandTargetsOtelUndeclaredNeedsOwnEnabled(t *testing.T) {
	d := declared(container("app", `[{"containerPort":9101,"protocol":"TCP"}]`))
	got := expandTargets(map[string]string{
		"io.opentelemetry.discovery.metrics/enabled":     "true",
		"io.opentelemetry.discovery.metrics.9999/config": "metrics_path: /x",
	}, d)
	if len(got) != 1 || got[0].port != 9101 {
		t.Fatalf("unexpected %+v", got)
	}
	got = expandTargets(map[string]string{
		"io.opentelemetry.discovery.metrics/enabled":      "true",
		"io.opentelemetry.discovery.metrics.9999/enabled": "true",
	}, d)
	if len(got) != 2 || got[1].port != 9999 || got[1].container != "" {
		t.Fatalf("unexpected %+v", got)
	}
}

// A label map is encoded with its keys in sorted order, the same every
// time, so that its JSON line is the same text for the same series.
func TestEncodeSortsMapKeys(t *testing.T) {
	labels := map[string]string{}
	for _, k := range []string{"mode", "cpu", "zone", "a", "le", "code", "instance_id", "b"} {
		labels[k] = "v"
	}
	rec := func() []map[string]interface{} {
		return []map[string]interface{}{{"table": "metrics", "samples": []map[string]interface{}{{"name": "x", "labels": labels}}}}
	}
	ts := time.Unix(1700000000, 0)
	first := encode(ts, rec())
	for i := 0; i < 20; i++ {
		if got := encode(ts, rec()); string(got) != string(first) {
			t.Fatalf("encode %d differs from the first", i)
		}
	}
	last := -1
	for _, k := range []string{"a", "b", "code", "cpu", "instance_id", "le", "mode", "zone"} {
		// a msgpack fixstr: 0xa0 plus the length, then the bytes
		i := strings.Index(string(first), string([]byte{byte(0xa0 + len(k))})+k)
		if i <= last {
			t.Fatalf("key %q at %d, after the previous key at %d", k, i, last)
		}
		last = i
	}
}
