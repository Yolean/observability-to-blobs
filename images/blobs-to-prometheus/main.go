// blobs-to-prometheus tails the metrics table of the node agent's bucket
// into a Prometheus remote-write receiver. Run it as a sidecar of a
// Prometheus started with --web.enable-remote-write-receiver and
// --storage.tsdb.out_of_order_time_window covering the backfill window:
// on start it replays the last BACKFILL of windows (so an ephemeral
// Prometheus volume comes back with recent history), then keeps pushing
// each new window about a minute behind real time, with the original
// scrape timestamps.
//
// It reads the PoC agent's key layout, {cluster}/{node}/{date}/{HH}/{MM}/
// metrics-N.parquet: for every node and minute window it GETs
// metrics-0.parquet, metrics-1.parquet, ... and a 404 ends the window.
// Nodes are discovered by listing the cluster prefix once per
// NODES_REFRESH. The PoC fluent-bit patch that numbered those keys
// ($SEQ, patch 0006) is gone, and the agent now writes
// raw1/{cluster}/{date}/{HH}/{node}/{MMSS}-{uuid}.jsonl.zst, so against a
// current bucket this finds no nodes and pushes nothing. Rewiring it to
// raw1 is still to do (README.md, "Known gaps").
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/golang/snappy"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/parquet-go/parquet-go"
)

type config struct {
	endpoint     string
	useSSL       bool
	bucket       string
	prefix       string // cluster name, the first key segment
	remoteWrite  string
	backfill     time.Duration
	poll         time.Duration
	nodesRefresh time.Duration
	windowSize   time.Duration
	grace        time.Duration // how long past a window's end new objects may appear
}

func envDefault(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envDuration(k, def string) time.Duration {
	d, err := time.ParseDuration(envDefault(k, def))
	if err != nil {
		log.Fatalf("%s: %v", k, err)
	}
	return d
}

func main() {
	log.SetFlags(0)
	var selectors *selectorSet
	if f := envDefault("SELECTORS_FILE", ""); f != "" {
		var err error
		selectors, err = loadSelectors(f)
		if err != nil {
			log.Fatalf("SELECTORS_FILE: %v", err)
		}
		log.Printf("filtering to %d selectors from %s", selectors.len(), f)
	}
	cfg := config{
		endpoint:     envDefault("S3_ENDPOINT", ""),
		useSSL:       envDefault("S3_TLS", "off") == "on",
		bucket:       envDefault("S3_BUCKET", ""),
		prefix:       envDefault("CLUSTER_NAME", ""),
		remoteWrite:  envDefault("REMOTE_WRITE_URL", "http://127.0.0.1:9090/api/v1/write"),
		backfill:     envDuration("BACKFILL", "1h"),
		poll:         envDuration("POLL", "15s"),
		nodesRefresh: envDuration("NODES_REFRESH", "2m"),
		windowSize:   envDuration("UPLOAD_WINDOW", "1m"),
		grace:        envDuration("WINDOW_GRACE", "30s"),
	}
	if cfg.endpoint == "" || cfg.bucket == "" || cfg.prefix == "" {
		log.Fatal("S3_ENDPOINT, S3_BUCKET and CLUSTER_NAME are required")
	}
	endpoint := strings.TrimPrefix(strings.TrimPrefix(cfg.endpoint, "https://"), "http://")
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), ""),
		Secure: cfg.useSSL,
		Region: envDefault("S3_REGION", "us-east-1"),
	})
	if err != nil {
		log.Fatal(err)
	}
	t := &tailer{cfg: cfg, mc: mc, done: map[string]int{}, selectors: selectors}
	t.run(context.Background())
}

type tailer struct {
	cfg   config
	mc    *minio.Client
	nodes []string
	// done[node+window] = number of objects already pushed for that window
	done      map[string]int
	selectors *selectorSet
}

func (t *tailer) run(ctx context.Context) {
	lastNodes := time.Time{}
	start := time.Now().Add(-t.cfg.backfill).Truncate(t.cfg.windowSize)
	log.Printf("backfilling from %s", start.UTC().Format(time.RFC3339))
	for {
		if time.Since(lastNodes) > t.cfg.nodesRefresh {
			if err := t.refreshNodes(ctx); err != nil {
				log.Printf("node discovery: %v", err)
			}
			lastNodes = time.Now()
		}
		// windows from `start` (backfill edge, advanced as windows complete)
		// up to and including the current one
		now := time.Now()
		for w := start; !w.After(now); w = w.Add(t.cfg.windowSize) {
			complete := now.After(w.Add(t.cfg.windowSize + t.cfg.grace))
			for _, node := range t.nodes {
				if err := t.pushWindow(ctx, node, w); err != nil {
					log.Printf("%s %s: %v", node, w.UTC().Format("15:04"), err)
				}
			}
			if complete && w.Equal(start) {
				// nothing more can arrive for the oldest tracked window
				for _, node := range t.nodes {
					delete(t.done, node+w.UTC().Format(time.RFC3339))
				}
				start = w.Add(t.cfg.windowSize)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.cfg.poll):
		}
	}
}

func (t *tailer) refreshNodes(ctx context.Context) error {
	seen := map[string]bool{}
	for obj := range t.mc.ListObjects(ctx, t.cfg.bucket, minio.ListObjectsOptions{
		Prefix: t.cfg.prefix + "/",
	}) {
		if obj.Err != nil {
			return obj.Err
		}
		// non-recursive listing yields "prefix/node/" entries
		parts := strings.Split(strings.TrimPrefix(obj.Key, t.cfg.prefix+"/"), "/")
		if len(parts) > 0 && parts[0] != "" && !seen[parts[0]] {
			seen[parts[0]] = true
		}
	}
	nodes := make([]string, 0, len(seen))
	for n := range seen {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	if len(nodes) > 0 && strings.Join(nodes, ",") != strings.Join(t.nodes, ",") {
		log.Printf("nodes: %v", nodes)
	}
	t.nodes = nodes
	return nil
}

// pushWindow fetches any not-yet-pushed objects of one node's window, using
// the predictable keys: metrics-0.parquet upward until 404.
func (t *tailer) pushWindow(ctx context.Context, node string, w time.Time) error {
	key := node + w.UTC().Format(time.RFC3339)
	seq := t.done[key]
	for {
		object := fmt.Sprintf("%s/%s/%s/metrics-%d.parquet",
			t.cfg.prefix, node, w.UTC().Format("2006-01-02/15/04"), seq)
		data, err := t.get(ctx, object)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return fmt.Errorf("get %s: %w", object, err)
		}
		series, samples, err := toRemoteWrite(data, t.selectors)
		if err != nil {
			return fmt.Errorf("decode %s: %w", object, err)
		}
		if err := t.send(ctx, series); err != nil {
			return fmt.Errorf("remote write %s: %w", object, err)
		}
		log.Printf("pushed %s (%d samples)", object, samples)
		seq++
		t.done[key] = seq
	}
}

func (t *tailer) get(ctx context.Context, object string) ([]byte, error) {
	obj, err := t.mc.GetObject(ctx, t.cfg.bucket, object, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == 404
}

func (t *tailer) send(ctx context.Context, wr []byte) error {
	if len(wr) == 0 {
		return nil
	}
	body := snappy.Encode(nil, wr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.remoteWrite, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// row matches the metrics table schema (CONSUMING.md).
type row struct {
	Time      int64             `parquet:"time,timestamp(nanosecond)"`
	Cluster   string            `parquet:"cluster"`
	Node      string            `parquet:"node"`
	Job       string            `parquet:"job"`
	Instance  string            `parquet:"instance"`
	Namespace string            `parquet:"namespace"`
	Pod       string            `parquet:"pod"`
	Container string            `parquet:"container"`
	Name      string            `parquet:"name"`
	Type      string            `parquet:"type"`
	Labels    map[string]string `parquet:"labels"`
	Value     float64           `parquet:"value"`
}

// toRemoteWrite turns one blob into an encoded prometheus.WriteRequest.
// Samples are grouped per series and kept in blob order, which is time
// order per origin scrape.
func toRemoteWrite(data []byte, keep *selectorSet) ([]byte, int, error) {
	pf, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, 0, err
	}
	reader := parquet.NewGenericReader[row](pf)
	defer reader.Close()

	type sample struct {
		ts    int64 // milliseconds
		value float64
	}
	series := map[string][]sample{}
	order := []string{}
	total := 0
	buf := make([]row, 256)
	for {
		n, err := reader.Read(buf)
		for i := 0; i < n; i++ {
			r := buf[i]
			if keep != nil && !keep.matches(r) {
				continue
			}
			key := seriesKey(r)
			if _, ok := series[key]; !ok {
				order = append(order, key)
			}
			series[key] = append(series[key], sample{ts: r.Time / 1e6, value: r.Value})
			total++
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, 0, err
		}
	}

	var wr protoBuf
	for _, key := range order {
		var ts protoBuf
		for _, l := range decodeSeriesKey(key) {
			var lbl protoBuf
			lbl.tagBytes(1, []byte(l[0]))
			lbl.tagBytes(2, []byte(l[1]))
			ts.tagMessage(1, lbl.Bytes())
		}
		for _, s := range series[key] {
			var smp protoBuf
			smp.tagDouble(1, s.value)
			smp.tagVarint(2, uint64(s.ts))
			ts.tagMessage(2, smp.Bytes())
		}
		wr.tagMessage(1, ts.Bytes())
	}
	return wr.Bytes(), total, nil
}

// seriesKey encodes the sorted label set; \xff separates entries since it
// cannot appear in UTF-8 label data at these positions.
func seriesKey(r row) string {
	labels := [][2]string{{"__name__", r.Name}}
	add := func(k, v string) {
		if v != "" {
			labels = append(labels, [2]string{k, v})
		}
	}
	add("cluster", r.Cluster)
	add("node", r.Node)
	add("job", r.Job)
	add("instance", r.Instance)
	add("namespace", r.Namespace)
	add("pod", r.Pod)
	add("container", r.Container)
	for k, v := range r.Labels {
		add(k, v)
	}
	sort.Slice(labels[1:], func(i, j int) bool { return labels[i+1][0] < labels[j+1][0] })
	var b strings.Builder
	for _, l := range labels {
		b.WriteString(l[0])
		b.WriteByte('\xfe')
		b.WriteString(l[1])
		b.WriteByte('\xff')
	}
	return b.String()
}

func decodeSeriesKey(key string) [][2]string {
	var out [][2]string
	for _, entry := range strings.Split(strings.TrimSuffix(key, "\xff"), "\xff") {
		kv := strings.SplitN(entry, "\xfe", 2)
		if len(kv) == 2 {
			out = append(out, [2]string{kv[0], kv[1]})
		}
	}
	return out
}

// protoBuf is a minimal protobuf wire-format writer, enough for
// prometheus.WriteRequest: message fields (length-delimited), bytes,
// varints and doubles. Field numbers follow prompb/remote.proto and
// prompb/types.proto.
type protoBuf struct {
	bytes.Buffer
}

func (p *protoBuf) varint(v uint64) {
	for v >= 0x80 {
		p.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	p.WriteByte(byte(v))
}

func (p *protoBuf) tagVarint(field int, v uint64) {
	p.varint(uint64(field)<<3 | 0)
	p.varint(v)
}

func (p *protoBuf) tagDouble(field int, v float64) {
	p.varint(uint64(field)<<3 | 1)
	var raw [8]byte
	bits := mathFloat64bits(v)
	for i := 0; i < 8; i++ {
		raw[i] = byte(bits >> (8 * i))
	}
	p.Write(raw[:])
}

func (p *protoBuf) tagBytes(field int, b []byte) {
	p.varint(uint64(field)<<3 | 2)
	p.varint(uint64(len(b)))
	p.Write(b)
}

func (p *protoBuf) tagMessage(field int, b []byte) {
	p.tagBytes(field, b)
}
