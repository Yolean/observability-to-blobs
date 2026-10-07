package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	clientv3 "go.etcd.io/etcd/client/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/Yolean/observability-to-blobs/images/fluentbit/plugins/k8s_objects/kobjects"
)

// The acceptance test of the objects path without a cluster: a real API
// server is the source; the agent's watcher writes the raw lines; DuckDB
// reads them through compactor/sql/raw.sql, tables.sql and
// local/apiserver_at.sql; apiserver-at serves the result. What kubectl reads
// from apiserver-at must be what it read from the source at that time.
//
// Needs KUBEBUILDER_ASSETS (etcd, kube-apiserver and kubectl, from
// setup-envtest) and duckdb on PATH or in DUCKDB; skipped without them.

const repoRoot = "../.."

func needAssets(t *testing.T) (assets, duckdb string) {
	t.Helper()
	assets = os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (setup-envtest use -p path)")
	}
	duckdb = os.Getenv("DUCKDB")
	if duckdb == "" {
		var err error
		if duckdb, err = exec.LookPath("duckdb"); err != nil {
			t.Skip("no duckdb on PATH and no DUCKDB")
		}
	}
	return assets, duckdb
}

func decode(t *testing.T, docs string) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	for _, d := range strings.Split(docs, "\n---\n") {
		if strings.TrimSpace(d) == "" {
			continue
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(d), &u.Object); err != nil {
			t.Fatalf("%v in\n%s", err, d)
		}
		out = append(out, u)
	}
	return out
}

// cluster is a client of one API server, the source or apiserver-at.
type cluster struct {
	cfg    *rest.Config
	disc   discovery.DiscoveryInterface
	dyn    dynamic.Interface
	mapper meta.ResettableRESTMapper
}

func newCluster(t *testing.T, cfg *rest.Config) *cluster {
	t.Helper()
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &cluster{cfg: cfg, disc: disc, dyn: dyn,
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))}
}

func (c *cluster) resource(t *testing.T, u *unstructured.Unstructured) dynamic.ResourceInterface {
	t.Helper()
	gvk := u.GroupVersionKind()
	m, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		c.mapper.Reset()
		if m, err = c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			t.Fatalf("%s: %v", gvk, err)
		}
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		return c.dyn.Resource(m.Resource).Namespace(u.GetNamespace())
	}
	return c.dyn.Resource(m.Resource)
}

// apply creates u, or replaces the object of its name.
func (c *cluster) apply(t *testing.T, ctx context.Context, u *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	r := c.resource(t, u)
	cur, err := r.Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		out, err := r.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create %s %s: %v", u.GetKind(), u.GetName(), err)
		}
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	u.SetResourceVersion(cur.GetResourceVersion())
	out, err := r.Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update %s %s: %v", u.GetKind(), u.GetName(), err)
	}
	return out
}

// setStatus writes u's status onto the object of its name.
func (c *cluster) setStatus(t *testing.T, ctx context.Context, u *unstructured.Unstructured) {
	t.Helper()
	r := c.resource(t, u)
	cur, err := r.Get(ctx, u.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Object["status"] = u.Object["status"]
	if _, err := r.UpdateStatus(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("status %s %s: %v", u.GetKind(), u.GetName(), err)
	}
}

// state is every watched object, as the agent would ship it, minus
// resourceVersion and with the random part of scrambled Secret values
// masked: resource -> namespace/name -> JSON.
type state map[string]map[string]string

// dump is the state of the cluster; sanitize, for the source, does to its
// objects what the agent does, which apiserver-at's objects already had.
func (c *cluster) dump(t *testing.T, ctx context.Context, gvrs []schema.GroupVersionResource, sanitize bool) state {
	t.Helper()
	s := state{}
	for _, g := range gvrs {
		l, err := c.dyn.Resource(g).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list %s: %v", g, err)
		}
		m := map[string]string{}
		for i := range l.Items {
			u := &l.Items[i]
			if sanitize {
				kobjects.Sanitize(g, u)
			}
			if g == secrets {
				maskScrambled(t, u)
			}
			m[u.GetNamespace()+"/"+u.GetName()] = canonical(t, u)
		}
		s[g.String()] = m
	}
	return s
}

// maskScrambled replaces the random part of every scrambled value of a
// Secret with as many x, so that two scrambles of a value compare equal
// and a value of another length does not. A value that is not scrambled
// fails the test, unless it is empty.
func maskScrambled(t *testing.T, u *unstructured.Unstructured) {
	t.Helper()
	data, _ := u.Object["data"].(map[string]interface{})
	for k, v := range data {
		s, _ := v.(string)
		if s == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(s)
		rest, ok := strings.CutPrefix(string(b), kobjects.ScrambledPrefix)
		if err != nil || !ok {
			t.Fatalf("secret %s/%s key %s is not scrambled: %q", u.GetNamespace(), u.GetName(), k, b)
		}
		data[k] = base64.StdEncoding.EncodeToString([]byte(kobjects.ScrambledPrefix + strings.Repeat("x", len(rest))))
	}
}

func canonical(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	u = u.DeepCopy()
	u.SetResourceVersion("")
	u.SetManagedFields(nil)
	b, err := json.Marshal(u.Object)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// diff lists what differs between two states, resource by resource.
func diff(want, got state) []string {
	var out []string
	for r, w := range want {
		g := got[r]
		for k, wv := range w {
			gv, ok := g[k]
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("%s %s: missing", r, k))
			case gv != wv:
				out = append(out, fmt.Sprintf("%s %s: differs\n  want %s\n  got  %s", r, k, wv, gv))
			}
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				out = append(out, fmt.Sprintf("%s %s: unexpected", r, k))
			}
		}
	}
	sort.Strings(out)
	return out
}

// lines is the raw tier as the plugin would write it, one JSON line per
// row's record, kept in emit order.
type lines struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	rows []kobjects.Row
}

func (l *lines) emit(rows []kobjects.Row) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rows {
		b, _ := json.Marshal(r.Record("acceptance"))
		l.buf.Write(b)
		l.buf.WriteByte('\n')
		l.rows = append(l.rows, r)
	}
}

func (l *lines) has(kind, resource, name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.rows {
		if r.Kind == kind && r.GVR.Resource == resource && r.Name == name {
			return true
		}
	}
	return false
}

func (l *lines) write(t *testing.T, path string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, _ := zstd.NewWriter(f)
	if _, err := w.Write(l.buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// apiserverAt runs local/apiserver_at.sql over raw objects with the CLI and
// returns the state file apiserver-at reads.
func apiserverAt(t *testing.T, duckdb, rawFiles string, at time.Time, out string) {
	t.Helper()
	root, _ := filepath.Abs(repoRoot)
	script := fmt.Sprintf(`SET TimeZone = 'UTC';
SET VARIABLE raw_files = '%s';
.read %s/compactor/sql/raw.sql
.read %s/compactor/sql/tables.sql
.read %s/local/apiserver_at.sql
COPY (SELECT api_version, resource, namespace, name, apijson FROM apiserver_at(TIMESTAMP_NS '%s')) TO '%s' (FORMAT json);
`, rawFiles, root, root, root, at.UTC().Format("2006-01-02 15:04:05.000000000"), out)
	sf := out + ".sql"
	if err := os.WriteFile(sf, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(duckdb, "-bail", "-c", ".read "+sf)
	if b, err := cmd.CombinedOutput(); err != nil || len(bytes.TrimSpace(b)) > 0 {
		t.Fatalf("duckdb: %v\n%s", err, b)
	}
}

// timeRelative are the table columns whose cells say how long ago
// something happened, which two servers asked a moment apart may render
// differently.
var timeRelative = map[string]bool{"Age": true, "Last Seen": true, "First Seen": true, "Duration": true, "Last Schedule": true}

// table is the server-side printing kubectl get shows: the column names
// and the cells of every row, minus the time-relative columns.
func (c *cluster) table(t *testing.T, ctx context.Context, g schema.GroupVersionResource) []string {
	t.Helper()
	hc, err := rest.HTTPClientFor(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := "/apis/" + g.Group + "/" + g.Version + "/" + g.Resource
	if g.Group == "" {
		path = "/api/" + g.Version + "/" + g.Resource
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.cfg.Host, "/")+path, nil)
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("table %s: %v", g, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("table %s: %s %s", g, resp.Status, b)
	}
	var tb metav1.Table
	if err := json.Unmarshal(b, &tb); err != nil {
		t.Fatal(err)
	}
	var keep []int
	var head []string
	for i, cd := range tb.ColumnDefinitions {
		if !timeRelative[cd.Name] {
			keep = append(keep, i)
			head = append(head, cd.Name)
		}
	}
	out := []string{strings.Join(head, " | ")}
	var rows []string
	for _, r := range tb.Rows {
		var cells []string
		for _, i := range keep {
			cells = append(cells, fmt.Sprint(r.Cells[i]))
		}
		rows = append(rows, strings.Join(cells, " | "))
	}
	sort.Strings(rows)
	return append(out, rows...)
}

// kubectl runs the source's kubectl binary against a kubeconfig.
func kubectl(t *testing.T, assets, kubeconfig string, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(assets, "kubectl"), append([]string{"--kubeconfig", kubeconfig}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, b)
	}
	return string(b)
}

// ages are the durations kubectl renders relative to now.
var ages = regexp.MustCompile(`\b(\d+d)?(\d+h)?(\d+m)?\d+[smhd]\b`)

// timeless is kubectl output without ages, and without the column padding
// that follows their widths.
func timeless(s string) string {
	var out []string
	for _, l := range strings.Split(ages.ReplaceAllString(s, "<age>"), "\n") {
		out = append(out, strings.Join(strings.Fields(l), " "))
	}
	return strings.Join(out, "\n")
}

func TestAcceptance(t *testing.T) {
	assets, duckdb := needAssets(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()

	// --- the source cluster, and the agent's watcher on it
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Stop()
	src := newCluster(t, cfg)
	for _, u := range decode(t, widgetCRD) {
		src.apply(t, ctx, u)
	}
	waitFor(t, "the widget CRD to be served", func() bool {
		_, err := src.dyn.Resource(widgets).Namespace("default").List(ctx, metav1.ListOptions{})
		return err == nil
	})
	src.mapper.Reset()
	uids := map[string]string{}
	for _, u := range decode(t, fixtures) {
		created := src.apply(t, ctx, u)
		uids[u.GetKind()+"/"+u.GetName()] = string(created.GetUID())
	}
	for _, u := range decode(t, statuses) {
		src.setStatus(t, ctx, u)
	}
	// describe finds a pod's events by the pod's uid
	for _, u := range decode(t, fixtures) {
		if u.GetKind() == "Event" {
			_ = unstructured.SetNestedField(u.Object, uids["Pod/web-1"], "involvedObject", "uid")
			src.apply(t, ctx, u)
		}
	}

	raw := &lines{}
	w := kobjects.NewWatcher(kobjects.Config{SnapshotInterval: time.Hour, DiscoveryInterval: time.Second}, src.disc, src.dyn, raw.emit)
	wctx, stopWatcher := context.WithCancel(ctx)
	defer stopWatcher()
	go w.Run(wctx)
	gvrs, err := kobjects.Resources(src.disc, kobjects.DefaultExclude())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the watcher to snapshot every resource", func() bool { return len(w.Watching()) == len(gvrs) })
	t.Logf("watching %d resources", len(gvrs))

	// --- t1, then changes, then t2
	time.Sleep(50 * time.Millisecond)
	t1 := time.Now()
	at1 := src.dump(t, ctx, gvrs, true)
	time.Sleep(50 * time.Millisecond)

	for _, u := range decode(t, changes) {
		if u.GetKind() == "Event" {
			_ = unstructured.SetNestedField(u.Object, uids["Node/node1"], "involvedObject", "uid")
		}
		src.apply(t, ctx, u)
	}
	if err := src.dyn.Resource(secrets).Namespace("app").Delete(ctx, "creds", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Background: the default for Jobs, orphan, leaves a finalizer that only
	// the garbage collector removes, and none runs here
	bg := metav1.DeletePropagationBackground
	if err := src.dyn.Resource(jobs).Namespace("app").Delete(ctx, "migrate", metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][3]string{
		{kobjects.KindModified, "configmaps", "conf"}, {kobjects.KindAdded, "configmaps", "later"},
		{kobjects.KindModified, "widgets", "w1"}, {kobjects.KindAdded, "events", "node1.rebooted"},
		{kobjects.KindDeleted, "secrets", "creds"}, {kobjects.KindDeleted, "jobs", "migrate"},
	} {
		waitFor(t, strings.Join(r[:], " ")+" row", func() bool { return raw.has(r[0], r[1], r[2]) })
	}
	time.Sleep(50 * time.Millisecond)
	t2 := time.Now()
	at2 := src.dump(t, ctx, gvrs, true)
	stopWatcher()

	// --- the raw object, read back by the SQL every consumer uses
	rawFile := filepath.Join(dir, "0000-acceptance.jsonl.zst")
	raw.write(t, rawFile)

	// --- the source's own etcd keys are the keys apiserver-at writes
	t.Run("keys", func(t *testing.T) {
		have := etcdKeys(t, ctx, env.ControlPlane.Etcd.URL.String())
		crds := map[schema.GroupResource]bool{{Group: "example.com", Resource: "widgets"}: true}
		for _, g := range gvrs {
			for k := range at2[g.String()] {
				ns, name, _ := strings.Cut(k, "/")
				key := Key(g.GroupResource(), crds[g.GroupResource()], ns, name)
				if !have[key] {
					t.Errorf("%s %s: no etcd key %s", g, k, key)
				}
			}
		}
	})

	for _, c := range []struct {
		name string
		at   time.Time
		want state
	}{{"t1", t1, at1}, {"t2", t2, at2}} {
		t.Run(c.name, func(t *testing.T) {
			stateFile := filepath.Join(dir, c.name+".jsonl")
			apiserverAt(t, duckdb, rawFile, c.at, stateFile)
			f, err := os.Open(stateFile)
			if err != nil {
				t.Fatal(err)
			}
			ls, err := ReadLines(f)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			p, err := Prepare(ls)
			if err != nil {
				t.Fatal(err)
			}
			s, err := Start(ctx, Options{Assets: assets, Prepared: p, Label: c.name})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Stop()
			at := newCluster(t, &rest.Config{Host: s.URL})

			// every object, as the source had it at that time
			for _, d := range diff(c.want, at.dump(t, ctx, gvrs, false)) {
				if !expectedDifference(d) {
					t.Error(d)
				}
			}

			// a write is refused
			_, err = at.dyn.Resource(configmaps).Namespace("app").Create(ctx,
				decode(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, namespace: app}\n")[0], metav1.CreateOptions{})
			if !apierrors.IsMethodNotSupported(err) {
				t.Errorf("create: %v, want MethodNotAllowed", err)
			}

			if c.name != "t2" {
				return
			}
			// at t2 the source still is as it was: kubectl's tables and
			// describe must match it
			for _, g := range gvrs {
				want, got := src.table(t, ctx, g), at.table(t, ctx, g)
				if strings.Join(want, "\n") != strings.Join(got, "\n") && !expectedDifference(g.String()+" table") {
					t.Errorf("table %s:\nwant\n  %s\ngot\n  %s", g, strings.Join(want, "\n  "), strings.Join(got, "\n  "))
				}
			}
			srcKubeconfig := filepath.Join(dir, "source.kubeconfig")
			if err := os.WriteFile(srcKubeconfig, env.KubeConfig, 0o600); err != nil {
				t.Fatal(err)
			}
			atKubeconfig := filepath.Join(dir, "at.kubeconfig")
			b, _ := s.KubeConfig("at")
			if err := os.WriteFile(atKubeconfig, b, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"describe", "pod", "-n", "app", "web-1"},
				{"describe", "node", "node1"},
				{"describe", "deployment", "-n", "app", "web"},
				{"get", "pods,deploy,svc,ing,cm,secrets,widgets,events", "-A", "-o", "wide"},
			} {
				want := timeless(kubectl(t, assets, srcKubeconfig, args...))
				got := timeless(kubectl(t, assets, atKubeconfig, args...))
				if want != got {
					t.Errorf("kubectl %s:\nwant\n%s\ngot\n%s", strings.Join(args, " "), want, got)
				}
			}
			t.Logf("kubectl get at t2 through apiserver-at:\n%s", kubectl(t, assets, atKubeconfig, "get", "pods,cm,secrets,widgets,events", "-A"))
		})
	}
}

var (
	widgets    = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	secrets    = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	configmaps = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	jobs       = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
)

// expectedDifference is what apiserver-at shows differently from its
// source, by design; each with the reason.
func expectedDifference(d string) bool {
	for _, prefix := range []string{
		// kube-apiserver writes its own client CAs here when it starts
		"/v1, Resource=configmaps kube-system/extension-apiserver-authentication: differs",
	} {
		if strings.HasPrefix(d, prefix) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func etcdKeys(t *testing.T, ctx context.Context, endpoint string) map[string]bool {
	t.Helper()
	c, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := c.Get(ctx, etcdPrefix+"/", clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, kv := range r.Kvs {
		have[string(kv.Key)] = true
	}
	return have
}
