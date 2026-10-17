package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatus(t *testing.T) {
	dir := t.TempDir()
	put := func(rel string, size int) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o644)
	}
	put("raw1/c/2026-10-11/08/n1/0000-a.jsonl.zst", 100)
	put("raw1/c/2026-10-11/09/n1/0000-b.jsonl.zst", 200)
	put("raw1/c/2026-10-11/09/n2/0010-c.jsonl.zst", 300)
	put("raw1/c/2026-10-11/09/_k8sagent/0020-d.jsonl.zst", 50)
	put("hourly/c/logs/2026-10-11/08.parquet", 1000)
	put("hourly/c/_folded/2026-10-11/08.csv", 10)
	put("daily/c/logs/2026-10-10/namespace=qa/data_0.parquet", 5000)
	put("raw1/access-check/probe", 1)
	t.Setenv("Y_OBS_CONFIG", filepath.Join(t.TempDir(), "none.yaml"))
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://objects.test")
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "the-secret")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var o, e bytes.Buffer
	if err := runStatus(context.Background(), []string{"--blobs-dir", dir, "--format", "json"}, &o, &e, time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err, e.String())
	}
	if strings.Contains(o.String()+e.String(), "the-secret") {
		t.Fatal("the secret printed")
	}
	var got struct {
		Contexts []contextRow
		Data     []tierRow
	}
	if err := json.Unmarshal(o.Bytes(), &got); err != nil {
		t.Fatal(err, o.String())
	}
	if len(got.Contexts) != 1 || got.Contexts[0].Context != "(environment)" || !got.Contexts[0].Current {
		t.Errorf("contexts: %+v", got.Contexts)
	}
	if len(got.Data) != 3 {
		t.Fatalf("data: %+v", got.Data)
	}
	raw, hourly, daily := got.Data[0], got.Data[1], got.Data[2]
	if raw.Tier != "raw" || raw.First != "2026-10-11T08" || raw.Last != "2026-10-11T09" || raw.Objects != 4 || raw.Bytes != 650 || raw.Nodes != "2 (n1,n2) + cluster agent" {
		t.Errorf("raw: %+v", raw)
	}
	if hourly.First != "2026-10-11" || hourly.Objects != 2 || hourly.Bytes != 1010 {
		t.Errorf("hourly: %+v", hourly)
	}
	if daily.First != "2026-10-10" || daily.Last != "2026-10-10" || daily.Objects != 1 || daily.Size != "5.0 kB" {
		t.Errorf("daily: %+v", daily)
	}
	if !strings.Contains(e.String(), "1 clusters in bucket") {
		t.Errorf("summary: %s", e.String())
	}
}
