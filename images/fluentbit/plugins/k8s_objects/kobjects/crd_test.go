package kobjects

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

var crds = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// a CRD of two versions, v1beta1 served and v1 stored, each with a schema,
// a conversion webhook and the applied manifest in an annotation
func gadgetCRD() *unstructured.Unstructured {
	big := func(v string) map[string]interface{} {
		return map[string]interface{}{"openAPIV3Schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{"spec": map[string]interface{}{
				"type": "object", "description": strings.Repeat(v+" ", 1000),
				"properties": map[string]interface{}{"size": map[string]interface{}{"type": "integer"}},
			}},
		}}
	}
	columns := []interface{}{
		map[string]interface{}{"name": "Size", "type": "integer", "jsonPath": ".spec.size"},
		map[string]interface{}{"name": "Color", "type": "string", "jsonPath": ".spec.color", "priority": int64(1)},
	}
	u := obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "gadgets.example.com", "7", map[string]interface{}{
		"spec": map[string]interface{}{
			"group": "example.com",
			"scope": "Namespaced",
			"names": map[string]interface{}{"plural": "gadgets", "singular": "gadget", "kind": "Gadget", "listKind": "GadgetList"},
			"versions": []interface{}{
				map[string]interface{}{"name": "v1beta1", "served": true, "storage": false, "deprecated": true,
					"deprecationWarning": "use v1", "schema": big("v1beta1"), "additionalPrinterColumns": columns},
				map[string]interface{}{"name": "v1", "served": true, "storage": true, "schema": big("v1"),
					"additionalPrinterColumns": columns, "subresources": map[string]interface{}{"status": map[string]interface{}{}}},
			},
			"conversion": map[string]interface{}{"strategy": "Webhook", "webhook": map[string]interface{}{
				"conversionReviewVersions": []interface{}{"v1"},
				"clientConfig": map[string]interface{}{
					"caBundle": "LS0tLS1CRUdJTi...",
					"service":  map[string]interface{}{"namespace": "gadgets", "name": "webhook", "path": "/convert"},
				},
			}},
		},
		"status": map[string]interface{}{
			"storedVersions": []interface{}{"v1beta1", "v1"},
			"acceptedNames":  map[string]interface{}{"plural": "gadgets", "kind": "Gadget"},
			"conditions":     []interface{}{map[string]interface{}{"type": "Established", "status": "True"}},
		},
	})
	u.SetAnnotations(map[string]string{"kubectl.kubernetes.io/last-applied-configuration": `{"spec":"the whole CRD"}`, "keep": "this"})
	return u
}

func TestRedactCRD(t *testing.T) {
	// the versions themselves, not a copy (NestedSlice copies)
	versions := func(u *unstructured.Unstructured) []interface{} {
		return u.Object["spec"].(map[string]interface{})["versions"].([]interface{})
	}
	schemaOf := func(v interface{}) interface{} { return v.(map[string]interface{})["schema"] }
	placeholder := map[string]interface{}{"openAPIV3Schema": PlaceholderSchema()}

	for _, c := range []struct {
		mode      CRDMode
		mark      string
		keepFirst bool
		keepLast  bool
	}{
		{CRDRedacted, "schemas", false, false},
		{CRDStorageSchema, "schemas-except-storage", false, true},
	} {
		t.Run(string(c.mode), func(t *testing.T) {
			orig := gadgetCRD()
			u := gadgetCRD()
			RedactCRD(crds, u, c.mode)

			// the schemas: replaced, but for the storage version's in storage-schema
			for i, keep := range []bool{c.keepFirst, c.keepLast} {
				got, want := schemaOf(versions(u)[i]), interface{}(placeholder)
				if keep {
					want = schemaOf(versions(orig)[i])
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("version %d schema %.200v", i, got)
				}
			}
			// the caBundle goes, the webhook's service stays
			cc, _, _ := unstructured.NestedMap(u.Object, "spec", "conversion", "webhook", "clientConfig")
			if _, ok := cc["caBundle"]; ok || cc["service"] == nil {
				t.Errorf("clientConfig %v", cc)
			}
			ann := u.GetAnnotations()
			if ann[RedactedAnnotation] != c.mark || ann["keep"] != "this" || ann["kubectl.kubernetes.io/last-applied-configuration"] != "" {
				t.Errorf("annotations %v", ann)
			}

			// everything else is as it was: put back what was replaced and compare
			for i := range versions(u) {
				versions(u)[i].(map[string]interface{})["schema"] = schemaOf(versions(orig)[i])
			}
			cc["caBundle"] = "LS0tLS1CRUdJTi..."
			_ = unstructured.SetNestedMap(u.Object, cc, "spec", "conversion", "webhook", "clientConfig")
			u.SetAnnotations(orig.GetAnnotations())
			if !reflect.DeepEqual(u.Object, orig.Object) {
				t.Errorf("more than the schemas, caBundle and annotations changed")
			}
		})
	}

	// most of the size goes
	full, _ := json.Marshal(gadgetCRD().Object)
	u := gadgetCRD()
	RedactCRD(crds, u, CRDRedacted)
	redacted, _ := json.Marshal(u.Object)
	if len(redacted)*4 > len(full) {
		t.Errorf("redacted %d bytes of %d", len(redacted), len(full))
	}

	// full, and other resources, are left alone
	u = gadgetCRD()
	RedactCRD(crds, u, CRDFull)
	if !reflect.DeepEqual(u.Object, gadgetCRD().Object) {
		t.Error("crd_mode full changed the CRD")
	}
	cm := obj("v1", "ConfigMap", "app", "conf", "1", map[string]interface{}{"spec": map[string]interface{}{"versions": []interface{}{map[string]interface{}{"schema": "x"}}}})
	before := cm.DeepCopy()
	RedactCRD(configmaps, cm, CRDRedacted)
	if !reflect.DeepEqual(cm.Object, before.Object) {
		t.Error("RedactCRD changed a ConfigMap")
	}
}

func TestParseCRDMode(t *testing.T) {
	for in, want := range map[string]CRDMode{"": CRDRedacted, "redacted": CRDRedacted, " storage-schema ": CRDStorageSchema, "full": CRDFull} {
		got, err := ParseCRDMode(in)
		if err != nil || got != want {
			t.Errorf("ParseCRDMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"none", "Full", "schemas"} {
		if _, err := ParseCRDMode(in); err == nil {
			t.Errorf("ParseCRDMode(%q) accepted", in)
		}
	}
}

// The watcher redacts CRDs by default, in snapshots and in changes.
func TestWatcherRedactsCRDs(t *testing.T) {
	rw := []string{"get", "list", "watch"}
	d := fakeDiscovery(&metav1.APIResourceList{GroupVersion: "apiextensions.k8s.io/v1", APIResources: []metav1.APIResource{
		{Name: "customresourcedefinitions", Verbs: rw},
	}})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crds: "CustomResourceDefinitionList",
	}, gadgetCRD())
	c := &collector{}
	w := NewWatcher(Config{SnapshotInterval: time.Hour, DiscoveryInterval: time.Hour}, d, dyn, c.emit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	c.await(t, "the CRD snapshot", func(rows []Row) bool { return count(rows, KindSnapshot, "customresourcedefinitions", "") == 1 })
	other := gadgetCRD()
	other.SetName("widgets.example.com")
	other.SetResourceVersion("8")
	if _, err := dyn.Resource(crds).Create(ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rows := c.await(t, "the added CRD", func(rows []Row) bool {
		return count(rows, KindAdded, "customresourcedefinitions", "widgets.example.com") == 1
	})
	n := 0
	for _, r := range rows {
		if r.Object == nil {
			continue
		}
		n++
		if strings.Contains(string(r.Object), "v1beta1 v1beta1") || !strings.Contains(string(r.Object), `"observability-to-blobs/redacted":"schemas"`) {
			t.Errorf("%s %s: not redacted: %.300s", r.Kind, r.Name, r.Object)
		}
	}
	if n != 2 {
		t.Errorf("%d CRD rows, want 2", n)
	}
}
