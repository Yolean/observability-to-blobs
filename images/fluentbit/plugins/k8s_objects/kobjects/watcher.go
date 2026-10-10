package kobjects

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// Config is what the watcher needs besides its clients.
type Config struct {
	// SnapshotInterval: every resource is snapshotted at each multiple of
	// it on the wall clock (1h: at every full hour), so that every hour of
	// the raw tier holds the whole cluster at least once
	SnapshotInterval time.Duration
	// DiscoveryInterval: how often discovery runs again, to start watching
	// new resources (a CRD installed) and stop watching removed ones
	DiscoveryInterval time.Duration
	// Exclude: resources not watched, as group/resource
	Exclude map[schema.GroupResource]bool
	// SecretsMode: whether Secrets are watched at all (SecretsSkip, the
	// default) or watched and shipped with scrambled values (SecretsScrambled)
	SecretsMode SecretsMode
	// CRDMode: what is kept of CustomResourceDefinitions' schemas
	// (CRDRedacted, the default; CRDStorageSchema; CRDFull)
	CRDMode CRDMode
}

// SecretsMode says what the watcher does with core Secrets.
type SecretsMode string

const (
	// SecretsSkip: Secrets are not listed or watched, so no Secret value
	// ever reaches the agent; the default.
	SecretsSkip SecretsMode = "skip"
	// SecretsScrambled: Secrets are watched and ship with their values
	// scrambled (Scramble), keeping their type and key names.
	SecretsScrambled SecretsMode = "scrambled"
)

// ParseSecretsMode reads skip or scrambled; empty is skip.
func ParseSecretsMode(s string) (SecretsMode, error) {
	switch m := SecretsMode(strings.TrimSpace(s)); m {
	case "":
		return SecretsSkip, nil
	case SecretsSkip, SecretsScrambled:
		return m, nil
	default:
		return "", fmt.Errorf("secrets_mode: %q is neither %q nor %q", s, SecretsSkip, SecretsScrambled)
	}
}

// CRDMode says what the watcher keeps of a CustomResourceDefinition
// (RedactCRD).
type CRDMode string

const (
	// CRDRedacted: every version's schema is replaced by a placeholder
	// that accepts any object; the default.
	CRDRedacted CRDMode = "redacted"
	// CRDStorageSchema: the storage version keeps its schema, the other
	// versions get the placeholder.
	CRDStorageSchema CRDMode = "storage-schema"
	// CRDFull: CRDs ship as the API server serves them.
	CRDFull CRDMode = "full"
)

// ParseCRDMode reads redacted, storage-schema or full; empty is redacted.
func ParseCRDMode(s string) (CRDMode, error) {
	switch m := CRDMode(strings.TrimSpace(s)); m {
	case "":
		return CRDRedacted, nil
	case CRDRedacted, CRDStorageSchema, CRDFull:
		return m, nil
	default:
		return "", fmt.Errorf("crd_mode: %q is none of %q, %q, %q", s, CRDRedacted, CRDStorageSchema, CRDFull)
	}
}

// secretsResource is the core Secrets' group/resource.
var secretsResource = schema.GroupResource{Resource: "secrets"}

// DefaultExclude is the resources a watcher skips unless told otherwise:
// events.k8s.io's events are the core events' storage seen through another
// group, so they would be every Event twice; leases are renewed every few
// seconds by every controller and node, and nothing a person asks of a past
// cluster needs them.
func DefaultExclude() map[schema.GroupResource]bool {
	return map[schema.GroupResource]bool{
		{Group: "events.k8s.io", Resource: "events"}:       true,
		{Group: "coordination.k8s.io", Resource: "leases"}: true,
	}
}

// ParseExclude reads a comma-separated list of resource.group ("leases.
// coordination.k8s.io"; a core resource is its bare name, "events").
func ParseExclude(s string) map[schema.GroupResource]bool {
	m := map[schema.GroupResource]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		m[schema.ParseGroupResource(f)] = true
	}
	return m
}

// Watcher runs one informer per listable and watchable resource of the
// server's preferred versions, all namespaces, and hands its rows to emit.
// Emit is called from many goroutines; the rows of one call stay in order.
type Watcher struct {
	cfg  Config
	disc discovery.DiscoveryInterface
	dyn  dynamic.Interface
	emit func([]Row)
	now  func() time.Time

	mu      sync.Mutex
	running map[schema.GroupVersionResource]*resourceInformer
	// forbidden is the resources the agent may not list, logged once
	forbidden map[schema.GroupVersionResource]bool
	// snapshotNow asks the loop for a snapshot of everything, now
	snapshotNow chan struct{}
}

type resourceInformer struct {
	gvr    schema.GroupVersionResource
	inf    cache.SharedIndexInformer
	cancel context.CancelFunc
	// synced is closed once the initial list is in the store and the
	// resource's first snapshot has been emitted; the scheduled snapshots
	// only include synced resources
	synced chan struct{}
}

func NewWatcher(cfg Config, disc discovery.DiscoveryInterface, dyn dynamic.Interface, emit func([]Row)) *Watcher {
	if cfg.SnapshotInterval <= 0 {
		cfg.SnapshotInterval = time.Hour
	}
	if cfg.DiscoveryInterval <= 0 {
		cfg.DiscoveryInterval = time.Minute
	}
	if cfg.Exclude == nil {
		cfg.Exclude = DefaultExclude()
	}
	if cfg.SecretsMode == "" {
		cfg.SecretsMode = SecretsSkip
	}
	if cfg.CRDMode == "" {
		cfg.CRDMode = CRDRedacted
	}
	if cfg.SecretsMode == SecretsSkip {
		// a copy: the caller's map stays as given
		ex := make(map[schema.GroupResource]bool, len(cfg.Exclude)+1)
		for gr, v := range cfg.Exclude {
			ex[gr] = v
		}
		ex[secretsResource] = true
		cfg.Exclude = ex
	}
	return &Watcher{cfg: cfg, disc: disc, dyn: dyn, emit: emit, now: time.Now,
		running: map[schema.GroupVersionResource]*resourceInformer{}, forbidden: map[schema.GroupVersionResource]bool{},
		snapshotNow: make(chan struct{}, 1)}
}

// RequestSnapshot asks for a snapshot of every resource as soon as
// possible; the plugin does when it had to drop rows.
func (w *Watcher) RequestSnapshot() {
	select {
	case w.snapshotNow <- struct{}{}:
	default:
	}
}

// Run watches until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	defer w.stopAll()
	w.reconcile(ctx)
	discover := time.NewTicker(w.cfg.DiscoveryInterval)
	defer discover.Stop()
	snapshot := time.NewTimer(w.untilNextSnapshot())
	defer snapshot.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-discover.C:
			w.reconcile(ctx)
		case <-snapshot.C:
			w.snapshotAll()
			snapshot.Reset(w.untilNextSnapshot())
		case <-w.snapshotNow:
			w.snapshotAll()
		}
	}
}

// untilNextSnapshot is the time to the next multiple of the interval, at
// least a second away, so a timer that fires early does not snapshot twice.
func (w *Watcher) untilNextSnapshot() time.Duration {
	now := w.now()
	d := now.Truncate(w.cfg.SnapshotInterval).Add(w.cfg.SnapshotInterval).Sub(now)
	if d < time.Second {
		d += w.cfg.SnapshotInterval
	}
	return d
}

// Resources is what the watcher watches, in discovery's terms: the server's
// preferred version of every resource that can be listed and watched, minus
// subresources and the excluded. A group that fails discovery (an aggregated
// API whose backend is down) is left out and logged; the others are kept.
func Resources(disc discovery.DiscoveryInterface, exclude map[schema.GroupResource]bool) ([]schema.GroupVersionResource, error) {
	lists, err := discovery.ServerPreferredResources(disc)
	if err != nil {
		var failed *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &failed) || len(lists) == 0 {
			return nil, err
		}
		log.Printf("[k8s_objects] discovery, partial: %v", err)
	}
	var out []schema.GroupVersionResource
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") || !hasVerbs(r, "list", "watch") {
				continue
			}
			if exclude[schema.GroupResource{Group: gv.Group, Resource: r.Name}] {
				continue
			}
			out = append(out, gv.WithResource(r.Name))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func hasVerbs(r metav1.APIResource, verbs ...string) bool {
	have := map[string]bool{}
	for _, v := range r.Verbs {
		have[v] = true
	}
	for _, v := range verbs {
		if !have[v] {
			return false
		}
	}
	return true
}

// reconcile starts an informer for every resource discovery lists that has
// none and stops the informers of resources it no longer lists. A resource
// whose preferred version changes is a new resource: its old informer stops
// and the new one snapshots.
func (w *Watcher) reconcile(ctx context.Context) {
	gvrs, err := Resources(w.disc, w.cfg.Exclude)
	if err != nil {
		log.Printf("[k8s_objects] discovery: %v", err)
		return
	}
	want := map[schema.GroupVersionResource]bool{}
	for _, g := range gvrs {
		want[g] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for g, ri := range w.running {
		if !want[g] {
			log.Printf("[k8s_objects] stop %s", g)
			ri.cancel()
			delete(w.running, g)
		}
	}
	for _, g := range gvrs {
		if _, ok := w.running[g]; ok {
			continue
		}
		if w.mayNotList(ctx, g) {
			continue
		}
		w.running[g] = w.start(ctx, g)
	}
}

// mayNotList tells whether RBAC refuses listing the resource, with a list
// of one object. Such a resource is left out until a later discovery run
// finds it allowed: its informer would retry, and log, forever, and it
// would never be snapshotted anyway. Any other error starts the informer,
// which retries.
func (w *Watcher) mayNotList(ctx context.Context, g schema.GroupVersionResource) bool {
	_, err := w.dyn.Resource(g).List(ctx, metav1.ListOptions{Limit: 1})
	if apierrors.IsForbidden(err) {
		if !w.forbidden[g] {
			log.Printf("[k8s_objects] not watching %s: %v", g, err)
			w.forbidden[g] = true
		}
		return true
	}
	delete(w.forbidden, g)
	return false
}

func (w *Watcher) stopAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for g, ri := range w.running {
		ri.cancel()
		delete(w.running, g)
	}
}

func (w *Watcher) start(parent context.Context, gvr schema.GroupVersionResource) *resourceInformer {
	ctx, cancel := context.WithCancel(parent)
	inf := dynamicinformer.NewFilteredDynamicInformer(w.dyn, gvr, metav1.NamespaceAll, 0, cache.Indexers{}, nil).Informer()
	// sanitize before the store keeps the object: Secret data is never
	// held, and managedFields and redacted CRD schemas cost no memory
	_ = inf.SetTransform(func(obj interface{}) (interface{}, error) {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			Sanitize(gvr, u)
			RedactCRD(gvr, u, w.cfg.CRDMode)
		}
		return obj, nil
	})
	ri := &resourceInformer{gvr: gvr, inf: inf, cancel: cancel, synced: make(chan struct{})}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj interface{}, isInInitialList bool) {
			// the initial list is the resource's first snapshot
			if !isInInitialList {
				w.emitObject(KindAdded, gvr, obj)
			}
		},
		UpdateFunc: func(old, obj interface{}) {
			// a relist delivers every object as an update; unchanged ones
			// keep their resourceVersion
			if o, n := asObject(old), asObject(obj); o != nil && n != nil && o.GetResourceVersion() == n.GetResourceVersion() {
				return
			}
			w.emitObject(KindModified, gvr, obj)
		},
		DeleteFunc: func(obj interface{}) {
			// a delete the watch missed, found by a relist, comes wrapped
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emitObject(KindDeleted, gvr, obj)
		},
	})
	go inf.Run(ctx.Done())
	go func() {
		if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
			return
		}
		log.Printf("[k8s_objects] watching %s", gvr)
		w.snapshot(ri, w.now())
		close(ri.synced)
	}()
	return ri
}

func asObject(obj interface{}) *unstructured.Unstructured {
	u, _ := obj.(*unstructured.Unstructured)
	return u
}

func (w *Watcher) emitObject(kind string, gvr schema.GroupVersionResource, obj interface{}) {
	u := asObject(obj)
	if u == nil {
		log.Printf("[k8s_objects] %s %s: not an object: %T", kind, gvr, obj)
		return
	}
	r, err := objectRow(w.now(), kind, gvr, u)
	if err != nil {
		log.Printf("[k8s_objects] %s %s %s/%s: %v", kind, gvr, u.GetNamespace(), u.GetName(), err)
		return
	}
	w.emit([]Row{r})
}

// snapshot emits every object in the resource's store with the time at,
// then the snapshot row that says the snapshot at that time is complete.
// One emit call, so the snapshot row follows its objects in the blob.
func (w *Watcher) snapshot(ri *resourceInformer, at time.Time) {
	items := ri.inf.GetStore().List()
	rows := make([]Row, 0, len(items)+1)
	for _, obj := range items {
		u := asObject(obj)
		if u == nil {
			continue
		}
		r, err := objectRow(at, KindSnapshotObject, ri.gvr, u)
		if err != nil {
			log.Printf("[k8s_objects] snapshot %s %s/%s: %v", ri.gvr, u.GetNamespace(), u.GetName(), err)
			continue
		}
		rows = append(rows, r)
	}
	rows = append(rows, Row{Time: at, Kind: KindSnapshot, GVR: ri.gvr, Rows: len(rows)})
	w.emit(rows)
}

// snapshotAll snapshots every synced resource at one time.
func (w *Watcher) snapshotAll() {
	at := w.now()
	w.mu.Lock()
	ris := make([]*resourceInformer, 0, len(w.running))
	for _, ri := range w.running {
		select {
		case <-ri.synced:
			ris = append(ris, ri)
		default:
		}
	}
	w.mu.Unlock()
	for _, ri := range ris {
		w.snapshot(ri, at)
	}
}

// Watching is the resources whose first snapshot has been emitted, for the
// plugin's startup log and the tests.
func (w *Watcher) Watching() []schema.GroupVersionResource {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []schema.GroupVersionResource
	for g, ri := range w.running {
		select {
		case <-ri.synced:
			out = append(out, g)
		default:
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// String is for logs.
func (c Config) String() string {
	var ex []string
	for gr := range c.Exclude {
		ex = append(ex, gr.String())
	}
	sort.Strings(ex)
	return fmt.Sprintf("snapshot_interval=%s discovery_interval=%s exclude=%s secrets_mode=%s crd_mode=%s", c.SnapshotInterval, c.DiscoveryInterval, strings.Join(ex, ","), c.SecretsMode, c.CRDMode)
}
