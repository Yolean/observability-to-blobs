package main

// Decoding of one raw object: zstd JSON lines (compactor/sql/raw.sql is the
// line contract), of which only table=metrics lines are read. Every
// metrics line is one scrape: the target's identity and its samples, all
// at the line's time.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// metaLabels are the line's columns that become series labels, in the
// order Prometheus would have them as target labels. A sample label of
// the same name is renamed exported_<name>, as a Prometheus scrape with
// honor_labels: false does, so no series carries a label twice.
var metaLabels = []string{"cluster", "node", "job", "instance", "namespace", "pod", "container"}

type rawLine struct {
	Table     string      `json:"table"`
	Time      string      `json:"time"`
	TS        string      `json:"ts"`
	Cluster   string      `json:"cluster"`
	Node      string      `json:"node"`
	Job       string      `json:"job"`
	Instance  string      `json:"instance"`
	Namespace string      `json:"namespace"`
	Pod       string      `json:"pod"`
	Container string      `json:"container"`
	Samples   []rawSample `json:"samples"`
}

type rawSample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Value  json.RawMessage   `json:"value"`
}

// Fluent Bit renders a NaN or infinite double as a bare nan or inf, which
// is not JSON; such a value is quoted before the line is decoded again.
var bareNonFinite = regexp.MustCompile(`("value":\s*)(-?(?i:nan|inf|infinity))\b`)

type label struct{ name, value string }

type sample struct {
	ts    int64 // milliseconds
	value float64
}

type series struct {
	labels  []label
	samples []sample
}

// batch is the series of one object in first-seen order, with their
// samples in line order, which is time order per target.
type batch struct {
	order   []string
	series  map[string]*series
	samples int
	newest  int64 // ms, the newest sample's time
	lines   int   // metrics lines read
	skipped int   // lines that could not be decoded
}

func newBatch() *batch { return &batch{series: map[string]*series{}} }

func (b *batch) add(ls []label, s sample) {
	key := seriesKey(ls)
	sr, ok := b.series[key]
	if !ok {
		sr = &series{labels: ls}
		b.series[key] = sr
		b.order = append(b.order, key)
	}
	sr.samples = append(sr.samples, s)
	b.samples++
	if s.ts > b.newest {
		b.newest = s.ts
	}
}

// decodeObject reads one raw object's body (zstd JSON lines) into a batch,
// keeping the samples keep accepts (nil keeps all).
func decodeObject(body []byte, keep *selectorSet) (*batch, error) {
	zr, err := zstd.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return decodeLines(zr, keep)
}

func decodeLines(r io.Reader, keep *selectorSet) (*batch, error) {
	b := newBatch()
	sc := bufio.NewScanner(r)
	// a scrape of a large target is one line
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		// cheap pre-filter: most lines are logs
		if !bytes.Contains(raw, []byte(`"metrics"`)) {
			continue
		}
		var l rawLine
		if err := json.Unmarshal(raw, &l); err != nil {
			fixed := bareNonFinite.ReplaceAll(raw, []byte(`$1"$2"`))
			if err2 := json.Unmarshal(fixed, &l); err2 != nil {
				b.skipped++
				continue
			}
		}
		if l.Table != "metrics" {
			continue
		}
		ts, err := lineTime(l)
		if err != nil {
			b.skipped++
			continue
		}
		b.lines++
		meta := map[string]string{"cluster": l.Cluster, "node": l.Node, "job": l.Job,
			"instance": l.Instance, "namespace": l.Namespace, "pod": l.Pod, "container": l.Container}
		for _, s := range l.Samples {
			if s.Name == "" {
				continue
			}
			v, err := parseValue(s.Value)
			if err != nil {
				b.skipped++
				continue
			}
			ls := seriesLabels(s.Name, meta, s.Labels)
			if keep != nil && !keep.matches(ls) {
				continue
			}
			b.add(ls, sample{ts: ts, value: v})
		}
	}
	if err := sc.Err(); err != nil {
		return b, err
	}
	return b, nil
}

// lineTime is coalesce(time, ts) in milliseconds.
func lineTime(l rawLine) (int64, error) {
	s := l.Time
	if s == "" {
		s = l.TS
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, fmt.Errorf("time %q: %w", s, err)
	}
	return t.UnixMilli(), nil
}

func parseValue(raw json.RawMessage) (float64, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("no value")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		switch strings.ToLower(s) {
		case "nan":
			return math.NaN(), nil
		case "inf", "+inf", "infinity", "+infinity":
			return math.Inf(1), nil
		case "-inf", "-infinity":
			return math.Inf(-1), nil
		}
		return strconv.ParseFloat(s, 64)
	}
	return strconv.ParseFloat(string(raw), 64)
}

// seriesLabels is the label set Prometheus would store for the sample:
// __name__, the line's non-empty meta labels, and the sample's labels,
// those named like a set meta label renamed exported_<name>; sorted by
// name.
func seriesLabels(name string, meta, labels map[string]string) []label {
	ls := make([]label, 0, 1+len(metaLabels)+len(labels))
	ls = append(ls, label{"__name__", name})
	for _, m := range metaLabels {
		if v := meta[m]; v != "" {
			ls = append(ls, label{m, v})
		}
	}
	for k, v := range labels {
		if k == "__name__" || v == "" {
			continue
		}
		if meta[k] != "" {
			k = "exported_" + k
		}
		ls = append(ls, label{k, v})
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].name < ls[j].name })
	return ls
}

func labelValue(ls []label, name string) string {
	for _, l := range ls {
		if l.name == name {
			return l.value
		}
	}
	return ""
}

// seriesKey encodes a sorted label set; \xfe and \xff cannot appear in
// UTF-8 text.
func seriesKey(ls []label) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.name)
		b.WriteByte('\xfe')
		b.WriteString(l.value)
		b.WriteByte('\xff')
	}
	return b.String()
}
