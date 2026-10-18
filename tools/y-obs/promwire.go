package main

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Prometheus remote write 1.0 and remote read, encoded by hand like
// images/blobs-to-prometheus/remotewrite.go to keep the dependency set
// small. Field numbers follow prompb/remote.proto and prompb/types.proto.

type label struct{ name, value string }

type sample struct {
	ts    int64 // milliseconds
	value float64
}

type series struct {
	labels  []label
	samples []sample
}

// metaLabels are the metrics columns that become series labels, as
// blobs-to-prometheus's raw.go has them: in the order Prometheus would
// have them as target labels. A sample label of the same name is renamed
// exported_<name>, as a scrape with honor_labels: false does, unless the
// row's honor_labels is set.
var metaLabels = []string{"cluster", "node", "scrape", "instance", "namespace", "pod", "container",
	"k8s_deployment_name", "k8s_replicaset_name", "k8s_statefulset_name", "k8s_daemonset_name",
	"k8s_job_name", "k8s_cronjob_name"}

func isMeta(name string) bool {
	for _, m := range metaLabels {
		if m == name {
			return true
		}
	}
	return false
}

// seriesLabels is the label set Prometheus stores for a sample: __name__,
// the non-empty meta labels, and the sample's labels, renamed on a clash;
// with honor, as with honor_labels: true, a sample label keeps its name
// and the meta label of that name is left out. Sorted by name.
func seriesLabels(name string, meta map[string]string, labels map[string]string, honor bool) []label {
	ls := make([]label, 0, 1+len(metaLabels)+len(labels))
	ls = append(ls, label{"__name__", name})
	for _, m := range metaLabels {
		if v := meta[m]; v != "" && !(honor && labels[m] != "") {
			ls = append(ls, label{m, v})
		}
	}
	for k, v := range labels {
		if v == "" {
			continue
		}
		if k == "__name__" || meta[k] != "" && !honor {
			k = "exported_" + k
		}
		ls = append(ls, label{k, v})
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].name < ls[j].name })
	return ls
}

func seriesKey(ls []label) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.name)
		b.WriteByte(0)
		b.WriteString(l.value)
		b.WriteByte(0)
	}
	return b.String()
}

// protoBuf is a minimal protobuf wire-format writer.
type protoBuf struct{ bytes.Buffer }

func (p *protoBuf) varint(v uint64) {
	for v >= 0x80 {
		p.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	p.WriteByte(byte(v))
}

func (p *protoBuf) tagVarint(field int, v uint64) { p.varint(uint64(field)<<3 | 0); p.varint(v) }

func (p *protoBuf) tagDouble(field int, v float64) {
	p.varint(uint64(field)<<3 | 1)
	bits := math.Float64bits(v)
	for i := 0; i < 8; i++ {
		p.WriteByte(byte(bits >> (8 * i)))
	}
}

func (p *protoBuf) tagBytes(field int, b []byte) {
	p.varint(uint64(field)<<3 | 2)
	p.varint(uint64(len(b)))
	p.Write(b)
}

// encodeSeries is a prompb.TimeSeries: labels (1), samples (2).
func encodeSeries(sr *series) []byte {
	var ts protoBuf
	for _, l := range sr.labels {
		var lbl protoBuf
		lbl.tagBytes(1, []byte(l.name))
		lbl.tagBytes(2, []byte(l.value))
		ts.tagBytes(1, lbl.Bytes())
	}
	for _, s := range sr.samples {
		var smp protoBuf
		smp.tagDouble(1, s.value)
		smp.tagVarint(2, uint64(s.ts))
		ts.tagBytes(2, smp.Bytes())
	}
	return ts.Bytes()
}

// encodeWriteRequest is a prompb.WriteRequest: timeseries (1).
func encodeWriteRequest(ss []*series) []byte {
	var wr protoBuf
	for _, sr := range ss {
		wr.tagBytes(1, encodeSeries(sr))
	}
	return wr.Bytes()
}

// encodeReadResponse is a prompb.ReadResponse of SAMPLES: results (1), one
// QueryResult per query, each timeseries (1).
func encodeReadResponse(results [][]*series) []byte {
	var rr protoBuf
	for _, ss := range results {
		var qr protoBuf
		for _, sr := range ss {
			qr.tagBytes(1, encodeSeries(sr))
		}
		rr.tagBytes(1, qr.Bytes())
	}
	return rr.Bytes()
}

// matcher is a prompb.LabelMatcher.
type matcher struct {
	typ         int // 0 EQ, 1 NEQ, 2 RE, 3 NRE
	name, value string
}

func (m matcher) String() string {
	return m.name + [...]string{"=", "!=", "=~", "!~"}[m.typ&3] + fmt.Sprintf("%q", m.value)
}

// readQuery is a prompb.Query.
type readQuery struct {
	startMs, endMs int64
	matchers       []matcher
}

// protoReader walks protobuf fields.
type protoReader struct {
	b   []byte
	pos int
}

var errProto = errors.New("malformed protobuf")

func (r *protoReader) varint() (uint64, error) {
	var v uint64
	for shift := uint(0); shift < 64; shift += 7 {
		if r.pos >= len(r.b) {
			return 0, errProto
		}
		c := r.b[r.pos]
		r.pos++
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, nil
		}
	}
	return 0, errProto
}

// next returns a field's number, wire type, and its varint or its bytes.
func (r *protoReader) next() (field int, wire int, v uint64, b []byte, err error) {
	tag, err := r.varint()
	if err != nil {
		return 0, 0, 0, nil, err
	}
	field, wire = int(tag>>3), int(tag&7)
	switch wire {
	case 0:
		v, err = r.varint()
	case 1:
		if r.pos+8 > len(r.b) {
			return 0, 0, 0, nil, errProto
		}
		r.pos += 8
	case 2:
		var n uint64
		if n, err = r.varint(); err == nil {
			if r.pos+int(n) > len(r.b) || int(n) < 0 {
				return 0, 0, 0, nil, errProto
			}
			b = r.b[r.pos : r.pos+int(n)]
			r.pos += int(n)
		}
	case 5:
		if r.pos+4 > len(r.b) {
			return 0, 0, 0, nil, errProto
		}
		r.pos += 4
	default:
		err = errProto
	}
	return field, wire, v, b, err
}

func (r *protoReader) done() bool { return r.pos >= len(r.b) }

// decodeReadRequest reads a prompb.ReadRequest: queries (1), each with
// start (1), end (2) and matchers (3); hints and response types are
// ignored, since the answer is always SAMPLES.
func decodeReadRequest(b []byte) ([]readQuery, error) {
	var qs []readQuery
	r := &protoReader{b: b}
	for !r.done() {
		f, w, _, qb, err := r.next()
		if err != nil {
			return nil, err
		}
		if f != 1 || w != 2 {
			continue
		}
		var q readQuery
		qr := &protoReader{b: qb}
		for !qr.done() {
			f, w, v, mb, err := qr.next()
			if err != nil {
				return nil, err
			}
			switch {
			case f == 1 && w == 0:
				q.startMs = int64(v)
			case f == 2 && w == 0:
				q.endMs = int64(v)
			case f == 3 && w == 2:
				var m matcher
				mr := &protoReader{b: mb}
				for !mr.done() {
					f, w, v, sb, err := mr.next()
					if err != nil {
						return nil, err
					}
					switch {
					case f == 1 && w == 0:
						m.typ = int(v)
					case f == 2 && w == 2:
						m.name = string(sb)
					case f == 3 && w == 2:
						m.value = string(sb)
					}
				}
				q.matchers = append(q.matchers, m)
			}
		}
		qs = append(qs, q)
	}
	return qs, nil
}
