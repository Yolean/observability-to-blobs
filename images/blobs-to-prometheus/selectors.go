package main

// Selector filtering: SELECTORS_FILE points at the JSON manifest that
// tools/rules-manifest emits from Prometheus rule files. Only rows
// matching at least one selector are pushed, which is what tailors
// consumption to the series (and label subsets) rule evaluation actually
// needs. No file, no filtering.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

type manifest struct {
	Selectors []manifestSelector `json:"selectors"`
}

type manifestSelector struct {
	Name     string `json:"name"`
	Matchers []struct {
		Label string `json:"label"`
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"matchers"`
}

type compiledMatcher struct {
	label string
	typ   string
	value string
	regex *regexp.Regexp
}

type compiledSelector struct {
	name     string
	matchers []compiledMatcher
}

type selectorSet struct {
	selectors []compiledSelector
}

func loadSelectors(path string) (*selectorSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	set := &selectorSet{}
	for _, s := range m.Selectors {
		cs := compiledSelector{name: s.Name}
		for _, mm := range s.Matchers {
			cm := compiledMatcher{label: mm.Label, typ: mm.Type, value: mm.Value}
			switch mm.Type {
			case "=", "!=":
			case "=~", "!~":
				// PromQL regex matchers are fully anchored
				re, err := regexp.Compile("^(?:" + mm.Value + ")$")
				if err != nil {
					return nil, fmt.Errorf("selector %q label %q: %w", s.Name, mm.Label, err)
				}
				cm.regex = re
			default:
				return nil, fmt.Errorf("selector %q label %q: unknown matcher type %q", s.Name, mm.Label, mm.Type)
			}
			cs.matchers = append(cs.matchers, cm)
		}
		set.selectors = append(set.selectors, cs)
	}
	return set, nil
}

func (s *selectorSet) len() int { return len(s.selectors) }

// matches reports whether any selector accepts the row. Labels are looked
// up across the row's meta columns and its labels map, the same view the
// series gets in Prometheus.
func (s *selectorSet) matches(r row) bool {
	for _, sel := range s.selectors {
		if sel.name != "" && sel.name != r.Name {
			continue
		}
		ok := true
		for _, m := range sel.matchers {
			v := rowLabel(r, m.label)
			switch m.typ {
			case "=":
				ok = v == m.value
			case "!=":
				ok = v != m.value
			case "=~":
				ok = m.regex.MatchString(v)
			case "!~":
				ok = !m.regex.MatchString(v)
			}
			if !ok {
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func rowLabel(r row, name string) string {
	switch name {
	case "__name__":
		return r.Name
	case "cluster":
		return r.Cluster
	case "node":
		return r.Node
	case "job":
		return r.Job
	case "instance":
		return r.Instance
	case "namespace":
		return r.Namespace
	case "pod":
		return r.Pod
	case "container":
		return r.Container
	}
	return r.Labels[name]
}
