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
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/fluent/fluent-bit-go/input"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Yolean/observability-to-blobs/images/fluentbit/plugins/k8s_objects/kobjects"
)

// k8s_objects is a Fluent Bit Go input plugin for the cluster agent
// (K8SAGENT.md): it watches every listable resource through the API server
// and emits the apiserver table's rows (kobjects). It runs in its own
// process, one per cluster, never next to cri_scrape: two Go runtimes in
// one process are not supported.
//
// Fluent Bit polls FLBPluginInputCallback once a second; the informers
// queue rows from their own goroutines and the callback drains the queue.

type pluginState struct {
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	queue    [][]byte
	queued   int
	dropped  int
	queueMax int
	cluster  string
	watcher  *kobjects.Watcher
}

var state *pluginState

//export FLBPluginRegister
func FLBPluginRegister(def unsafe.Pointer) int {
	return input.FLBPluginRegister(def, "k8s_objects", "Kubernetes objects and events, watched through the API server")
}

type config struct {
	kubeconfig string
	watcher    kobjects.Config
	queueMax   int
}

func parseConfig(get func(key, def string) string) (config, error) {
	c := config{kubeconfig: get("kubeconfig", "")}
	for _, d := range []struct {
		key, def string
		v        *time.Duration
	}{
		{"snapshot_interval", "1h", &c.watcher.SnapshotInterval},
		{"discovery_interval", "1m", &c.watcher.DiscoveryInterval},
	} {
		v, err := time.ParseDuration(get(d.key, d.def))
		if err != nil {
			return config{}, fmt.Errorf("%s: %w", d.key, err)
		}
		if v <= 0 {
			return config{}, fmt.Errorf("%s: %s is not positive", d.key, v)
		}
		*d.v = v
	}
	if ex := get("exclude", "default"); ex == "default" {
		c.watcher.Exclude = kobjects.DefaultExclude()
	} else {
		c.watcher.Exclude = kobjects.ParseExclude(ex)
	}
	sm, err := kobjects.ParseSecretsMode(get("secrets_mode", string(kobjects.SecretsSkip)))
	if err != nil {
		return config{}, err
	}
	c.watcher.SecretsMode = sm
	n, err := strconv.Atoi(get("queue_max_records", "200000"))
	if err != nil || n <= 0 {
		return config{}, fmt.Errorf("queue_max_records: %q is not a positive integer", get("queue_max_records", ""))
	}
	c.queueMax = n
	return c, nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
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
	cfg, err := parseConfig(get)
	if err != nil {
		log.Printf("[k8s_objects] %v", err)
		return input.FLB_ERROR
	}
	rc, err := restConfig(cfg.kubeconfig)
	if err != nil {
		log.Printf("[k8s_objects] %v", err)
		return input.FLB_ERROR
	}
	// a snapshot lists from the informers' stores, so the API load is the
	// initial lists and the watches; the rate limit only paces startup
	rc.QPS, rc.Burst = 20, 50
	rc.UserAgent = "k8s_objects"
	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		log.Printf("[k8s_objects] %v", err)
		return input.FLB_ERROR
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		log.Printf("[k8s_objects] %v", err)
		return input.FLB_ERROR
	}
	st := &pluginState{queueMax: cfg.queueMax, cluster: get("cluster", os.Getenv("CLUSTER_NAME"))}
	// no discovery cache: with aggregated discovery a run is two requests
	st.watcher = kobjects.NewWatcher(cfg.watcher, dc, dyn, st.enqueue)
	ctx, cancel := context.WithCancel(context.Background())
	st.cancel = cancel
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		st.watcher.Run(ctx)
	}()
	state = st
	log.Printf("[k8s_objects] started %s", cfg.watcher)
	return input.FLB_OK
}

func (st *pluginState) enqueue(rows []kobjects.Row) {
	enc := input.NewEncoder()
	var buf []byte
	for _, r := range rows {
		packed, err := enc.Encode([]interface{}{input.FLBTime{Time: r.Time}, r.Record(st.cluster)})
		if err != nil {
			log.Printf("[k8s_objects] encode: %v", err)
			continue
		}
		buf = append(buf, packed...)
	}
	st.mu.Lock()
	full := st.queued+len(rows) > st.queueMax
	if full {
		st.dropped += len(rows)
	} else {
		st.queue = append(st.queue, buf)
		st.queued += len(rows)
	}
	dropped := st.dropped
	st.mu.Unlock()
	if full {
		// A dropped change leaves the stream wrong until the object
		// changes again; a snapshot makes it right. It is queued once the
		// queue drains, like any rows.
		if dropped%1000 < len(rows) {
			log.Printf("[k8s_objects] queue full, dropped %d rows so far; a snapshot follows", dropped)
		}
		st.watcher.RequestSnapshot()
	}
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

func main() {}
