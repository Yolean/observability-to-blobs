// Package kobjects watches every listable resource of a cluster and turns
// what it sees into the rows of the raw tier's apiserver table (K8SAGENT.md):
// one row per add, change and delete, a full snapshot of each resource at
// every snapshot interval and when its watch starts, and a snapshot row per
// resource that says the snapshot before it is complete.
//
// It has no cgo and no Fluent Bit: the k8s_objects input plugin encodes the
// rows' records as msgpack, the acceptance test as JSON lines.
package kobjects

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TimeFormat is the raw tier's time: ISO 8601 UTC with nanoseconds.
const TimeFormat = "2006-01-02T15:04:05.000000000Z"

// The kinds of apiserver rows.
const (
	// a watch event after the resource's initial list
	KindAdded    = "object-added"
	KindModified = "object-modified"
	KindDeleted  = "object-deleted"
	// one row per object of a snapshot, all with the snapshot's time
	KindSnapshotObject = "object-snapshot"
	// written after a resource's snapshot rows: the snapshot at this time
	// is complete and holds Objects rows
	KindSnapshot = "snapshot"
)

// RedactedAnnotation marks an object the agent changed: "data" on a Secret
// whose values it scrambled, "schemas" or "schemas-except-storage" on a
// CustomResourceDefinition whose schemas it replaced (RedactCRD).
const RedactedAnnotation = "observability-to-blobs/redacted"

// ScrambledPrefix starts every Secret value the agent ships. The rest is
// random letters and digits, a tenth of the original value's length in
// bytes, rounded up: a reader sees that a key had a value, and roughly how
// long it was, and nothing of the value itself. An empty value stays
// empty: it has nothing to hide, and empty says more than a prefix would.
const ScrambledPrefix = "k8sobs_"

// Row is one line of the apiserver table.
type Row struct {
	Time            time.Time
	Kind            string
	GVR             schema.GroupVersionResource
	Namespace       string
	Name            string
	UID             string
	ResourceVersion string
	// Object is the sanitized object as JSON; nil on snapshot rows
	Object []byte
	// Rows is the snapshot's count of object-snapshot rows, on snapshot rows only
	Rows int
}

// APIVersion is the row's group/version as objects carry it: "v1", "apps/v1".
func (r Row) APIVersion() string { return r.GVR.GroupVersion().String() }

// Record is the row as the raw line's fields. Fields that do not apply to
// the row's kind are absent, not empty.
func (r Row) Record(cluster string) map[string]interface{} {
	rec := map[string]interface{}{
		"table":       "apiserver",
		"time":        r.Time.UTC().Format(TimeFormat),
		"cluster":     cluster,
		"kind":        r.Kind,
		"api_version": r.APIVersion(),
		"resource":    r.GVR.Resource,
	}
	if r.Kind == KindSnapshot {
		rec["rows"] = r.Rows
		return rec
	}
	if r.Namespace != "" {
		rec["namespace"] = r.Namespace
	}
	rec["name"] = r.Name
	rec["object_uid"] = r.UID
	rec["resource_version"] = r.ResourceVersion
	rec["apijson"] = string(r.Object)
	return rec
}

// objectRow builds the row of an object that Sanitize has already seen.
func objectRow(now time.Time, kind string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) (Row, error) {
	b, err := json.Marshal(u.Object)
	if err != nil {
		return Row{}, err
	}
	return Row{
		Time:            now,
		Kind:            kind,
		GVR:             gvr,
		Namespace:       u.GetNamespace(),
		Name:            u.GetName(),
		UID:             string(u.GetUID()),
		ResourceVersion: u.GetResourceVersion(),
		Object:          b,
	}, nil
}

// Sanitize is what leaves the cluster of an object: managedFields never
// (bookkeeping of field owners, often larger than the object), and of a
// Secret its data keys with scrambled values (Scramble) and none of the
// copies of the data that annotations can hold. The Secret keeps its type
// and key names, so kubectl get secrets still counts the keys. It changes u
// in place; the watcher calls it as objects enter the informer's store, so
// the agent never holds a Secret's values.
func Sanitize(gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	u.SetManagedFields(nil)
	if gvr.Group != "" || gvr.Resource != "secrets" {
		return
	}
	// data holds base64, which the scrambled value is too, so that an API
	// server can still read the Secret; stringData holds the plain value
	if m, ok := u.Object["data"].(map[string]interface{}); ok {
		for k, v := range m {
			s, _ := v.(string)
			n := len(s)
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				n = len(b)
			}
			if n == 0 {
				m[k] = ""
				continue
			}
			m[k] = base64.StdEncoding.EncodeToString([]byte(Scramble(n)))
		}
	}
	if m, ok := u.Object["stringData"].(map[string]interface{}); ok {
		for k, v := range m {
			if s, _ := v.(string); s != "" {
				m[k] = Scramble(len(s))
			}
		}
	}
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	// kubectl apply keeps the applied manifest, data included
	delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
	ann[RedactedAnnotation] = "data"
	u.SetAnnotations(ann)
}

const scrambleAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Scramble is the value shipped for a Secret value of n bytes:
// ScrambledPrefix and ceil(n/10) random letters and digits.
func Scramble(n int) string {
	out := []byte(ScrambledPrefix)
	want := len(out) + (n+9)/10
	var buf [64]byte
	for len(out) < want {
		if _, err := rand.Read(buf[:]); err != nil {
			panic(err) // crypto/rand does not fail
		}
		for _, b := range buf {
			// 248 is the largest multiple of 62 in a byte: no bias
			if b < 248 && len(out) < want {
				out = append(out, scrambleAlphabet[b%62])
			}
		}
	}
	return string(out)
}

// RedactCRD removes from a CustomResourceDefinition what is large and that
// nothing read from the blobs needs: the versions' OpenAPI schemas (all of
// them in CRDRedacted, all but the storage version's in CRDStorageSchema),
// the conversion webhook's caBundle, and the last-applied-configuration
// annotation, which holds the whole CRD again. A replaced schema is
// PlaceholderSchema, which accepts any object, so an API server still
// serves the custom resources: their objects as stored, with the CRD's
// printer columns, subresources and conversion strategy, all of which are
// kept, as are the names, scope, versions and status. It marks the CRD
// with RedactedAnnotation. Other objects, and CRDFull, are left as they are.
func RedactCRD(gvr schema.GroupVersionResource, u *unstructured.Unstructured, mode CRDMode) {
	if mode == CRDFull || gvr.Group != "apiextensions.k8s.io" || gvr.Resource != "customresourcedefinitions" {
		return
	}
	spec, _ := u.Object["spec"].(map[string]interface{})
	versions, _ := spec["versions"].([]interface{})
	for _, v := range versions {
		ver, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if storage, _ := ver["storage"].(bool); storage && mode == CRDStorageSchema {
			continue
		}
		if _, ok := ver["schema"]; ok {
			ver["schema"] = map[string]interface{}{"openAPIV3Schema": PlaceholderSchema()}
		}
	}
	if conv, ok := spec["conversion"].(map[string]interface{}); ok {
		if wh, ok := conv["webhook"].(map[string]interface{}); ok {
			if cc, ok := wh["clientConfig"].(map[string]interface{}); ok {
				delete(cc, "caBundle")
			}
		}
	}
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
	ann[RedactedAnnotation] = "schemas"
	if mode == CRDStorageSchema {
		ann[RedactedAnnotation] = "schemas-except-storage"
	}
	u.SetAnnotations(ann)
}

// PlaceholderSchema is the schema RedactCRD leaves in place of a removed
// one: a structural schema that accepts any object and says why it is
// empty, for kubectl explain.
func PlaceholderSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                                 "object",
		"x-kubernetes-preserve-unknown-fields": true,
		"description":                          "Schema removed by the observability-to-blobs cluster agent (crd_mode).",
	}
}
