package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// local/apiserver_at.sql on crafted raw rows: the cases a short-lived source
// cluster cannot produce on demand.

func row(at, kind, apiVersion, resource, ns, name, uid, rv string) map[string]interface{} {
	r := map[string]interface{}{"table": "apiserver", "time": "2026-10-07T" + at + ":00.000000000Z", "cluster": "c",
		"kind": kind, "api_version": apiVersion, "resource": resource}
	if kind == "snapshot" {
		r["rows"] = 0
		return r
	}
	if ns != "" {
		r["namespace"] = ns
	}
	r["name"], r["object_uid"], r["resource_version"] = name, uid, rv
	r["apijson"] = `{"metadata":{"name":"` + name + `","uid":"` + uid + `"}}`
	return r
}

func cm(at, kind, name, uid, rv string) map[string]interface{} {
	return row(at, kind, "v1", "configmaps", "app", name, uid, rv)
}

func TestObjectsAt(t *testing.T) {
	duckdb := os.Getenv("DUCKDB")
	if duckdb == "" {
		var err error
		if duckdb, err = exec.LookPath("duckdb"); err != nil {
			t.Skip("no duckdb on PATH and no DUCKDB")
		}
	}
	rows := []map[string]interface{}{
		// an event before the resource's first snapshot is not a state
		cm("09:58", "object-added", "early", "u0", "1"),
		// snapshot at 10:00
		cm("10:00", "object-snapshot", "a", "ua", "10"),
		cm("10:00", "object-snapshot", "b", "ub", "11"),
		cm("10:00", "snapshot", "", "", ""),
		cm("10:10", "object-modified", "a", "ua", "13"),
		// a name reused: deleted, then created with another uid
		cm("10:12", "object-added", "d", "ud1", "14"),
		cm("10:14", "object-deleted", "d", "ud1", "15"),
		cm("10:16", "object-added", "d", "ud2", "16"),
		cm("10:20", "object-deleted", "b", "ub", "17"),
		cm("10:30", "object-added", "c", "uc", "18"),
		// the agent is down from 10:40 and misses c's deletion; its restart
		// snapshots at 10:50
		cm("10:50", "object-snapshot", "a", "ua", "13"),
		cm("10:50", "object-snapshot", "d", "ud2", "16"),
		cm("10:50", "snapshot", "", "", ""),
		// a snapshot cut off before its snapshot row: not a base
		cm("11:00", "object-snapshot", "a", "ua", "13"),
		cm("11:05", "object-modified", "d", "ud2", "20"),
		// a resource whose preferred version changes: the new version's
		// snapshot is the base
		row("10:00", "object-snapshot", "example.com/v1", "widgets", "app", "old", "uo", "30"),
		row("10:00", "snapshot", "example.com/v1", "widgets", "", "", "", ""),
		row("10:30", "object-snapshot", "example.com/v2", "widgets", "app", "new", "un", "31"),
		row("10:30", "snapshot", "example.com/v2", "widgets", "", "", "", ""),
		// a cluster-scoped object
		row("10:00", "object-snapshot", "v1", "namespaces", "", "app", "uns", "2"),
		row("10:00", "snapshot", "v1", "namespaces", "", "", "", ""),
	}
	dir := t.TempDir()
	rawFile := filepath.Join(dir, "raw.jsonl.zst")
	f, err := os.Create(rawFile)
	if err != nil {
		t.Fatal(err)
	}
	zw, _ := zstd.NewWriter(f)
	enc := json.NewEncoder(zw)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	zw.Close()
	f.Close()

	for _, c := range []struct {
		at   string
		want []string
	}{
		{"09:59", nil},
		{"10:05", []string{"configmaps app/a 10", "configmaps app/b 11", "namespaces /app 2", "widgets app/old 30"}},
		{"10:13", []string{"configmaps app/a 13", "configmaps app/b 11", "configmaps app/d 14", "namespaces /app 2", "widgets app/old 30"}},
		{"10:15", []string{"configmaps app/a 13", "configmaps app/b 11", "namespaces /app 2", "widgets app/old 30"}},
		{"10:17", []string{"configmaps app/a 13", "configmaps app/b 11", "configmaps app/d 16", "namespaces /app 2", "widgets app/old 30"}},
		{"10:35", []string{"configmaps app/a 13", "configmaps app/c 18", "configmaps app/d 16", "namespaces /app 2", "widgets app/new 31"}},
		{"10:55", []string{"configmaps app/a 13", "configmaps app/d 16", "namespaces /app 2", "widgets app/new 31"}},
		{"11:06", []string{"configmaps app/a 13", "configmaps app/d 20", "namespaces /app 2", "widgets app/new 31"}},
	} {
		got := queryAt(t, duckdb, rawFile, c.at)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("at %s: got %q, want %q", c.at, got, c.want)
		}
	}
}

func queryAt(t *testing.T, duckdb, rawFile, at string) []string {
	t.Helper()
	root, _ := filepath.Abs(repoRoot)
	cmd := exec.Command(duckdb, "-bail", "-noheader", "-list",
		"-c", "SET TimeZone = 'UTC'",
		"-c", "SET VARIABLE raw_files = '"+rawFile+"'",
		"-c", ".read "+root+"/compactor/sql/raw.sql",
		"-c", ".read "+root+"/compactor/sql/tables.sql",
		"-c", ".read "+root+"/local/apiserver_at.sql",
		"-c", "SELECT resource || ' ' || coalesce(namespace, '') || '/' || name || ' ' || resource_version FROM apiserver_at(TIMESTAMP '2026-10-07 "+at+":00')")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("duckdb: %v\n%s", err, b)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func TestKey(t *testing.T) {
	for _, c := range []struct {
		gr     schema.GroupResource
		custom bool
		ns     string
		want   string
	}{
		{schema.GroupResource{Resource: "pods"}, false, "app", "/registry/pods/app/x"},
		{schema.GroupResource{Resource: "nodes"}, false, "", "/registry/minions/x"},
		{schema.GroupResource{Resource: "services"}, false, "app", "/registry/services/specs/app/x"},
		{schema.GroupResource{Group: "apps", Resource: "deployments"}, false, "app", "/registry/deployments/app/x"},
		{schema.GroupResource{Group: "networking.k8s.io", Resource: "ingresses"}, false, "app", "/registry/ingress/app/x"},
		{schema.GroupResource{Group: "example.com", Resource: "widgets"}, true, "app", "/registry/example.com/widgets/app/x"},
		{schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, false, "", "/registry/apiextensions.k8s.io/customresourcedefinitions/x"},
	} {
		if got := Key(c.gr, c.custom, c.ns, "x"); got != c.want {
			t.Errorf("%s: %s, want %s", c.gr, got, c.want)
		}
	}
}

func TestPrepare(t *testing.T) {
	crd := `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"widgets.example.com","resourceVersion":"5"},"spec":{"group":"example.com","names":{"plural":"widgets"}}}`
	p, err := Prepare([]Line{
		{APIVersion: "example.com/v1", Resource: "widgets", Namespace: "app", Name: "w1", Object: `{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w1","namespace":"app","resourceVersion":"9","uid":"u1"}}`},
		{APIVersion: "apiextensions.k8s.io/v1", Resource: "customresourcedefinitions", Name: "widgets.example.com", Object: crd},
		{APIVersion: "apiregistration.k8s.io/v1", Resource: "apiservices", Name: "v1beta1.metrics.k8s.io", Object: `{"spec":{"service":{"name":"metrics-server","namespace":"kube-system"}}}`},
		{APIVersion: "apiregistration.k8s.io/v1", Resource: "apiservices", Name: "v1.apps", Object: `{"spec":{"group":"apps"}}`},
		{APIVersion: "networking.k8s.io/v1", Resource: "servicecidrs", Name: "kubernetes", Object: `{"spec":{"cidrs":["10.43.0.0/16"]}}`},
		{APIVersion: "v1", Resource: "services", Namespace: "app", Name: "lb", Object: `{"spec":{"type":"LoadBalancer","ports":[{"port":80,"nodePort":30001},{"port":443,"nodePort":30017}],"healthCheckNodePort":30010}}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, kv := range p.KVs {
		keys = append(keys, kv.Key)
		if strings.Contains(string(kv.Value), "resourceVersion") {
			t.Errorf("%s keeps its resourceVersion", kv.Key)
		}
	}
	want := []string{
		"/registry/apiextensions.k8s.io/customresourcedefinitions/widgets.example.com",
		"/registry/apiregistration.k8s.io/apiservices/v1.apps",
		"/registry/example.com/widgets/app/w1",
		"/registry/ranges/servicenodeports",
		"/registry/servicecidrs/kubernetes",
		"/registry/services/specs/app/lb",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keys %q", keys)
	}
	// the NodePorts 30001, 30010 and 30017 are allocated: bits 1, 10, 17
	for _, kv := range p.KVs {
		if kv.Key != "/registry/ranges/servicenodeports" {
			continue
		}
		var ra struct {
			Kind, Range string
			Data        []byte
		}
		if err := json.Unmarshal(kv.Value, &ra); err != nil {
			t.Fatal(err)
		}
		if ra.Kind != "RangeAllocation" || ra.Range != "30000-32767" || !reflect.DeepEqual(ra.Data, []byte{0x02, 0x04, 0x02}) {
			t.Errorf("node ports %s", kv.Value)
		}
	}
	if p.Skipped["aggregated apiservice"] != 1 || !reflect.DeepEqual(p.ServiceCIDRs, []string{"10.43.0.0/16"}) {
		t.Errorf("skipped %v cidrs %v", p.Skipped, p.ServiceCIDRs)
	}
}
