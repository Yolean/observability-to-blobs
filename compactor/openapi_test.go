package main

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

// compactor/openapi.json is what `blobs-compactor openapi` prints: a change
// to the API's types that is not committed there fails here.
func TestOpenAPICommitted(t *testing.T) {
	got, err := openAPISpec()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("compactor/openapi.json differs from the generated spec: run `go run . openapi > openapi.json` in compactor/")
	}
}

// The logs operations are the reference's: testdata/reference-logs-openapi.json
// holds its two logs operations and the error schemas they reference, as
// its generator prints them. Paths, operation ids, summaries, tags,
// parameters (names, places, types, defaults, enums, descriptions) and
// responses (the response headers and their schemas, the body, the problem
// JSON error) must be equal, so that a client generated from the
// reference's spec works against this server. The operations' prose
// descriptions may differ: the reference describes its live source.
func TestOpenAPIMatchesTheReference(t *testing.T) {
	type spec struct {
		Paths      map[string]map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	read := func(b []byte) spec {
		var s spec
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	ref, err := os.ReadFile("testdata/reference-logs-openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	gen, err := openAPISpec()
	if err != nil {
		t.Fatal(err)
	}
	want, got := read(ref), read(gen)
	if !reflect.DeepEqual(keys(got.Paths), keys(want.Paths)) {
		t.Fatalf("paths %v, want %v", keys(got.Paths), keys(want.Paths))
	}
	for path, ops := range want.Paths {
		if !reflect.DeepEqual(keys(got.Paths[path]), keys(ops)) {
			t.Fatalf("%s: methods %v, want %v", path, keys(got.Paths[path]), keys(ops))
		}
		for method, op := range ops {
			g := got.Paths[path][method]
			for _, field := range []string{"operationId", "summary", "tags", "parameters", "responses"} {
				if !reflect.DeepEqual(g[field], op[field]) {
					gj, _ := json.MarshalIndent(g[field], "", "  ")
					wj, _ := json.MarshalIndent(op[field], "", "  ")
					t.Fatalf("%s %s: %s\n%s\nwant\n%s", method, path, field, gj, wj)
				}
			}
		}
	}
	for name, schema := range want.Components.Schemas {
		if !reflect.DeepEqual(got.Components.Schemas[name], schema) {
			t.Fatalf("components.schemas.%s differs from the reference's", name)
		}
	}
	if len(got.Components.Schemas) != len(want.Components.Schemas) {
		t.Fatalf("schemas %v, want %v", keys(got.Components.Schemas), keys(want.Components.Schemas))
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
