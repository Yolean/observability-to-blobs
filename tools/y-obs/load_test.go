package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// A state with large objects, as CRDs with big schemas are, loads: no
// transaction goes past etcd's request limit.
func TestLoadLargeObjects(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (setup-envtest use -p path)")
	}
	etcd := &envtest.Etcd{
		Path:         filepath.Join(assets, "etcd"),
		DataDir:      t.TempDir(),
		StartTimeout: time.Minute,
	}
	if err := etcd.Start(); err != nil {
		t.Fatal(err)
	}
	defer etcd.Stop()

	// 200 small objects and four of 1.2 MiB: 128 objects of these in one
	// transaction would be far past etcd's 1.5 MiB
	var kvs []KV
	for i := range 200 {
		kvs = append(kvs, KV{Key: fmt.Sprintf("/registry/configmaps/app/small-%03d", i), Value: []byte(`{}`)})
		if i%50 == 0 {
			kvs = append(kvs, KV{Key: fmt.Sprintf("/registry/configmaps/app/large-%03d", i), Value: []byte(strings.Repeat("x", 1200<<10))})
		}
	}
	ctx := context.Background()
	if err := load(ctx, etcd.URL.String(), kvs); err != nil {
		t.Fatal(err)
	}
	c, err := clientv3.New(clientv3.Config{Endpoints: []string{etcd.URL.String()}, DialTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := c.Get(ctx, "/registry/", clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		t.Fatal(err)
	}
	if r.Count != int64(len(kvs)) {
		t.Errorf("%d keys, want %d", r.Count, len(kvs))
	}
}
