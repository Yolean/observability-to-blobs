package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/golang/snappy"
)

// promServer is y-obs's side of a local Prometheus: the remote-read
// endpoint that answers older ranges from the bucket, the push of the
// recent window, and its own /metrics, which Prometheus scrapes as the
// one target it has.
type promServer struct {
	sf         sessionFlags
	maxSamples int
	listen     string // the remote-read address, 127.0.0.1:port

	reads, readErrors, readSamples, readObjects, pushed atomic.Int64
	readNanos                                           atomic.Int64
	pushedTo                                            atomic.Int64 // unix seconds, the push window's end
	pushedFrom                                          atomic.Int64
}

// read answers a prompb.ReadRequest with SAMPLES, one planned session per
// query: its window is the query's range, its namespace matcher prunes the
// daily tier.
func (p *promServer) read(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p.reads.Add(1)
	defer func() { p.readNanos.Add(int64(time.Since(start))) }()
	fail := func(code int, err error) {
		p.readErrors.Add(1)
		fmt.Fprintf(os.Stderr, "y-obs: remote read: %v\n", err)
		http.Error(w, "y-obs: "+err.Error(), code)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	raw, err := snappy.Decode(nil, body)
	if err != nil {
		fail(http.StatusBadRequest, fmt.Errorf("snappy: %w", err))
		return
	}
	qs, err := decodeReadRequest(raw)
	if err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	results := make([][]*series, len(qs))
	for i, q := range qs {
		ss, err := p.query(r.Context(), q)
		if err != nil {
			fail(http.StatusInternalServerError, err)
			return
		}
		results[i] = ss
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Content-Encoding", "snappy")
	w.Write(snappy.Encode(nil, encodeReadResponse(results)))
}

func (p *promServer) query(ctx context.Context, q readQuery) ([]*series, error) {
	// Prometheus asks for every range: its read_recent: false goes by the
	// head's start, which the pushed out-of-order samples do not move. The
	// pushed window is answered locally, so only what lies before it is
	// read here.
	if from := p.pushedFrom.Load(); from > 0 && q.endMs >= from*1000 {
		q.endMs = from*1000 - 1
	}
	if q.endMs < q.startMs {
		return nil, nil
	}
	sql, err := metricsQuery(q.matchers)
	if err != nil {
		return nil, err
	}
	w := window{from: msTime(q.startMs), to: msTime(q.endMs + 1)}
	s, err := openSession(ctx, &p.sf, w)
	if err != nil {
		return nil, err
	}
	defer s.close()
	if err := s.planWindow(ctx, []string{"metrics"}, namespaceOf(q.matchers), time.Now()); err != nil {
		return nil, err
	}
	n := s.planned()
	p.readObjects.Add(int64(n))
	if n > p.sf.maxObjects {
		return nil, fmt.Errorf("%s over %s reads %d objects, over --max-objects %d (%s): narrow the time range or add a namespace matcher", matchersString(q.matchers), w, n, p.sf.maxObjects, s.sourcesSummary())
	}
	if err := s.createWindowViews(ctx); err != nil {
		return nil, err
	}
	ss, k, err := collectSeries(ctx, s, sql, p.maxSamples)
	p.readSamples.Add(int64(k))
	return ss, err
}

func matchersString(ms []matcher) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.String()
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// push writes the recent window into Prometheus by remote write, in
// requests of about batchSamples samples.
func (p *promServer) push(ctx context.Context, promURL string, w window) error {
	s, err := openSession(ctx, &p.sf, w)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.planWindow(ctx, []string{"metrics"}, "", time.Now()); err != nil {
		return err
	}
	if n := s.planned(); n > p.sf.maxObjects {
		return fail(exitBudget, "loading %s reads %d objects, over --max-objects %d (%s): a shorter --since, or raise --max-objects", w, n, p.sf.maxObjects, s.sourcesSummary())
	}
	if err := s.createWindowViews(ctx); err != nil {
		return err
	}
	q, _ := metricsQuery(nil)
	readStart := time.Now()
	ss, n, err := collectSeries(ctx, s, q, 0)
	if err != nil {
		return err
	}
	readTook, pushStart := time.Since(readStart), time.Now()
	const batchSamples = 200000
	client := &http.Client{Timeout: 2 * time.Minute}
	var batch []*series
	inBatch := 0
	send := func() error {
		if len(batch) == 0 {
			return nil
		}
		body := snappy.Encode(nil, encodeWriteRequest(batch))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, promURL+"/api/v1/write", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "snappy")
		req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode/100 != 2 {
			msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
			return fmt.Errorf("remote write: HTTP %d %s", res.StatusCode, strings.TrimSpace(string(msg)))
		}
		p.pushed.Add(int64(inBatch))
		batch, inBatch = batch[:0], 0
		return nil
	}
	for _, sr := range ss {
		batch = append(batch, sr)
		inBatch += len(sr.samples)
		if inBatch >= batchSamples {
			if err := send(); err != nil {
				return err
			}
		}
	}
	if err := send(); err != nil {
		return err
	}
	if p.pushedFrom.Load() == 0 {
		p.pushedFrom.Store(w.from.Unix())
	}
	p.pushedTo.Store(w.to.Unix())
	fmt.Fprintf(os.Stderr, "y-obs: loaded %d samples of %d series, %s (%s): read %s, pushed %s\n", n, len(ss), w, s.sourcesSummary(),
		readTook.Round(time.Millisecond), time.Since(pushStart).Round(time.Millisecond))
	return nil
}

// metrics is the target Prometheus scrapes: what y-obs loaded and served.
func (p *promServer) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP yobs_pushed_samples_total Samples of the recent window written into Prometheus.\n# TYPE yobs_pushed_samples_total counter\nyobs_pushed_samples_total %d\n", p.pushed.Load())
	fmt.Fprintf(w, "# HELP yobs_pushed_window_seconds The pushed window's bounds, unix seconds.\n# TYPE yobs_pushed_window_seconds gauge\nyobs_pushed_window_seconds{bound=\"from\"} %d\nyobs_pushed_window_seconds{bound=\"to\"} %d\n", p.pushedFrom.Load(), p.pushedTo.Load())
	fmt.Fprintf(w, "# HELP yobs_remote_reads_total Remote-read requests, older ranges answered from the bucket.\n# TYPE yobs_remote_reads_total counter\nyobs_remote_reads_total %d\n", p.reads.Load())
	fmt.Fprintf(w, "# HELP yobs_remote_read_errors_total Remote-read requests that failed.\n# TYPE yobs_remote_read_errors_total counter\nyobs_remote_read_errors_total %d\n", p.readErrors.Load())
	fmt.Fprintf(w, "# HELP yobs_remote_read_seconds_total Time spent answering remote reads.\n# TYPE yobs_remote_read_seconds_total counter\nyobs_remote_read_seconds_total %g\n", time.Duration(p.readNanos.Load()).Seconds())
	fmt.Fprintf(w, "# HELP yobs_remote_read_samples_total Samples returned by remote reads.\n# TYPE yobs_remote_read_samples_total counter\nyobs_remote_read_samples_total %d\n", p.readSamples.Load())
	fmt.Fprintf(w, "# HELP yobs_remote_read_objects_total Bucket objects planned by remote reads.\n# TYPE yobs_remote_read_objects_total counter\nyobs_remote_read_objects_total %d\n", p.readObjects.Load())
}

// prometheusBinary finds the Prometheus server: PROMETHEUS, else ystack's
// y-prometheus, else prometheus on PATH.
func prometheusBinary() (string, error) {
	if p := os.Getenv("PROMETHEUS"); p != "" {
		return p, nil
	}
	for _, name := range []string{"y-prometheus", "prometheus"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fail(exitUsage, "no Prometheus: install ystack (y-prometheus), put prometheus on PATH, or set PROMETHEUS")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

const prometheusConfig = `# written by y-obs prometheus
global:
  scrape_interval: 15s
storage:
  tsdb:
    # the pushed window is older than the head
    out_of_order_time_window: %s
remote_read:
- url: http://%s/read
  name: bucket
  # only ranges older than the pushed window read the bucket
  read_recent: false
  remote_timeout: 5m
scrape_configs:
- job_name: y-obs
  static_configs:
  - targets: ['%s']
    labels:
      cluster: '%s'
`

// startPrometheus runs Prometheus on port with a temporary data directory,
// and waits until it is ready.
func startPrometheus(ctx context.Context, bin string, port int, dataDir, readAddr, cluster string, oooWindow time.Duration, log io.Writer) (*exec.Cmd, string, error) {
	cfg := filepath.Join(dataDir, "prometheus.yml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(prometheusConfig, oooWindow.Round(time.Minute).String(), readAddr, readAddr, cluster)), 0o600); err != nil {
		return nil, "", err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	cmd := exec.CommandContext(ctx, bin, "--config.file="+cfg, "--storage.tsdb.path="+filepath.Join(dataDir, "tsdb"),
		fmt.Sprintf("--web.listen-address=127.0.0.1:%d", port), "--web.enable-remote-write-receiver",
		"--web.page-title=y-obs "+cluster, "--storage.tsdb.retention.time=3650d")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(url + "/-/ready")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return cmd, url, nil
			}
		}
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cmd.Process.Kill()
	return nil, "", errors.New("prometheus did not become ready in 60s; its log is in " + dataDir)
}

const prometheusServeUsage = `Usage: y-obs prometheus-serve [flags]

The foreground part of y-obs prometheus: a local Prometheus with the
recent window pushed in and older ranges read from the bucket on query.
Runs until interrupted.

Flags:
`

func cmdPrometheusServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prometheus-serve", flag.ContinueOnError)
	p := &promServer{}
	p.sf.register(fs)
	port := fs.Int("port", 0, "Prometheus's port on 127.0.0.1; 0 picks a free one")
	maxSamples := fs.Int("max-samples", 20000000, "refuse a remote read returning more samples")
	ready := fs.String("ready-file", "", "write the Prometheus URL here once the window is loaded")
	poll := fs.Duration("poll", 30*time.Second, "without --to, push what arrived in the bucket this often; 0 loads once")
	fs.Usage = func() { fmt.Fprint(fs.Output(), prometheusServeUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return &exitError{code: exitOK}
		}
		return fail(exitUsage, "%v", err)
	}
	p.maxSamples = *maxSamples
	w, err := p.sf.window(time.Now())
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if p.sf.cluster == "" {
		// resolve the cluster as a session would, from the credential's scope
		st, scope, err := storeFor(ctx, p.sf.context)
		_ = st
		if err != nil {
			return err
		}
		if p.sf.cluster, err = clusterFor("", scope); err != nil {
			return err
		}
	}
	bin, err := prometheusBinary()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.listen = ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /read", p.read)
	mux.HandleFunc("GET /metrics", p.metrics)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()
	if *port == 0 {
		if *port, err = freePort(); err != nil {
			return err
		}
	}
	dataDir, err := os.MkdirTemp("", "y-obs-prometheus-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dataDir)
	logf, err := os.Create(filepath.Join(dataDir, "prometheus.log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	started := time.Now()
	// Prometheus scrapes y-obs at now before the push arrives, so the
	// out-of-order window has to reach back to the pushed window's start
	cmd, url, err := startPrometheus(ctx, bin, *port, dataDir, p.listen, p.sf.cluster, time.Since(w.from)+2*time.Hour, logf)
	if err != nil {
		return fail(exitStore, "%v", err)
	}
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
		}
	}()
	fmt.Fprintf(os.Stderr, "y-obs: prometheus %s ready in %s, loading %s\n", url, time.Since(started).Round(time.Millisecond), w)
	pushStart := time.Now()
	if err := p.push(ctx, url, w); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "y-obs: loaded in %s; %s\n", time.Since(pushStart).Round(time.Millisecond), url)
	if *ready != "" {
		if err := os.WriteFile(*ready, []byte(url+"\n"), 0o600); err != nil {
			return err
		}
	}
	if p.sf.to != "" || *poll <= 0 {
		<-ctx.Done()
		return nil
	}
	// an open-ended window follows the bucket: every poll the time since
	// the last push, overlapping it by followOverlap for uploads that
	// arrived late; Prometheus takes an identical sample again as a no-op
	last := w.to
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*poll):
		}
		next := window{from: last.Add(-followOverlap), to: time.Now().UTC()}
		if err := p.push(ctx, url, next); err != nil {
			fmt.Fprintf(os.Stderr, "y-obs: following %s: %v\n", next, err)
			continue
		}
		last = next.to
	}
}

// followOverlap is how far each follow push reaches back into the last
// one: rows land in the bucket up to a minute after their time.
const followOverlap = 2 * time.Minute
