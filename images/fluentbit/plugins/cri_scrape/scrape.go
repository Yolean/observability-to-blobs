package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// One scrape of one target: the identity once, and every sample the target
// exposed. This is the metrics record in the blob; the compactor unnests
// the samples into one row each.
type scrape struct {
	Time      time.Time
	Job       string
	Instance  string
	Namespace string
	Pod       string
	PodUID    string
	Container string
	Restart   *int32 // absent when the target names no running container
	Samples   []sample
}

type sample struct {
	Name   string
	Type   string
	Labels map[string]string
	Value  float64
}

type scraper struct {
	client        *http.Client
	interval      time.Duration
	timeout       time.Duration
	bodySizeLimit int64
	emit          func(scrape)
}

func newScraper(interval, timeout time.Duration, bodySizeLimit int64, emit func(scrape)) *scraper {
	return &scraper{
		client: &http.Client{
			// Timeouts are per request via context; the transport keeps one
			// idle connection per target so a healthy pod is not reconnected
			// on every scrape.
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
				MaxIdleConnsPerHost: 1,
				MaxConnsPerHost:     1,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		interval:      interval,
		timeout:       timeout,
		bodySizeLimit: bodySizeLimit,
		emit:          emit,
	}
}

// run scrapes one target until ctx is cancelled. The first scrape is offset
// by a stable hash of the target so that a node full of pods does not get
// scraped in one burst; after that the loop is sequential, so a target is
// never asked for metrics while the previous request is still in flight, and
// a slow pod is asked less often, not more.
func (s *scraper) run(ctx context.Context, t target, job string) {
	interval := s.interval
	if t.interval > 0 {
		interval = t.interval
	}
	h := fnv.New64a()
	h.Write([]byte(t.key))
	offset := time.Duration(h.Sum64() % uint64(interval))
	select {
	case <-ctx.Done():
		return
	case <-time.After(offset):
	}
	failures := 0
	for {
		start := time.Now()
		err := s.scrapeOnce(ctx, t, job, start)
		if err != nil {
			failures++
			if ctx.Err() == nil {
				log.Printf("[cri_scrape] %s: %v (failures=%d)", t.url(), err, failures)
			}
		} else {
			failures = 0
		}
		// Back off on repeated failure: a pod that cannot answer does not
		// need a request every interval to prove it. Capped so recovery is
		// noticed within a few minutes.
		wait := interval
		if failures > 0 {
			wait = interval * time.Duration(1<<min(failures, 3))
		}
		next := start.Add(wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
	}
}

func (s *scraper) scrapeOnce(parent context.Context, t target, job string, start time.Time) error {
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()

	out := scrape{Time: start, Job: job, Instance: t.instance(),
		Namespace: t.namespace, Pod: t.pod, PodUID: t.podUID, Container: t.container, Restart: t.restart}
	up := 0.0
	scraped := 0
	defer func() {
		// A scrape cut short because the target went away (replaced after
		// a restart, removed, plugin exit) says nothing about the target;
		// only the scrape's own timeout is an up=0.
		if up == 0 && parent.Err() != nil {
			return
		}
		d := time.Since(start).Seconds()
		// Prometheus's own series for a scrape, never relabeled
		kept := len(out.Samples)
		out.Samples = append(out.Samples,
			synthetic("up", up),
			synthetic("scrape_duration_seconds", d),
			synthetic("scrape_samples_scraped", float64(scraped)),
			synthetic("scrape_samples_post_metric_relabeling", float64(kept)))
		s.emit(out)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=1,*/*;q=0.1")
	req.Header.Set("User-Agent", "kubernetes-node-agent-to-blobs/cri_scrape")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(&limitedReader{r: resp.Body, limit: s.bodySizeLimit})
	if err != nil {
		var tooBig *bodyTooLarge
		if errors.As(err, &tooBig) {
			return err
		}
		return fmt.Errorf("parse: %w", err)
	}
	names := make([]string, 0, len(families))
	for n := range families {
		names = append(names, n)
	}
	sort.Strings(names)
	var rows []sample
	for _, n := range names {
		rows = appendFamily(rows, families[n])
	}
	scraped = len(rows)
	out.Samples = relabelSamples(rows, t.relabel)
	up = 1
	return nil
}

func synthetic(name string, v float64) sample {
	return sample{Name: name, Type: "gauge", Labels: map[string]string{}, Value: v}
}

// appendFamily flattens a metric family the way the text exposition does:
// histograms become _bucket/_sum/_count rows with an "le" label, summaries
// become quantile/_sum/_count rows. Nothing is aggregated or renamed, so the
// table is a lossless copy of what the pod served.
func appendFamily(rows []sample, mf *dto.MetricFamily) []sample {
	name := mf.GetName()
	typ := typeName(mf.GetType())
	for _, m := range mf.Metric {
		labels := make(map[string]string, len(m.Label)+1)
		for _, lp := range m.Label {
			labels[lp.GetName()] = lp.GetValue()
		}
		add := func(n string, extraKey, extraVal string, v float64) {
			r := sample{Name: n, Type: typ, Value: v, Labels: labels}
			if extraKey != "" {
				l := make(map[string]string, len(labels)+1)
				for k, v := range labels {
					l[k] = v
				}
				l[extraKey] = extraVal
				r.Labels = l
			}
			rows = append(rows, r)
		}
		switch {
		case m.Counter != nil:
			add(name, "", "", m.Counter.GetValue())
		case m.Gauge != nil:
			add(name, "", "", m.Gauge.GetValue())
		case m.Untyped != nil:
			add(name, "", "", m.Untyped.GetValue())
		case m.Histogram != nil:
			h := m.Histogram
			for _, b := range h.Bucket {
				add(name+"_bucket", "le", formatFloat(b.GetUpperBound()), float64(b.GetCumulativeCount()))
			}
			add(name+"_sum", "", "", h.GetSampleSum())
			add(name+"_count", "", "", float64(h.GetSampleCount()))
		case m.Summary != nil:
			sm := m.Summary
			for _, q := range sm.Quantile {
				add(name, "quantile", formatFloat(q.GetQuantile()), q.GetValue())
			}
			add(name+"_sum", "", "", sm.GetSampleSum())
			add(name+"_count", "", "", float64(sm.GetSampleCount()))
		}
	}
	return rows
}

func typeName(t dto.MetricType) string {
	switch t {
	case dto.MetricType_COUNTER:
		return "counter"
	case dto.MetricType_GAUGE:
		return "gauge"
	case dto.MetricType_HISTOGRAM:
		return "histogram"
	case dto.MetricType_SUMMARY:
		return "summary"
	case dto.MetricType_GAUGE_HISTOGRAM:
		return "gaugehistogram"
	default:
		return "untyped"
	}
}

// formatFloat matches Prometheus' own label rendering of le/quantile values.
func formatFloat(f float64) string {
	if math.IsInf(f, +1) {
		return "+Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

type bodyTooLarge struct{ limit int64 }

func (e *bodyTooLarge) Error() string {
	return fmt.Sprintf("response body exceeds body_size_limit (%d bytes)", e.limit)
}

type limitedReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *limitedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		return n, &bodyTooLarge{limit: c.limit}
	}
	return n, err
}
