// Package kobjects watches every listable resource of a cluster and turns
// what it sees into the rows of the raw tier's objects table (K8SAGENT.md):
// one row per add, change and delete, a full snapshot of each resource at
// every snapshot interval and when its watch starts, and a snapshot row per
// resource that says the snapshot before it is complete.
//
// It has no cgo and no Fluent Bit: the k8s_objects input plugin encodes the
// rows' records as msgpack, the acceptance test as JSON lines.
package kobjects

import (
	"encoding/json"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TimeFormat is the raw tier's time: ISO 8601 UTC with nanoseconds.
const TimeFormat = "2006-01-02T15:04:05.000000000Z"

// The kinds of objects rows.
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

// RedactedAnnotation marks a Secret whose data values the agent emptied.
const RedactedAnnotation = "observability-to-blobs/redacted"

// Row is one line of the objects table.
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
	// Objects is the snapshot's row count, on snapshot rows only
	Objects int
}

// APIVersion is the row's group/version as objects carry it: "v1", "apps/v1".
func (r Row) APIVersion() string { return r.GVR.GroupVersion().String() }

// Record is the row as the raw line's fields. Fields that do not apply to
// the row's kind are absent, not empty.
func (r Row) Record(cluster string) map[string]interface{} {
	rec := map[string]interface{}{
		"table":       "objects",
		"time":        r.Time.UTC().Format(TimeFormat),
		"cluster":     cluster,
		"kind":        r.Kind,
		"api_version": r.APIVersion(),
		"resource":    r.GVR.Resource,
	}
	if r.Kind == KindSnapshot {
		rec["objects"] = r.Objects
		return rec
	}
	if r.Namespace != "" {
		rec["namespace"] = r.Namespace
	}
	rec["name"] = r.Name
	rec["object_uid"] = r.UID
	rec["resource_version"] = r.ResourceVersion
	rec["object"] = string(r.Object)
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
// Secret its data keys with empty values and none of the copies of the data
// that annotations can hold. The Secret keeps its type and key names, so
// kubectl get secrets still counts the keys. It changes u in place.
func Sanitize(gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	u.SetManagedFields(nil)
	if gvr.Group != "" || gvr.Resource != "secrets" {
		return
	}
	for _, field := range []string{"data", "stringData"} {
		m, ok := u.Object[field].(map[string]interface{})
		if !ok {
			continue
		}
		for k := range m {
			m[k] = ""
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
