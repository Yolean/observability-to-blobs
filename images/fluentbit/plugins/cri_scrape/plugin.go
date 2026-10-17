package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"fmt"
	"log"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/fluent/fluent-bit-go/input"
	"github.com/ugorji/go/codec"
)

// cri_scrape is a Fluent Bit Go input plugin: it discovers pods on this node
// through the CRI socket, scrapes their Prometheus endpoints (plus any static
// targets) and emits one record per scrape, with the samples as an array.
// It also emits the podindex events (pod and container lifecycle) it sees
// while discovering. Every record carries a "table" field; the S3 output
// writes them all into the node's blob as JSON lines.
//
// Fluent Bit polls FLBPluginInputCallback once a second; scraping happens on
// its own goroutines and the callback drains whatever has been queued since
// the last poll.

const timeFormat = "2006-01-02T15:04:05.000000000Z"

type pluginState struct {
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	queue    [][]byte // msgpack-encoded entries, ready to hand over
	queued   int
	dropped  int
	queueMax int
	cluster  string
	node     string
	tracker  *podindexTracker
}

var state *pluginState

//export FLBPluginRegister
func FLBPluginRegister(def unsafe.Pointer) int {
	return input.FLBPluginRegister(def, "cri_scrape", "Prometheus scrape of pods discovered through the CRI socket")
}

//export FLBPluginInit
func FLBPluginInit(plugin unsafe.Pointer) int {
	get := func(key, def string) string {
		v := strings.TrimSpace(input.FLBPluginConfigKey(plugin, key))
		if v == "" {
			return def
		}
		return v
	}
	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	// Bad config is an init error, found before anything is started.
	cfg, err := parseConfig(get)
	if err != nil {
		log.Printf("[cri_scrape] %v", err)
		return input.FLB_ERROR
	}

	cluster := get("cluster", os.Getenv("CLUSTER_NAME"))
	if err := checkCluster(cluster); err != nil {
		log.Printf("[cri_scrape] %v", err)
		return input.FLB_ERROR
	}
	st := &pluginState{
		queueMax: cfg.queueMax,
		cluster:  cluster,
		node:     get("node", os.Getenv("NODE_NAME")),
	}
	ctx, cancel := context.WithCancel(context.Background())
	st.cancel = cancel
	sc := newScraper(cfg.interval, cfg.timeout, int64(cfg.bodyLimit), st.enqueueScrape)

	// The static targets are endpoints this pod serves (node_exporter and
	// the agent's own metrics behind prometheus_exporter), so their rows
	// carry this pod's identity: the endpoint, not the scraper, is the
	// source of namespace/pod/container.
	self := target{
		namespace: get("self_namespace", os.Getenv("POD_NAMESPACE")),
		pod:       get("self_pod", os.Getenv("POD_NAME")),
		podUID:    get("self_pod_uid", os.Getenv("POD_UID")),
		container: get("self_container", os.Getenv("CONTAINER_NAME")),
	}
	var disc *criDiscoverer
	if cfg.socket != "" && cfg.socket != "off" {
		if disc, err = newCRIDiscoverer(cfg.socket, cfg.timeout, get("node_ip", os.Getenv("NODE_IP"))); err != nil {
			cancel()
			log.Printf("[cri_scrape] %v", err)
			return input.FLB_ERROR
		}
		// a process's own restart count is fixed for its lifetime
		self.restart = disc.ownRestart(ctx, self.podUID, self.container)
	}
	for _, s := range cfg.statics {
		t := s.target
		t.namespace, t.pod, t.podUID, t.container, t.restart = self.namespace, self.pod, self.podUID, self.container, self.restart
		st.wg.Add(1)
		go func() {
			defer st.wg.Done()
			sc.run(ctx, t, s.scrape)
		}()
	}

	if disc != nil {
		st.tracker = newPodindexTracker(cfg.podindexCheckpoint, st.enqueuePodindex)
		st.wg.Add(1)
		go func() {
			defer st.wg.Done()
			defer disc.close()
			st.discoveryLoop(ctx, disc, sc, cfg.discoveryInterval)
		}()
	}
	state = st
	log.Printf("[cri_scrape] started socket=%s interval=%s timeout=%s statics=%q", cfg.socket, cfg.interval, cfg.timeout, cfg.staticsSpec)
	return input.FLB_OK
}

type config struct {
	socket             string
	interval           time.Duration
	timeout            time.Duration
	discoveryInterval  time.Duration
	podindexCheckpoint time.Duration
	bodyLimit          int
	queueMax           int
	staticsSpec        string
	statics            []staticTarget
}

type staticTarget struct {
	target target
	scrape string
}

// parseConfig reads and validates every setting; get returns def for an
// unset key. Durations and sizes must be positive: a zero scrape_interval
// would divide by zero, a zero discovery_interval panic the ticker.
func parseConfig(get func(key, def string) string) (config, error) {
	c := config{
		socket:      get("cri_socket", "/run/containerd/containerd.sock"),
		staticsSpec: get("static_targets", ""),
	}
	for _, d := range []struct {
		key, def string
		v        *time.Duration
	}{
		{"scrape_interval", "15s", &c.interval},
		{"scrape_timeout", "10s", &c.timeout},
		{"discovery_interval", "10s", &c.discoveryInterval},
		{"podindex_checkpoint", "1h", &c.podindexCheckpoint},
	} {
		v, err := parseDuration(get(d.key, d.def))
		if err != nil {
			return config{}, fmt.Errorf("%s: %w", d.key, err)
		}
		if v <= 0 {
			return config{}, fmt.Errorf("%s: %s is not positive", d.key, v)
		}
		*d.v = v
	}
	for _, i := range []struct {
		key, def string
		v        *int
	}{
		{"body_size_limit", strconv.Itoa(8 << 20), &c.bodyLimit},
		{"queue_max_records", "100000", &c.queueMax},
	} {
		v, err := strconv.Atoi(get(i.key, i.def))
		if err != nil {
			return config{}, fmt.Errorf("%s: bad integer: %w", i.key, err)
		}
		if v <= 0 {
			return config{}, fmt.Errorf("%s: %d is not positive", i.key, v)
		}
		*i.v = v
	}
	for _, spec := range strings.Split(c.staticsSpec, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		t, name, err := parseStaticTarget(spec)
		if err != nil {
			return config{}, fmt.Errorf("static_targets: %w", err)
		}
		c.statics = append(c.statics, staticTarget{target: t, scrape: name})
	}
	return c, nil
}

// static_targets: comma-separated "scrape@host:port/path", the name before
// the @ being the rows' scrape, e.g.
// "node@127.0.0.1:2021/metrics" for node_exporter metrics served by
// fluent-bit's own prometheus_exporter output.
func parseStaticTarget(spec string) (target, string, error) {
	name, rest, ok := strings.Cut(spec, "@")
	if !ok {
		return target{}, "", fmt.Errorf("%q: expected scrape@host:port/path", spec)
	}
	hostport, path, _ := strings.Cut(rest, "/")
	host, portStr, ok := strings.Cut(hostport, ":")
	if !ok {
		return target{}, "", fmt.Errorf("%q: expected host:port", spec)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return target{}, "", fmt.Errorf("%q: bad port: %v", spec, err)
	}
	return target{key: "static/" + spec, scheme: "http", host: host, port: port, path: "/" + path}, name, nil
}

func (st *pluginState) discoveryLoop(ctx context.Context, disc *criDiscoverer, sc *scraper, every time.Duration) {
	type running struct {
		cancel context.CancelFunc
		t      target
	}
	active := map[string]running{}
	defer func() {
		for _, r := range active {
			r.cancel()
		}
	}()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		targets, inv, err := disc.discover(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[cri_scrape] discovery: %v", err)
		} else {
			st.tracker.observe(ctx, time.Now(), inv.sandboxes, inv.containers,
				criStatus{container: disc.containerStatus, sandbox: disc.sandboxStatus})
			for key, t := range targets {
				if _, ok := active[key]; ok {
					continue
				}
				tctx, cancel := context.WithCancel(ctx)
				active[key] = running{cancel: cancel, t: t}
				log.Printf("[cri_scrape] target added %s/%s/%s %s", t.namespace, t.pod, t.container, t.url())
				if t.relabelErr != "" {
					log.Printf("[cri_scrape] target %s/%s/%s %s: metric_relabel_configs not applied, every sample is shipped: %s",
						t.namespace, t.pod, t.container, t.url(), t.relabelErr)
				}
				st.wg.Add(1)
				go func(t target) {
					defer st.wg.Done()
					sc.run(tctx, t, "pod")
				}(t)
			}
			for key, r := range active {
				if _, ok := targets[key]; !ok {
					r.cancel()
					delete(active, key)
					log.Printf("[cri_scrape] target removed %s/%s/%s", r.t.namespace, r.t.pod, r.t.container)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// enqueueScrape encodes one scrape as one record: the target's identity
// and a samples array. Encoding happens on the scrape goroutine so the
// collector callback only concatenates.
func (st *pluginState) enqueueScrape(s scrape) {
	samples := make([]map[string]interface{}, 0, len(s.Samples))
	for _, r := range s.Samples {
		samples = append(samples, map[string]interface{}{
			"name":   r.Name,
			"type":   r.Type,
			"labels": r.Labels,
			"value":  r.Value,
		})
	}
	rec := map[string]interface{}{
		"table":     "metrics",
		"time":      s.Time.UTC().Format(timeFormat),
		"cluster":   st.cluster,
		"node":      st.node,
		"scrape":    s.Scrape,
		"instance":  s.Instance,
		"namespace": s.Namespace,
		"pod":       s.Pod,
		"pod_uid":   s.PodUID,
		"container": s.Container,
		"samples":   samples,
	}
	if s.Restart != nil {
		rec["restart"] = *s.Restart
	}
	for k, v := range s.Workload.fields() {
		rec[k] = v
	}
	st.enqueue(s.Time, []map[string]interface{}{rec})
}

// enqueuePodindex encodes podindex rows. Fields not applicable to a kind
// are absent, not zero-valued.
func (st *pluginState) enqueuePodindex(rows []pirow) {
	for _, r := range rows {
		rec := map[string]interface{}{
			"table":     "podindex",
			"time":      r.Time.UTC().Format(timeFormat),
			"cluster":   st.cluster,
			"node":      st.node,
			"kind":      r.Kind,
			"namespace": r.Namespace,
			"pod":       r.Pod,
			"pod_uid":   r.PodUID,
		}
		if r.Container != "" {
			rec["container"] = r.Container
			rec["container_id"] = r.ContainerID
			rec["image"] = r.Image
			rec["restart"] = r.Restart
		}
		if r.Labels != nil {
			rec["labels"] = r.Labels
		}
		if r.PodStarted != nil {
			rec["pod_started"] = r.PodStarted.UTC().Format(timeFormat)
		}
		setString(rec, "qos_class", r.QOSClass)
		setString(rec, "pod_ip", r.PodIP)
		setString(rec, "image_id", r.ImageID)
		if r.ContainerStarted != nil {
			rec["container_started"] = r.ContainerStarted.UTC().Format(timeFormat)
		}
		setInt(rec, "memory_limit", r.MemoryLimit)
		setInt(rec, "cpu_quota", r.CPUQuota)
		setInt(rec, "cpu_shares", r.CPUShares)
		if r.ExitCode != nil {
			rec["exit_code"] = *r.ExitCode
			rec["exit_reason"] = r.ExitReason
		}
		setString(rec, "exit_message", r.ExitMessage)
		// one entry per row with its own event time, so the output's
		// rendered ts matches the row's time
		st.enqueue(r.Time, []map[string]interface{}{rec})
	}
}

func setString(rec map[string]interface{}, key, v string) {
	if v != "" {
		rec[key] = v
	}
}

func setInt(rec map[string]interface{}, key string, v int64) {
	if v != 0 {
		rec[key] = v
	}
}

// msgpack is fluent-bit-go's encoding (input.NewEncoder) with map keys in
// sorted order. Its own leaves them in Go's map order, which differs from
// one encode to the next, and a consumer that compares maps as values,
// as DuckDB's MAP does, then counts one series as several.
var msgpack = func() *codec.MsgpackHandle {
	h := &codec.MsgpackHandle{}
	h.WriteExt = true
	h.Canonical = true
	h.SetBytesExt(reflect.TypeOf(input.FLBTime{}), 0, &input.FLBTime{})
	return h
}()

// encode is recs as Fluent Bit entries at ts, one after another.
func encode(ts time.Time, recs []map[string]interface{}) []byte {
	buf := make([]byte, 0, 512*len(recs))
	for _, rec := range recs {
		var packed []byte
		err := codec.NewEncoderBytes(&packed, msgpack).Encode([]interface{}{input.FLBTime{Time: ts}, rec})
		if err != nil {
			log.Printf("[cri_scrape] encode: %v", err)
			continue
		}
		buf = append(buf, packed...)
	}
	return buf
}

func (st *pluginState) enqueue(ts time.Time, recs []map[string]interface{}) {
	buf := encode(ts, recs)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.queued+len(recs) > st.queueMax {
		// Fluent Bit has stopped draining (backpressure or a stall). Keep the
		// newest: a gap in old samples is less harmful than a growing heap.
		st.dropped += len(recs)
		if st.dropped%1000 < len(recs) {
			log.Printf("[cri_scrape] queue full, dropped %d records so far", st.dropped)
		}
		return
	}
	st.queue = append(st.queue, buf)
	st.queued += len(recs)
}

//export FLBPluginInputCallback
func FLBPluginInputCallback(data *unsafe.Pointer, size *C.size_t) int {
	st := state
	if st == nil {
		return input.FLB_OK
	}
	st.mu.Lock()
	batch := st.queue
	st.queue = nil
	st.queued = 0
	st.mu.Unlock()
	if len(batch) == 0 {
		*size = 0
		return input.FLB_OK
	}
	total := 0
	for _, b := range batch {
		total += len(b)
	}
	out := make([]byte, 0, total)
	for _, b := range batch {
		out = append(out, b...)
	}
	*data = C.CBytes(out)
	*size = C.size_t(len(out))
	return input.FLB_OK
}

//export FLBPluginInputCleanupCallback
func FLBPluginInputCleanupCallback(data unsafe.Pointer) int {
	if data != nil {
		C.free(data)
	}
	return input.FLB_OK
}

//export FLBPluginExit
func FLBPluginExit() int {
	if state != nil {
		state.cancel()
		state.wg.Wait()
	}
	return input.FLB_OK
}

// parseDuration takes a Go duration or a bare number of seconds.
func parseDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return d, nil
}

func main() {}

// clusterRE is the compactor's rule for a cluster name: one plain key
// segment, as raw1/{cluster}/ needs.
var clusterRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)

// checkCluster refuses a missing or unusable cluster name, so that an
// agent that is not configured stops instead of writing under a wrong
// prefix: the manifests have no default.
func checkCluster(name string) error {
	if name == "" {
		return fmt.Errorf("CLUSTER_NAME is required: the cluster's name, the first segment of every key the agent writes")
	}
	if !clusterRE.MatchString(name) {
		return fmt.Errorf("CLUSTER_NAME %q: want a lowercase name of letters, digits, '-', '_' and '.'", name)
	}
	return nil
}
