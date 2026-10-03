package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
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
}
