package main

import (
	"strings"
	"testing"
	"time"
)

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func bucketEnv(extra map[string]string) map[string]string {
	kv := map[string]string{"CLUSTER_NAME": "itest", "S3_BUCKET": "b", "S3_ENDPOINT": "http://versitygw:7070/",
		"AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s"}
	for k, v := range extra {
		kv[k] = v
	}
	return kv
}

func TestConfigDefaults(t *testing.T) {
	c, err := parseConfig(envOf(bucketEnv(map[string]string{"DUCKDB_MEMORY_LIMIT": "4294967296"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.endpoint != "versitygw:7070" || !c.tls || c.region != "us-east-1" {
		t.Fatalf("endpoint %q tls %v region %q", c.endpoint, c.tls, c.region)
	}
	if c.rolling != 2*time.Minute || c.grace != 10*time.Minute || c.past != time.Hour || c.rawDelete {
		t.Fatalf("cadence %s %s %s delete %v", c.rolling, c.grace, c.past, c.rawDelete)
	}
	if c.memoryLimit != 3<<30 {
		t.Fatalf("memory limit %d, want 75%% of 4 GiB", c.memoryLimit)
	}
	if c.threads != 4 || c.httpRetries != 5 {
		t.Fatalf("threads %d retries %d", c.threads, c.httpRetries)
	}
}

func TestConfigRefuses(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"GRACE": "31m"}, "GRACE 31m0s: at most 30m0s"},
		{map[string]string{"ROLLING_INTERVAL": "6m"}, "ROLLING_INTERVAL 6m0s: at most 5m0s"},
		{map[string]string{"PAST_INTERVAL": "61m"}, "PAST_INTERVAL 1h1m0s: at most 1h0m0s"},
		{map[string]string{"RAW_DELETE": "yes"}, "RAW_DELETE"},
		{map[string]string{"CLUSTER_NAME": "a/b"}, "CLUSTER_NAME"},
		{map[string]string{"S3_BUCKET": ""}, "S3_BUCKET is required"},
		{map[string]string{"ROLLING_INTERVAL": "0s"}, "ROLLING_INTERVAL"},
		{map[string]string{"S3_TLS": "maybe"}, "S3_TLS"},
		{map[string]string{"PARQUET_KEY_ID": "k1"}, "PARQUET_KEYS_DIR is required"},
		{map[string]string{"PARQUET_KEY_ID": "K/1", "PARQUET_KEYS_DIR": "/keys"}, "PARQUET_KEY_ID"},
	} {
		_, err := parseConfig(envOf(bucketEnv(tc.env)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err %v, want %q", tc.env, err, tc.want)
		}
	}
	c, err := parseConfig(envOf(map[string]string{"CLUSTER_NAME": "itest", "BLOBS_DIR": "/tmp/x", "GRACE": "30m",
		"ROLLING_INTERVAL": "5m", "PAST_INTERVAL": "1h", "RAW_DELETE": "after-final"}))
	if err != nil || !c.rawDelete || c.grace != maxGrace || c.rolling != maxRolling || c.past != maxPast {
		t.Fatalf("a local directory needs no bucket: %v", err)
	}
}

// The secret never appears in an error from the engine's init.
func TestInitRedactsTheSecret(t *testing.T) {
	db, err := open("", initStmt{label: "CREATE SECRET", redact: []string{"hunter2"}, sql: "SELECT 'hunter2' FROM nowhere"})
	if err == nil {
		err = db.Ping()
		db.Close()
	}
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "CREATE SECRET") {
		t.Fatalf("err %v", err)
	}
}
