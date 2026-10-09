package kobjects

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

var (
	configmaps = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets    = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	events     = schema.GroupVersionResource{Version: "v1", Resource: "events"}
)

func obj(apiVersion, kind, ns, name, rv string, fields map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": apiVersion, "kind": kind}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetUID(types.UID("uid-" + name))
	u.SetResourceVersion(rv)
	return u
}

func TestSanitizeSecret(t *testing.T) {
	u := obj("v1", "Secret", "app", "creds", "1", map[string]interface{}{
		"type": "Opaque",
		"data": map[string]interface{}{"password": "aHVudGVyMg==", "user": "YWRtaW4="},
	})
	u.SetAnnotations(map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"password":"aHVudGVyMg=="}}`,
		"keep": "this",
	})
	u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}})
	Sanitize(secrets, u)
	// hunter2 and admin, 7 and 5 bytes: one random character each
	data := u.Object["data"].(map[string]interface{})
	if len(data) != 2 {
		t.Fatalf("data %v", data)
	}
	for k, v := range data {
		if !scrambled(t, v, 1) {
			t.Fatalf("%s: %v", k, v)
		}
	}
	want := map[string]string{"keep": "this", RedactedAnnotation: "data"}
	if got := u.GetAnnotations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("annotations %v", got)
	}
	if u.GetManagedFields() != nil {
		t.Fatal("managedFields kept")
	}
	if u.Object["type"] != "Opaque" {
		t.Fatal("type lost")
	}
}

// scrambled tells whether v is the base64 of ScrambledPrefix and n random
// letters and digits.
func scrambled(t *testing.T, v interface{}, n int) bool {
	t.Helper()
	s, _ := v.(string)
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && regexp.MustCompile(fmt.Sprintf(`^k8sobs_[A-Za-z0-9]{%d}$`, n)).Match(b)
}

func TestScramble(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 1, 10: 1, 11: 2, 64: 7, 4096: 410} {
		s := Scramble(n)
		if !strings.HasPrefix(s, ScrambledPrefix) || len(s) != len(ScrambledPrefix)+want {
			t.Errorf("Scramble(%d) = %q, want the prefix and %d characters", n, s, want)
		}
		if strings.Trim(s[len(ScrambledPrefix):], scrambleAlphabet) != "" {
			t.Errorf("Scramble(%d) = %q: not letters and digits", n, s)
		}
	}
	if Scramble(4096) == Scramble(4096) {
		t.Error("two scrambles of a value alike")
	}
}

func TestSanitizeSecretStringData(t *testing.T) {
	u := obj("v1", "Secret", "app", "s", "1", map[string]interface{}{
		"stringData": map[string]interface{}{"token": strings.Repeat("x", 25), "none": ""},
		"data":       map[string]interface{}{"empty": ""},
	})
	Sanitize(secrets, u)
	if v := u.Object["stringData"].(map[string]interface{})["token"].(string); !regexp.MustCompile(`^k8sobs_[A-Za-z0-9]{3}$`).MatchString(v) {
		t.Errorf("stringData %q", v)
	}
	// an empty value stays empty
	if v := u.Object["data"].(map[string]interface{})["empty"]; v != "" {
		t.Errorf("an empty data value: %v", v)
	}
	if v := u.Object["stringData"].(map[string]interface{})["none"]; v != "" {
		t.Errorf("an empty stringData value: %v", v)
	}
}

func TestSanitizeOthersKeepData(t *testing.T) {
	u := obj("v1", "ConfigMap", "app", "conf", "1", map[string]interface{}{"data": map[string]interface{}{"a": "b"}})
	u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}})
	Sanitize(configmaps, u)
	if !reflect.DeepEqual(u.Object["data"], map[string]interface{}{"a": "b"}) || u.GetAnnotations() != nil {
		t.Fatalf("changed %v", u.Object)
	}
	if u.GetManagedFields() != nil {
		t.Fatal("managedFields kept")
	}
	// a resource named secrets in another group is not a core Secret
	other := obj("example.com/v1", "Secret", "app", "x", "1", map[string]interface{}{"data": map[string]interface{}{"a": "b"}})
	Sanitize(schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "secrets"}, other)
	if !reflect.DeepEqual(other.Object["data"], map[string]interface{}{"a": "b"}) {
		t.Fatal("redacted a custom resource")
	}
}

func TestRecordFieldsByKind(t *testing.T) {
	at := time.Date(2026, 10, 7, 10, 0, 0, 123456789, time.UTC)
	deploy := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	r, err := objectRow(at, KindModified, deploy, obj("apps/v1", "Deployment", "app", "web", "42", nil))
	if err != nil {
		t.Fatal(err)
	}
	rec := r.Record("c1")
	for k, v := range map[string]interface{}{
		"table": "apiserver", "time": "2026-10-07T10:00:00.123456789Z", "cluster": "c1", "kind": KindModified,
		"api_version": "apps/v1", "resource": "deployments", "namespace": "app", "name": "web",
		"object_uid": "uid-web", "resource_version": "42",
	} {
		if rec[k] != v {
			t.Errorf("%s: %v, want %v", k, rec[k], v)
		}
	}
	var back map[string]interface{}
	if err := json.Unmarshal([]byte(rec["apijson"].(string)), &back); err != nil || back["kind"] != "Deployment" {
		t.Errorf("apijson %v %v", rec["apijson"], err)
	}
	if _, ok := rec["rows"]; ok {
		t.Error("rows on an object row")
	}

	cluster := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	r, _ = objectRow(at, KindAdded, cluster, obj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "view", "7", nil))
	if _, ok := r.Record("c1")["namespace"]; ok {
		t.Error("namespace on a cluster-scoped object")
	}

	snap := Row{Time: at, Kind: KindSnapshot, GVR: configmaps, Rows: 3}.Record("c1")
	want := map[string]interface{}{"table": "apiserver", "time": "2026-10-07T10:00:00.123456789Z", "cluster": "c1",
		"kind": KindSnapshot, "api_version": "v1", "resource": "configmaps", "rows": 3}
	if !reflect.DeepEqual(snap, want) {
		t.Errorf("snapshot row %v", snap)
	}
}

func fakeDiscovery(lists ...*metav1.APIResourceList) *fakediscovery.FakeDiscovery {
	return &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{Resources: lists}}
}

func TestResources(t *testing.T) {
	rw := []string{"get", "list", "watch", "create"}
	d := fakeDiscovery(
		&metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", Namespaced: true, Verbs: rw},
			{Name: "pods/log", Namespaced: true, Verbs: []string{"get"}},
			{Name: "pods/status", Namespaced: true, Verbs: rw},
			{Name: "events", Namespaced: true, Verbs: rw},
			{Name: "bindings", Namespaced: true, Verbs: []string{"create"}},
			{Name: "componentstatuses", Verbs: []string{"get", "list"}},
		}},
		&metav1.APIResourceList{GroupVersion: "events.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "events", Namespaced: true, Verbs: rw},
		}},
		&metav1.APIResourceList{GroupVersion: "coordination.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "leases", Namespaced: true, Verbs: rw},
		}},
		&metav1.APIResourceList{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", Namespaced: true, Verbs: rw},
		}},
	)
	got, err := Resources(d, DefaultExclude())
	if err != nil {
		t.Fatal(err)
	}
	want := []schema.GroupVersionResource{
		{Version: "v1", Resource: "events"},
		{Version: "v1", Resource: "pods"},
		{Group: "example.com", Version: "v1", Resource: "widgets"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	got, _ = Resources(d, ParseExclude("pods, widgets.example.com"))
	want = []schema.GroupVersionResource{
		{Version: "v1", Resource: "events"},
		{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"},
		{Group: "events.k8s.io", Version: "v1", Resource: "events"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exclude: got %v", got)
	}
}

func TestUntilNextSnapshot(t *testing.T) {
	w := NewWatcher(Config{SnapshotInterval: time.Hour}, nil, nil, nil)
	w.now = func() time.Time { return time.Date(2026, 10, 7, 10, 59, 30, 0, time.UTC) }
	if d := w.untilNextSnapshot(); d != 30*time.Second {
		t.Fatalf("%s", d)
	}
	// a timer that fires a hair early does not snapshot twice in a row
	w.now = func() time.Time { return time.Date(2026, 10, 7, 10, 59, 59, 999e6, time.UTC) }
	if d := w.untilNextSnapshot(); d != time.Hour+time.Millisecond {
		t.Fatalf("%s", d)
	}
}

// collector keeps every row emitted, in emit order.
type collector struct {
	mu   sync.Mutex
	rows []Row
}

func (c *collector) emit(rows []Row) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, rows...)
}

func (c *collector) await(t *testing.T, what string, ok func([]Row) bool) []Row {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		rows := append([]Row(nil), c.rows...)
		c.mu.Unlock()
		if ok(rows) {
			return rows
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("timed out waiting for %s; rows: %v", what, summarize(c.rows))
	return nil
}

func summarize(rows []Row) []string {
	var s []string
	for _, r := range rows {
		s = append(s, r.Kind+" "+r.GVR.Resource+" "+r.Namespace+"/"+r.Name)
	}
	return s
}

func count(rows []Row, kind, resource, name string) int {
	n := 0
	for _, r := range rows {
		if r.Kind == kind && r.GVR.Resource == resource && r.Name == name {
			n++
		}
	}
	return n
}

func TestWatcherSnapshotsThenEvents(t *testing.T) {
	rw := []string{"get", "list", "watch"}
	d := fakeDiscovery(&metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "configmaps", Namespaced: true, Verbs: rw},
		{Name: "secrets", Namespaced: true, Verbs: rw},
		{Name: "events", Namespaced: true, Verbs: rw},
	}})
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		configmaps: "ConfigMapList", secrets: "SecretList", events: "EventList",
	},
		obj("v1", "ConfigMap", "app", "conf", "1", map[string]interface{}{"data": map[string]interface{}{"k": "v"}}),
		obj("v1", "Secret", "app", "creds", "2", map[string]interface{}{"data": map[string]interface{}{"password": "aHVudGVyMg=="}}),
	)
	c := &collector{}
	w := NewWatcher(Config{SnapshotInterval: time.Hour, DiscoveryInterval: time.Hour, SecretsMode: SecretsScrambled}, d, dyn, c.emit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// the initial lists are snapshots: object rows, then the snapshot row
	rows := c.await(t, "three snapshot rows", func(rows []Row) bool {
		return count(rows, KindSnapshot, "configmaps", "")+count(rows, KindSnapshot, "secrets", "")+
			count(rows, KindSnapshot, "events", "") == 3
	})
	for _, r := range rows {
		if r.Kind == KindAdded {
			t.Fatalf("initial list emitted as added: %v", summarize(rows))
		}
	}
	for i, r := range rows {
		if r.Kind != KindSnapshot {
			continue
		}
		want := map[string]int{"configmaps": 1, "secrets": 1, "events": 0}[r.GVR.Resource]
		if r.Rows != want {
			t.Errorf("snapshot of %s: %d rows, want %d", r.GVR.Resource, r.Rows, want)
		}
		if want == 1 && (i == 0 || rows[i-1].Kind != KindSnapshotObject || rows[i-1].GVR != r.GVR || !rows[i-1].Time.Equal(r.Time)) {
			t.Errorf("snapshot row of %s does not follow its object at its time", r.GVR.Resource)
		}
	}

	// changes after the initial list are events, in all namespaces
	cm := dyn.Resource(configmaps)
	if _, err := cm.Namespace("app").Create(ctx, obj("v1", "ConfigMap", "app", "new", "3", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	upd := obj("v1", "ConfigMap", "app", "conf", "4", map[string]interface{}{"data": map[string]interface{}{"k": "v2"}})
	if _, err := cm.Namespace("app").Update(ctx, upd, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := dyn.Resource(secrets).Namespace("app").Delete(ctx, "creds", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	ev := obj("v1", "Event", "default", "node1.abc", "5", map[string]interface{}{
		"involvedObject": map[string]interface{}{"kind": "Node", "name": "node1"}, "reason": "Rebooted",
	})
	if _, err := dyn.Resource(events).Namespace("default").Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rows = c.await(t, "the events", func(rows []Row) bool {
		return count(rows, KindAdded, "configmaps", "new") == 1 && count(rows, KindModified, "configmaps", "conf") == 1 &&
			count(rows, KindDeleted, "secrets", "creds") == 1 && count(rows, KindAdded, "events", "node1.abc") == 1
	})

	// a requested snapshot has the current state at one time
	w.RequestSnapshot()
	rows = c.await(t, "the second snapshot", func(rows []Row) bool { return count(rows, KindSnapshot, "configmaps", "") == 2 })
	var at time.Time
	got := map[string]string{}
	for _, r := range rows {
		if r.Kind == KindSnapshot && r.GVR == configmaps {
			at = r.Time
		}
	}
	for _, r := range rows {
		if r.Kind == KindSnapshotObject && r.GVR == configmaps && r.Time.Equal(at) {
			got[r.Name] = r.ResourceVersion
		}
	}
	if !reflect.DeepEqual(got, map[string]string{"conf": "4", "new": "3"}) {
		t.Fatalf("second snapshot %v", got)
	}

	// no row ever carries Secret data
	for _, r := range rows {
		if r.GVR == secrets && r.Object != nil {
			var o map[string]interface{}
			_ = json.Unmarshal(r.Object, &o)
			d, _ := o["data"].(map[string]interface{})
			if len(d) != 1 || !scrambled(t, d["password"], 1) {
				t.Fatalf("%s secret row with data %v", r.Kind, o["data"])
			}
		}
	}
	if got := w.Watching(); len(got) != 3 {
		t.Fatalf("watching %v", got)
	}
}

func TestWatcherSkipsWhatItMayNotList(t *testing.T) {
	rw := []string{"get", "list", "watch"}
	d := fakeDiscovery(&metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "configmaps", Namespaced: true, Verbs: rw},
		{Name: "secrets", Namespaced: true, Verbs: rw},
	}})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		configmaps: "ConfigMapList", secrets: "SecretList",
	}, obj("v1", "ConfigMap", "app", "conf", "1", nil))
	dyn.PrependReactor("list", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC"))
	})
	c := &collector{}
	w := NewWatcher(Config{SnapshotInterval: time.Hour, DiscoveryInterval: time.Hour}, d, dyn, c.emit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	c.await(t, "the configmaps snapshot", func(rows []Row) bool { return count(rows, KindSnapshot, "configmaps", "") == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := w.Watching(); !reflect.DeepEqual(got, []schema.GroupVersionResource{configmaps}) {
		t.Fatalf("watching %v", got)
	}
	for _, r := range c.rows {
		if r.GVR == secrets {
			t.Fatalf("a row of a resource it may not list: %v", summarize(c.rows))
		}
	}
}

func TestParseSecretsMode(t *testing.T) {
	for in, want := range map[string]SecretsMode{"": SecretsSkip, "skip": SecretsSkip, " scrambled ": SecretsScrambled} {
		got, err := ParseSecretsMode(in)
		if err != nil || got != want {
			t.Errorf("ParseSecretsMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"Skip", "raw", "plain", "on"} {
		if _, err := ParseSecretsMode(in); err == nil {
			t.Errorf("ParseSecretsMode(%q) accepted", in)
		}
	}
}

// Secrets are watched only when scrambled is asked for; skip is the default,
// and the caller's exclusions are kept and not modified.
func TestSecretsModeExcludesSecrets(t *testing.T) {
	rw := []string{"get", "list", "watch"}
	d := fakeDiscovery(&metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "configmaps", Namespaced: true, Verbs: rw},
		{Name: "secrets", Namespaced: true, Verbs: rw},
	}})
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	watched := func(mode SecretsMode) bool {
		given := DefaultExclude()
		w := NewWatcher(Config{Exclude: given, SecretsMode: mode}, d, nil, nil)
		if given[secretsResource] {
			t.Fatalf("mode %q modified the caller's exclusions", mode)
		}
		got, err := Resources(d, w.cfg.Exclude)
		if err != nil {
			t.Fatal(err)
		}
		for _, gvr := range got {
			if gvr == secrets {
				return true
			}
		}
		return false
	}
	if watched("") || watched(SecretsSkip) {
		t.Error("Secrets watched without secrets_mode scrambled")
	}
	if !watched(SecretsScrambled) {
		t.Error("Secrets not watched with secrets_mode scrambled")
	}
}
