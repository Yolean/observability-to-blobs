package main

import (
	"os"
	"path/filepath"
	"testing"
)

func loadFrom(t *testing.T, content string) *selectorSet {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := loadSelectors(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSelectorMatching(t *testing.T) {
	s := loadFrom(t, `{"selectors":[
		{"name":"app_requests_total","matchers":[{"label":"code","type":"=","value":"500"}]},
		{"name":"up"},
		{"name":"","matchers":[{"label":"__name__","type":"=~","value":"live_.*"},{"label":"namespace","type":"!=","value":"dev3"}]}
	]}`)
	meta := func(ns string) map[string]string { return map[string]string{"namespace": ns} }
	cases := []struct {
		ls   []label
		want bool
	}{
		{seriesLabels("app_requests_total", nil, map[string]string{"code": "500"}), true},
		{seriesLabels("app_requests_total", nil, map[string]string{"code": "200"}), false},
		{seriesLabels("up", meta("anywhere"), nil), true},
		{seriesLabels("live_operations_total", meta("live"), nil), true},
		{seriesLabels("live_operations_total", meta("dev3"), nil), false},
		{seriesLabels("liveish", meta("live"), nil), false}, // regex is anchored
		{seriesLabels("node_cpu_seconds_total", nil, nil), false},
	}
	for i, c := range cases {
		if got := s.matches(c.ls); got != c.want {
			t.Fatalf("case %d (%v): got %v", i, c.ls, got)
		}
	}
}

// A selector sees the labels as stored: an exposed label named like a
// set meta label is exported_<name>, and . matches a newline as in PromQL.
func TestSelectorSeesStoredLabels(t *testing.T) {
	s := loadFrom(t, `{"selectors":[
		{"name":"blobs_compactor_last_success_timestamp_seconds","matchers":[{"label":"exported_job","type":"=","value":"rolling"}]},
		{"name":"msg","matchers":[{"label":"text","type":"=~","value":"a.b"}]}
	]}`)
	ls := seriesLabels("blobs_compactor_last_success_timestamp_seconds",
		map[string]string{"job": "pod"}, map[string]string{"job": "rolling"})
	if !s.matches(ls) {
		t.Fatalf("exported_job: %v", ls)
	}
	if !s.matches(seriesLabels("msg", nil, map[string]string{"text": "a\nb"})) {
		t.Fatal("(?s): a newline is not matched by .")
	}
}

func TestSelectorsRejectBadRegex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.json")
	if err := os.WriteFile(p, []byte(`{"selectors":[{"name":"x","matchers":[{"label":"a","type":"=~","value":"("}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSelectors(p); err == nil {
		t.Fatal("expected error")
	}
}
