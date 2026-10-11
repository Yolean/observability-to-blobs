package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Line is one object of the state to serve: a row of local/apiserver_at.sql
// written as JSON lines (DuckDB's COPY ... (FORMAT json)).
type Line struct {
	APIVersion string `json:"api_version"`
	Resource   string `json:"resource"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	// the object as JSON text, as the apiserver table has it
	Object string `json:"apijson"`
}

// ReadLines reads JSON lines; objects can be large (CRDs with big schemas).
func ReadLines(r io.Reader) ([]Line, error) {
	var out []Line
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	n := 0
	for sc.Scan() {
		n++
		b := sc.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		var l Line
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// KV is one etcd key and the value the API server will read there.
type KV struct {
	Key   string
	Value []byte
}

// Prepared is the state as etcd entries, and what was left out and why.
type Prepared struct {
	KVs []KV
	// Skipped counts objects not loaded, by reason
	Skipped map[string]int
	// ServiceCIDRs is the cluster's service ranges, from its default
	// ServiceCIDR object, for the API server's flag; empty when the state
	// has none
	ServiceCIDRs []string
}

// Prepare turns lines into etcd entries under the API server's key layout.
// The objects keep their uid, creationTimestamp, generation and status;
// resourceVersion goes, since the server sets it from the etcd revision.
// APIServices served by a Service are left out: their backends are not
// here, and every discovery would warn that the group is unavailable.
func Prepare(lines []Line) (Prepared, error) {
	p := Prepared{Skipped: map[string]int{}}
	crds := map[schema.GroupResource]bool{}
	type parsed struct {
		l   Line
		gvr schema.GroupVersionResource
		obj map[string]interface{}
	}
	all := make([]parsed, 0, len(lines))
	for i, l := range lines {
		gv, err := schema.ParseGroupVersion(l.APIVersion)
		if err != nil {
			return p, fmt.Errorf("line %d: api_version %q: %w", i+1, l.APIVersion, err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(l.Object), &obj); err != nil {
			return p, fmt.Errorf("line %d: %s %s/%s: object: %w", i+1, l.Resource, l.Namespace, l.Name, err)
		}
		gvr := gv.WithResource(l.Resource)
		if gvr.GroupResource() == crdResource {
			if g, plural := crdNames(obj); g != "" && plural != "" {
				crds[schema.GroupResource{Group: g, Resource: plural}] = true
			}
		}
		all = append(all, parsed{l, gvr, obj})
	}
	nodePorts := new(big.Int)
	for _, x := range all {
		gr := x.gvr.GroupResource()
		if gr == serviceResource {
			for _, port := range servicePorts(x.obj) {
				if port >= nodePortBase && port < nodePortBase+nodePortSize {
					nodePorts.SetBit(nodePorts, int(port-nodePortBase), 1)
				}
			}
		}
		if gr == apiServiceResource && hasService(x.obj) {
			p.Skipped["aggregated apiservice"]++
			continue
		}
		if gr == serviceCIDRResource && x.l.Name == "kubernetes" {
			p.ServiceCIDRs = stringSlice(x.obj, "spec", "cidrs")
		}
		if md, ok := x.obj["metadata"].(map[string]interface{}); ok {
			delete(md, "resourceVersion")
			delete(md, "managedFields")
		}
		b, err := json.Marshal(x.obj)
		if err != nil {
			return p, err
		}
		p.KVs = append(p.KVs, KV{Key: Key(gr, crds[gr], x.l.Namespace, x.l.Name), Value: b})
	}
	b, err := json.Marshal(map[string]interface{}{
		"kind": "RangeAllocation", "apiVersion": "v1",
		"metadata": map[string]interface{}{},
		"range":    NodePortRange,
		"data":     nodePorts.Bytes(),
	})
	if err != nil {
		return p, err
	}
	p.KVs = append(p.KVs, KV{Key: nodePortsKey, Value: b})
	sort.Slice(p.KVs, func(i, j int) bool { return p.KVs[i].Key < p.KVs[j].Key })
	return p, nil
}

// The API server's NodePort allocation, a bitmap of the ports its Services
// hold (offset from the range's base, as big.Int bytes). Without it, the
// server's repair loop finds every NodePort unallocated, allocates it, and
// records a PortNotAllocated Warning Event that the cluster never had.
// The range is the API server's default, which apiserver-at keeps; a port
// outside it gets that Event anyway.
const (
	NodePortRange              = "30000-32767"
	nodePortBase, nodePortSize = 30000, 2768
	nodePortsKey               = etcdPrefix + "/ranges/servicenodeports"
)

// servicePorts is the NodePorts a Service holds: its ports' and its
// health check's
func servicePorts(obj map[string]interface{}) []int64 {
	spec, _ := obj["spec"].(map[string]interface{})
	var out []int64
	ports, _ := spec["ports"].([]interface{})
	for _, p := range ports {
		m, _ := p.(map[string]interface{})
		if n, ok := m["nodePort"].(float64); ok && n > 0 {
			out = append(out, int64(n))
		}
	}
	if n, ok := spec["healthCheckNodePort"].(float64); ok && n > 0 {
		out = append(out, int64(n))
	}
	return out
}

var (
	serviceResource     = schema.GroupResource{Resource: "services"}
	crdResource         = schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}
	apiServiceResource  = schema.GroupResource{Group: "apiregistration.k8s.io", Resource: "apiservices"}
	serviceCIDRResource = schema.GroupResource{Group: "networking.k8s.io", Resource: "servicecidrs"}
)

func crdNames(obj map[string]interface{}) (group, plural string) {
	spec, _ := obj["spec"].(map[string]interface{})
	names, _ := spec["names"].(map[string]interface{})
	group, _ = spec["group"].(string)
	plural, _ = names["plural"].(string)
	return group, plural
}

func hasService(obj map[string]interface{}) bool {
	spec, _ := obj["spec"].(map[string]interface{})
	svc, ok := spec["service"].(map[string]interface{})
	return ok && svc != nil
}

func stringSlice(obj map[string]interface{}, path ...string) []string {
	var cur interface{} = obj
	for _, p := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[p]
	}
	items, _ := cur.([]interface{})
	var out []string
	for _, i := range items {
		if s, ok := i.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
