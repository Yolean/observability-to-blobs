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
	cases := []struct {
		r    row
		want bool
	}{
		{row{Name: "app_requests_total", Labels: map[string]string{"code": "500"}}, true},
		{row{Name: "app_requests_total", Labels: map[string]string{"code": "200"}}, false},
		{row{Name: "up", Namespace: "anywhere"}, true},
		{row{Name: "live_operations_total", Namespace: "live"}, true},
		{row{Name: "live_operations_total", Namespace: "dev3"}, false},
		{row{Name: "liveish", Namespace: "live"}, false}, // regex is anchored
		{row{Name: "node_cpu_seconds_total"}, false},
	}
	for i, c := range cases {
		if got := s.matches(c.r); got != c.want {
			t.Fatalf("case %d (%s): got %v", i, c.r.Name, got)
		}
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
