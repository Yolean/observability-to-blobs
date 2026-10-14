package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testMint answers like a real mint: a credential living ttl, scoped to
// clusters, for the bearer token "t". It counts what it minted.
func testMint(t *testing.T, ttl time.Duration, clusters ...string) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer t" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		n.Add(1)
		json.NewEncoder(w).Encode(credential{AccessKeyID: "k", SecretAccessKey: "s", SessionToken: "st",
			Expiration: time.Now().Add(ttl), Clusters: clusters})
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func testConfig(t *testing.T, mintURL, token string) {
	t.Helper()
	dir := t.TempDir()
	cfg := "current: one\ncontexts:\n  one:\n    bucket: b\n    endpoint: http://objects.test/\n    credentials:\n      url: " + mintURL + "\n      token: printf " + token + "\n"
	if err := os.WriteFile(filepath.Join(dir, "contexts.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("Y_OBS_CONFIG", filepath.Join(dir, "contexts.yaml"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "run"))
	t.Setenv("S3_BUCKET", "")
	t.Setenv("Y_OBS_CONTEXT", "")
}

func TestContextCredential(t *testing.T) {
	srv, minted := testMint(t, 10*time.Minute, "prod")
	testConfig(t, srv.URL, "t")
	ctx := context.Background()
	st, scope, err := storeFor(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.bucket != "b" || st.endpoint != "objects.test" || !st.tls || st.keyID != "k" || st.token != "st" || st.region != "us-east-1" {
		t.Errorf("store: %+v", st)
	}
	if len(scope) != 1 || scope[0] != "prod" || st.margin != renewBefore {
		t.Errorf("scope %v, margin %s", scope, st.margin)
	}
	if _, _, err := storeFor(ctx, ""); err != nil || minted.Load() != 1 {
		t.Errorf("the second call minted again: %d %v", minted.Load(), err)
	}
	fi, err := os.Stat(cachePath("one"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("cache file: %v %v", fi, err)
	}
	if _, how, err := credentialFor(ctx, "one", contextConfig{Credentials: struct {
		URL   string `json:"url"`
		Token string `json:"token,omitempty"`
	}{srv.URL, "printf t"}}, true); err != nil || how != "minted" || minted.Load() != 2 {
		t.Errorf("--renew: %s %d %v", how, minted.Load(), err)
	}
	if _, _, err := storeFor(ctx, "two"); exitOf(err) != exitUsage {
		t.Errorf("unknown context: %v", err)
	}
}

func TestContextRefused(t *testing.T) {
	srv, _ := testMint(t, time.Minute)
	testConfig(t, srv.URL, "wrong")
	if _, _, err := storeFor(context.Background(), ""); exitOf(err) != exitAuth {
		t.Errorf("wrong token: %v", err)
	}
	testConfig(t, "http://127.0.0.1:1/v1/read", "t")
	if _, _, err := storeFor(context.Background(), ""); exitOf(err) != exitAuth {
		t.Errorf("no mint: %v", err)
	}
}

func TestMargin(t *testing.T) {
	now := time.Now()
	for life, want := range map[time.Duration]time.Duration{10 * time.Minute: renewBefore, 10 * time.Second: 5 * time.Second, 2*time.Minute + 2*time.Second: 61 * time.Second} {
		c := credential{Obtained: now, Expiration: now.Add(life)}
		if got := c.margin(); got != want {
			t.Errorf("lifetime %s: margin %s, want %s", life, got, want)
		}
	}
}

func TestClusterFor(t *testing.T) {
	for _, c := range []struct {
		flag  string
		scope []string
		want  string
		code  int
	}{
		{"", []string{"prod"}, "prod", 0},
		{"prod", []string{"prod", "qa"}, "prod", 0},
		{"x", nil, "x", 0},
		{"", []string{"prod", "qa"}, "", exitUsage},
		{"", nil, "", exitUsage},
		{"itest", []string{"prod"}, "", exitAuth},
	} {
		got, err := clusterFor(c.flag, c.scope)
		if got != c.want || exitOf(err) != c.code {
			t.Errorf("%q %v: %q %v", c.flag, c.scope, got, err)
		}
	}
}

func TestContextName(t *testing.T) {
	cfg := &configFile{Current: "a", Contexts: map[string]contextConfig{"a": {}, "b": {}}}
	t.Setenv("Y_OBS_CONTEXT", "")
	t.Setenv("S3_BUCKET", "")
	if n := contextName("b", cfg); n != "b" {
		t.Errorf("flag: %s", n)
	}
	if n := contextName("", cfg); n != "a" {
		t.Errorf("current: %s", n)
	}
	t.Setenv("S3_BUCKET", "env-bucket")
	if n := contextName("", cfg); n != "" {
		t.Errorf("S3_BUCKET wins over current: %s", n)
	}
	t.Setenv("Y_OBS_CONTEXT", "b")
	if n := contextName("", cfg); n != "b" {
		t.Errorf("Y_OBS_CONTEXT: %s", n)
	}
}

// auth with static keys from the environment describes them, never the
// secret, and exits 0.
func TestAuthStaticKeys(t *testing.T) {
	t.Setenv("Y_OBS_CONFIG", filepath.Join(t.TempDir(), "none.yaml"))
	t.Setenv("Y_OBS_CONTEXT", "")
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("S3_ENDPOINT", "objects.test")
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "the-secret")
	r, w, _ := os.Pipe()
	stderr := os.Stderr
	os.Stderr = w
	err := cmdAuth(context.Background(), nil)
	w.Close()
	os.Stderr = stderr
	out, _ := io.ReadAll(r)
	if err != nil || !strings.Contains(string(out), "static keys from the environment, bucket b at objects.test, AWS_ACCESS_KEY_ID set, no expiry") || strings.Contains(string(out), "the-secret") {
		t.Errorf("%q %v", out, err)
	}
}
