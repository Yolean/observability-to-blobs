package main

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	"gopkg.in/yaml.v3"
)

// parseRelabel decodes and validates the metric_relabel_configs of a config
// hint, a list in the format of a Prometheus scrape config's. An absent
// node is no rules.
func parseRelabel(n yaml.Node) ([]*relabel.Config, error) {
	if n.Kind == 0 {
		return nil, nil
	}
	var cfgs []*relabel.Config
	if err := n.Decode(&cfgs); err != nil {
		return nil, err
	}
	for i, c := range cfgs {
		if c == nil {
			return nil, fmt.Errorf("rule %d is empty", i)
		}
		if err := c.Validate(model.UTF8Validation); err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
	}
	return cfgs, nil
}

// relabelSamples applies cfgs to every sample, as Prometheus applies
// metric_relabel_configs: the name is the __name__ label, and the rules see
// the sample's own labels only, not the target's identity, which is in the
// row's columns. A sample is dropped when a rule drops it, when its name
// ends up empty, or when its labels end up equal to an earlier sample's
// (a labeldrop can do that; Prometheus rejects the duplicate too). As in
// Prometheus, a label with an empty value is the same as no label, so a
// sample that is kept loses its empty labels.
func relabelSamples(rows []sample, cfgs []*relabel.Config) []sample {
	if len(cfgs) == 0 {
		return rows
	}
	out := rows[:0]
	b := labels.NewBuilder(labels.EmptyLabels())
	seen := map[uint64][]labels.Labels{}
	for _, r := range rows {
		b.Reset(labels.EmptyLabels())
		b.Set(model.MetricNameLabel, r.Name)
		for k, v := range r.Labels {
			b.Set(k, v)
		}
		if !relabel.ProcessBuilder(b, cfgs...) {
			continue
		}
		lset := b.Labels()
		name := lset.Get(model.MetricNameLabel)
		if name == "" {
			continue
		}
		h := lset.Hash()
		dup := false
		for _, earlier := range seen[h] {
			if labels.Equal(earlier, lset) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		seen[h] = append(seen[h], lset)
		l := make(map[string]string, lset.Len()-1)
		lset.Range(func(lb labels.Label) {
			if lb.Name != model.MetricNameLabel {
				l[lb.Name] = lb.Value
			}
		})
		out = append(out, sample{Name: name, Type: r.Type, Labels: l, Value: r.Value})
	}
	return out
}
