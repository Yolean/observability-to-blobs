package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func rules(t *testing.T, src string) yaml.Node {
	t.Helper()
	var c otelConfig
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	return c.MetricRelabelConfigs
}

func names(rows []sample) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

func TestRelabelKeepByNameIsAnchored(t *testing.T) {
	cfgs, err := parseRelabel(rules(t, `
metric_relabel_configs:
- source_labels: [__name__]
  regex: envoy_cluster_upstream_rq|envoy_listener_downstream_cx_(active|overflow)
  action: Keep
`))
	if err != nil {
		t.Fatal(err)
	}
	got := relabelSamples([]sample{
		{Name: "envoy_cluster_upstream_rq", Labels: map[string]string{"envoy_response_code": "200"}},
		{Name: "envoy_cluster_upstream_rq_active", Labels: map[string]string{}},
		{Name: "envoy_listener_downstream_cx_overflow", Labels: map[string]string{}},
	}, cfgs)
	if want := []string{"envoy_cluster_upstream_rq", "envoy_listener_downstream_cx_overflow"}; !reflect.DeepEqual(names(got), want) {
		t.Fatalf("kept %v, want %v", names(got), want)
	}
	if got[0].Labels["envoy_response_code"] != "200" {
		t.Fatalf("labels lost: %+v", got[0])
	}
}

func TestRelabelDropByNameAndLabel(t *testing.T) {
	cfgs, err := parseRelabel(rules(t, `
metric_relabel_configs:
- source_labels: [__name__, cluster]
  regex: membership_.*;(a|b)
  action: drop
`))
	if err != nil {
		t.Fatal(err)
	}
	got := relabelSamples([]sample{
		{Name: "membership_total", Labels: map[string]string{"cluster": "a"}},
		{Name: "membership_total", Labels: map[string]string{"cluster": "c"}},
		{Name: "rq_total", Labels: map[string]string{"cluster": "a"}},
	}, cfgs)
	if len(got) != 2 || got[0].Labels["cluster"] != "c" || got[1].Name != "rq_total" {
		t.Fatalf("got %+v", got)
	}
}

func TestRelabelLabeldropDropsTheDuplicates(t *testing.T) {
	cfgs, err := parseRelabel(rules(t, `
metric_relabel_configs:
- regex: worker
  action: labeldrop
`))
	if err != nil {
		t.Fatal(err)
	}
	got := relabelSamples([]sample{
		{Name: "rq_total", Type: "counter", Labels: map[string]string{"worker": "0", "code": "200"}, Value: 1},
		{Name: "rq_total", Type: "counter", Labels: map[string]string{"worker": "1", "code": "200"}, Value: 2},
		{Name: "rq_total", Type: "counter", Labels: map[string]string{"worker": "1", "code": "500"}, Value: 3},
	}, cfgs)
	want := []sample{
		{Name: "rq_total", Type: "counter", Labels: map[string]string{"code": "200"}, Value: 1},
		{Name: "rq_total", Type: "counter", Labels: map[string]string{"code": "500"}, Value: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRelabelReplaceCanRenameAndEmptyNameDrops(t *testing.T) {
	cfgs, err := parseRelabel(rules(t, `
metric_relabel_configs:
- source_labels: [__name__]
  regex: old_(.*)
  target_label: __name__
  replacement: new_$1
- source_labels: [__name__]
  regex: gone
  target_label: __name__
  replacement: ""
`))
	if err != nil {
		t.Fatal(err)
	}
	got := relabelSamples([]sample{
		{Name: "old_total", Labels: map[string]string{}},
		{Name: "gone", Labels: map[string]string{}},
	}, cfgs)
	if want := []string{"new_total"}; !reflect.DeepEqual(names(got), want) {
		t.Fatalf("got %v, want %v", names(got), want)
	}
}

func TestParseRelabelRejects(t *testing.T) {
	for _, src := range []string{
		"metric_relabel_configs:\n- action: nosuch\n",
		"metric_relabel_configs:\n- action: replace\n  regex: x\n",
		"metric_relabel_configs:\n- regex: '('\n  action: drop\n",
		"metric_relabel_configs:\n- \n",
		"metric_relabel_configs: keep\n",
	} {
		if cfgs, err := parseRelabel(rules(t, src)); err == nil {
			t.Errorf("%q: accepted %+v", src, cfgs)
		}
	}
	if cfgs, err := parseRelabel(rules(t, "metrics_path: /m\n")); err != nil || cfgs != nil {
		t.Errorf("absent: %v %v", cfgs, err)
	}
}

func TestExpandTargetsOtelMetricRelabelConfigs(t *testing.T) {
	d := declared(
		container("app", `[{"name":"a","containerPort":9101,"protocol":"TCP"},{"name":"b","containerPort":9102,"protocol":"TCP"},{"name":"c","containerPort":9103,"protocol":"TCP"}]`),
	)
	got := expandTargets(map[string]string{
		"io.opentelemetry.discovery.metrics/enabled": "true",
		"io.opentelemetry.discovery.metrics/config":  "metric_relabel_configs:\n- source_labels: [__name__]\n  regex: pod_.*\n  action: keep\n",
		// replaces the pod-level list, an empty list included
		"io.opentelemetry.discovery.metrics.9102/config": "metric_relabel_configs: []\n",
		// a broken list leaves the other fields in effect
		"io.opentelemetry.discovery.metrics.9103/config": "metrics_path: /c\nmetric_relabel_configs:\n- action: nosuch\n",
	}, d)
	if len(got) != 3 {
		t.Fatalf("unexpected %+v", got)
	}
	if len(got[0].relabel) != 1 || got[0].relabelErr != "" {
		t.Fatalf("pod-level: %+v", got[0])
	}
	if len(got[1].relabel) != 0 || got[1].relabelErr != "" {
		t.Fatalf("port-scoped empty list: %+v", got[1])
	}
	if got[2].relabel != nil || !strings.Contains(got[2].relabelErr, "nosuch") || got[2].path != "/c" {
		t.Fatalf("broken list: %+v", got[2])
	}
}

func TestScrapeCountsBeforeAndAfterRelabeling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# TYPE a_total counter\na_total 1\n# TYPE b gauge\nb{x=\"1\"} 2\nb{x=\"2\"} 3\n"))
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	cfgs, err := parseRelabel(rules(t, "metric_relabel_configs:\n- source_labels: [__name__]\n  regex: b\n  action: drop\n"))
	if err != nil {
		t.Fatal(err)
	}
	tg := target{key: "test", scheme: "http", host: host, port: p, path: "/metrics", relabel: cfgs}
	var got []scrape
	sc := newScraper(time.Minute, 5*time.Second, 1<<20, func(s scrape) { got = append(got, s) })
	if err := sc.scrapeOnce(context.Background(), tg, "pod", time.Now()); err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, s := range got[0].Samples {
		values[s.Name] = s.Value
	}
	want := map[string]float64{"a_total": 1, "up": 1, "scrape_samples_scraped": 3, "scrape_samples_post_metric_relabeling": 1}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s = %v, want %v (all: %v)", k, values[k], v, values)
		}
	}
	if _, ok := values["b"]; ok {
		t.Errorf("b was not dropped: %v", values)
	}
}

// The config operator-to-annotations writes for a PodMonitor's
// metricRelabelings (its TestTranslateMetricRelabelings) parses here.
func TestParseRelabelAsTranslated(t *testing.T) {
	cfgs, err := parseRelabel(rules(t, `metric_relabel_configs:
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
`))
	if err != nil {
		t.Fatal(err)
	}
	got := relabelSamples([]sample{
		{Name: "envoy_cluster_membership_total", Labels: map[string]string{"envoy_cluster_name": "a"}},
		{Name: "envoy_cluster_membership_total", Labels: map[string]string{"envoy_cluster_name": "c", "worker": "1"}},
		{Name: "gone", Labels: map[string]string{}},
	}, cfgs)
	want := []sample{{Name: "envoy_cluster_membership_total", Labels: map[string]string{"envoy_cluster_name": "c"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}
