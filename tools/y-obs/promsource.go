package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// matcherSQL is a Prometheus label matcher as a condition on w_metrics.
// __name__ is the name column, the meta labels their columns, exported_X
// the sample label X that clashed with meta label X, anything else a
// sample label. An absent label is the empty string, as in PromQL, and a
// regular expression matches the whole value.
func matcherSQL(m matcher) (string, error) {
	var col string
	switch {
	case m.name == "__name__":
		col = "name"
	case m.name == "namespace":
		col = "nullif(namespace, '_node')"
	case isMeta(m.name):
		col = m.name
	case strings.HasPrefix(m.name, "exported_") && isMeta(strings.TrimPrefix(m.name, "exported_")):
		col = "labels[" + sqlString(strings.TrimPrefix(m.name, "exported_")) + "]"
	default:
		col = "labels[" + sqlString(m.name) + "]"
	}
	v := "coalesce(" + col + ", '')"
	switch m.typ {
	case 0:
		return v + " = " + sqlString(m.value), nil
	case 1:
		return v + " <> " + sqlString(m.value), nil
	case 2, 3:
		if _, err := regexp.Compile("^(?:" + m.value + ")$"); err != nil {
			return "", fmt.Errorf("matcher %s: %v", m, err)
		}
		e := "regexp_full_match(" + v + ", " + sqlString(m.value) + ")"
		if m.typ == 3 {
			e = "NOT " + e
		}
		return e, nil
	}
	return "", fmt.Errorf("matcher %s: unknown type %d", m, m.typ)
}

// namespaceOf is the namespace an equality matcher pins, for the daily
// tier's partition pruning.
func namespaceOf(ms []matcher) string {
	for _, m := range ms {
		if m.name == "namespace" && m.typ == 0 {
			return m.value
		}
	}
	return ""
}

// metricsQuery selects the series of the matchers, one row each with its
// samples as one list of (t, v): grouping in the engine keeps the rows few
// and builds each label set once. The list is unordered: an ordered list
// aggregate holds its rows in memory and cannot spill, and ran out of a
// 2 GB limit on one hour (measured); Go sorts each series instead.
func metricsQuery(ms []matcher) (string, error) {
	conds := []string{}
	for _, m := range ms {
		c, err := matcherSQL(m)
		if err != nil {
			return "", err
		}
		conds = append(conds, c)
	}
	q := "SELECT name"
	for _, m := range metaLabels {
		col := m
		if m == "namespace" {
			col = "nullif(namespace, '_node')"
		}
		q += ", coalesce(" + col + ", '') AS " + m
	}
	q += `, labels, list({'t': epoch_ms(time), 'v': value}) AS s
FROM w_metrics`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	return q + " GROUP BY ALL", nil
}

// collectSeries runs a metrics query on a planned session. Rows that map
// to one label set (a sample label that clashed with a meta label) join
// one series; a retried upload's repeat of a timestamp counts once.
func collectSeries(ctx context.Context, s *session, q string, limit int) ([]*series, int, error) {
	rows, err := s.conn.QueryContext(ctx, q)
	if err != nil {
		return nil, 0, s.wrap(err)
	}
	defer rows.Close()
	by := map[string]*series{}
	var order []*series
	n := 0
	for rows.Next() {
		var name string
		values := make([]string, len(metaLabels))
		var labels, list any
		dest := []any{&name}
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := rows.Scan(append(dest, &labels, &list)...); err != nil {
			return nil, n, err
		}
		meta := make(map[string]string, len(metaLabels))
		for i, m := range metaLabels {
			meta[m] = values[i]
		}
		ls := seriesLabels(name, meta, labelMap(labels))
		key := seriesKey(ls)
		sr, ok := by[key]
		if !ok {
			sr = &series{labels: ls}
			by[key] = sr
			order = append(order, sr)
		}
		items, _ := list.([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			ts, _ := m["t"].(int64)
			v, _ := m["v"].(float64)
			sr.samples = append(sr.samples, sample{ts: ts, value: v})
			n++
		}
		if limit > 0 && n > limit {
			return nil, n, fail(exitBudget, "more than %d samples: narrow the selector or the time range", limit)
		}
	}
	// time order, and a retried upload's repeat of a timestamp once
	n = 0
	for _, sr := range order {
		sort.SliceStable(sr.samples, func(i, j int) bool { return sr.samples[i].ts < sr.samples[j].ts })
		kept := sr.samples[:0]
		for _, smp := range sr.samples {
			if k := len(kept); k > 0 && kept[k-1].ts == smp.ts {
				continue
			}
			kept = append(kept, smp)
		}
		sr.samples = kept
		n += len(kept)
	}
	return order, n, s.wrap(rows.Err())
}

// labelMap is a MAP(VARCHAR, VARCHAR) as scanned.
func labelMap(v any) map[string]string {
	m := map[string]string{}
	if p, ok := plain(v).(map[string]any); ok {
		for k, val := range p {
			if s, ok := val.(string); ok {
				m[k] = s
			}
		}
	}
	return m
}

// msTime is a remote-read timestamp as a window bound.
func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
