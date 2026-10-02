package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// the custom rules directory shape: a bare list of groups, possibly
// behind a commented-out "#groups:" first line
const bareList = `#groups:
- name: example.live.rules
  rules:
  - alert: InvalidOperationEncountered
    expr: "max by (site) (increase(live_number_invalid_operations[1h])) > 0"
    labels:
      severity: critical
  - alert: LagOnOperations
    expr: >-
      kminion_kafka_consumer_group_topic_lag{
        namespace="kafka",
        topic_name=~"^site-[a-z0-9-]+\\.example-app\\.operations.00[0-9]$"
      } > 1
`

const prometheusRule = `apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: x
spec:
  groups:
  - name: g
    rules:
    - record: job:up:sum
      expr: sum by (job) (up)
    - alert: Nameless
      expr: '{__name__=~"live_.*", namespace!="dev3"} > 0'
    - alert: Gone
      expr: absent(app_requests_total{code="500"})
`

func TestFromFiles(t *testing.T) {
	dir := t.TempDir()
	f1 := write(t, dir, "bare.yml", bareList)
	f2 := write(t, dir, "crd.yaml", prometheusRule)
	m, errs := FromFiles([]string{f1, f2})
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	byName := map[string]Selector{}
	for _, s := range m.Selectors {
		byName[s.Name] = s
	}
	if _, ok := byName["live_number_invalid_operations"]; !ok {
		t.Fatalf("missing simple selector: %+v", m.Selectors)
	}
	lag := byName["kminion_kafka_consumer_group_topic_lag"]
	if len(lag.Matchers) != 2 || lag.Matchers[0].Label != "namespace" || lag.Matchers[0].Type != "=" ||
		lag.Matchers[1].Label != "topic_name" || lag.Matchers[1].Type != "=~" {
		t.Fatalf("lag matchers: %+v", lag.Matchers)
	}
	if _, ok := byName["up"]; !ok {
		t.Fatalf("recording rule selector missing: %+v", m.Selectors)
	}
	absent := byName["app_requests_total"]
	if len(absent.Matchers) != 1 || absent.Matchers[0].Value != "500" {
		t.Fatalf("absent selector: %+v", absent)
	}
	nameless := byName[""]
	if len(nameless.Matchers) != 2 {
		t.Fatalf("nameless selector should keep __name__ regex and namespace matcher: %+v", nameless)
	}
}

func TestUnparseableIsAnError(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "bad.yml", "- name: g\n  rules:\n  - alert: A\n    expr: 'sum by ('\n")
	_, errs := FromFiles([]string{f})
	if len(errs) != 1 {
		t.Fatalf("expected one error, got %v", errs)
	}
}

// A YAML error used to drop the file without a word: the manifest came
// out with no selectors and the exit status was 0.
func TestYAMLErrorNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	good := write(t, dir, "good.yml", prometheusRule)
	bad := write(t, dir, "indent.yml", "- name: g\n  rules:\n  - alert: A\n     expr: up == 0\n")
	_, errs := FromFiles([]string{good, bad})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), bad) {
		t.Fatalf("expected one error naming %s, got %v", bad, errs)
	}
}

func TestEveryDocumentIsRead(t *testing.T) {
	dir := t.TempDir()
	second := strings.Replace(prometheusRule, "name: x", "name: y", 1)
	second = strings.Replace(second, "sum by (job) (up)", "sum(second_doc_metric)", 1)
	f := write(t, dir, "multi.yaml", "---\n"+prometheusRule+"---\n"+second+"---\n")
	m, errs := FromFiles([]string{f})
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	names := map[string]bool{}
	for _, s := range m.Selectors {
		names[s.Name] = true
	}
	if !names["up"] || !names["second_doc_metric"] {
		t.Fatalf("want selectors from both documents: %+v", m.Selectors)
	}
}

func TestNotARuleDocumentIsAnError(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "kustomization.yaml", "resources:\n- rules.yaml\n")
	_, errs := FromFiles([]string{f})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "not a rule document") {
		t.Fatalf("expected a not-a-rule-document error, got %v", errs)
	}
}

func TestNoSelectorsIsAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "comments.yml", "# nothing here yet\n")
	g := write(t, dir, "empty.yml", "groups: []\n")
	m, errs := FromFiles([]string{f, g})
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"selectors":[]}` {
		t.Fatalf("got %s", out)
	}
}
