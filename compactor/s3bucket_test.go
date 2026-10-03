package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestS3Bucket runs a fold with deletion and a rollup against a real
// S3-compatible bucket, through httpfs and this client. It needs:
//
//	TEST_S3_ENDPOINT=127.0.0.1:7070 TEST_S3_BUCKET=... AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
//	HTTPFS_EXTENSION=<httpfs.duckdb_extension for this platform and the engine's version>
//
// for example versitygw in docker; the itest covers the same path in a cluster.
func TestS3Bucket(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" || os.Getenv("HTTPFS_EXTENSION") == "" {
		t.Skip("TEST_S3_ENDPOINT and HTTPFS_EXTENSION not set")
	}
	cluster := "s3test-" + time.Now().UTC().Format("150405")
	cfg, err := parseConfig(envOf(map[string]string{"CLUSTER_NAME": cluster, "S3_BUCKET": os.Getenv("TEST_S3_BUCKET"),
		"S3_ENDPOINT": endpoint, "S3_TLS": "off", "AWS_ACCESS_KEY_ID": os.Getenv("AWS_ACCESS_KEY_ID"),
		"AWS_SECRET_ACCESS_KEY": os.Getenv("AWS_SECRET_ACCESS_KEY"), "RAW_DELETE": "after-final"}))
	if err != nil {
		t.Fatal(err)
	}
	c, err := newCompactor(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	b := c.raw.(*s3Bucket)
	w := newWorld(t) // writes the fixtures; they are uploaded from its directory
	put := func(key string) {
		t.Helper()
		body, err := os.ReadFile(w.c.raw.URL(key))
		if err != nil {
			t.Fatal(err)
		}
		key = strings.Replace(key, "raw1/"+testCluster+"/", "raw1/"+cluster+"/", 1)
		if _, err := b.do(ctx, http.MethodPut, key, nil, body, http.StatusOK); err != nil {
			t.Fatal(err)
		}
	}
	put(w.putA())
	put(w.putB())
	put(w.putBRetry())
	r, err := c.fold(ctx, h10, nil, during)
	if err != nil || r.keys != 3 {
		t.Fatalf("fold: %+v %v", r, err)
	}
	put(w.putC())
	put(w.putE())
	if err := c.past(ctx, day.Add(30*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	left, err := b.List(ctx, "raw1/"+cluster+"/")
	if err != nil || len(left) != 0 {
		t.Fatalf("raw left after the final folds: %v %v", left, err)
	}
	var n int64
	if err := c.db.QueryRowContext(ctx, "SELECT count(*) FROM read_parquet('s3://"+cfg.bucket+"/daily/"+cluster+"/logs/2026-10-01/*/*.parquet')").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != h10Logs+5 {
		t.Fatalf("daily logs %d, want %d", n, h10Logs+5)
	}
	if ok, err := b.Stat(ctx, c.l.dailySidecar(day)); !ok || err != nil {
		t.Fatalf("daily sidecar: %v %v", ok, err)
	}
	if ok, err := b.Stat(ctx, "nothing/here"); ok || err != nil {
		t.Fatalf("stat of a missing key: %v %v", ok, err)
	}

	// A write that its deadline interrupts mid-upload: httpfs completes
	// the multipart upload of the half-written file, at the temporary key,
	// and the hour's object stays the previous one.
	hourly := func() (n int64, err error) {
		err = c.db.QueryRowContext(ctx, "SELECT count(*) FROM read_parquet("+sqlString(b.URL(c.l.hourly("logs", h10)))+")").Scan(&n)
		return
	}
	before, err := hourly()
	if err != nil || before != h10Logs {
		t.Fatalf("hourly logs before: %d %v", before, err)
	}
	interruptWrite(t, c, c.l.hourly("logs", h10), 3*time.Second)
	if n, err := hourly(); n != before || err != nil {
		t.Fatalf("hourly logs after the interrupted write: %d %v, want %d", n, err, before)
	}
	// what the write's own delete missed, the sweep deletes
	if err := c.sweep(ctx, time.Now().Add(time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	if left := tmpLeft(t, c); len(left) != 0 {
		t.Fatalf("temporary objects left: %v", left)
	}
}

// TestS3BucketListing pages List and Dirs past 1,000 entries and round-trips
// keys that need escaping, against the bucket TestS3Bucket names.
func TestS3BucketListing(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	b := &s3Bucket{bucket: os.Getenv("TEST_S3_BUCKET"), endpoint: endpoint, region: "us-east-1",
		keyID: os.Getenv("AWS_ACCESS_KEY_ID"), secret: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		http: &http.Client{Timeout: time.Minute}, now: time.Now}
	ctx := context.Background()
	root := "listing-" + time.Now().UTC().Format("150405") + "/"
	var keys, dirs []string
	for _, s := range []string{"a b", "a+b", "a=b", "a&b", "a~b", "å", "a%20b", "a'b", "a(b)", "a!b*", "a,b;c:d@e$"} {
		keys = append(keys, root+"s/"+s+".jsonl.zst")
	}
	for i := 0; i < 1203; i++ {
		d := fmt.Sprintf("%sd/%05d/", root, i)
		dirs = append(dirs, d)
		keys = append(keys, d+"x")
	}
	for _, k := range keys {
		if _, err := b.do(ctx, http.MethodPut, k, nil, []byte("x"), http.StatusOK); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}
	got, err := b.List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	if len(got) != len(keys) {
		t.Fatalf("listed %d keys, want %d", len(got), len(keys))
	}
	for i := range got {
		if got[i].Key != keys[i] || got[i].Modified.IsZero() {
			t.Fatalf("key %d: %+v, want %q", i, got[i], keys[i])
		}
		if ok, err := b.Stat(ctx, keys[i]); !ok || err != nil {
			t.Fatalf("stat %q: %v %v", keys[i], ok, err)
		}
	}
	gotDirs, err := b.Dirs(ctx, root+"d/")
	if err != nil || strings.Join(gotDirs, " ") != strings.Join(dirs, " ") {
		t.Fatalf("dirs: %d of %d, %v", len(gotDirs), len(dirs), err)
	}
	if err := b.Move(ctx, keys[0], root+"moved"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.Stat(ctx, keys[0]); ok {
		t.Fatal("moved key still there")
	}
	if err := b.Delete(ctx, append(keys[1:], root+"moved", root+"missing")); err != nil {
		t.Fatal(err)
	}
	if got, err := b.List(ctx, root); err != nil || len(got) != 0 {
		t.Fatalf("after the delete: %d keys, %v", len(got), err)
	}
}
