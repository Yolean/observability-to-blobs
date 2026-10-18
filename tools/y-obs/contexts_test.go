package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// awsEnv isolates the AWS SDK from this machine's AWS setup: empty shared
// files unless config is given, no instance metadata, no AWS_* from the
// caller. It returns the directory the files are in.
func awsEnv(t *testing.T, config string) string {
	t.Helper()
	forgetStores()
	dir := t.TempDir()
	for _, k := range []string{"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "S3_BUCKET", "Y_OBS_CONTEXT", "CLUSTER_NAME"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("Y_OBS_CONFIG", filepath.Join(dir, "contexts.yaml"))
	return dir
}

func writeContexts(t *testing.T, dir, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "contexts.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// grantScript is a credential_process command: it counts its runs in
// <dir>/runs and prints a key expiring ttl from now, or with a file
// <dir>/refuse, refuses as a grant service would.
func grantScript(t *testing.T, dir string, ttl time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, "grant.sh")
	expires := time.Now().Add(ttl).UTC().Format(time.RFC3339)
	script := "#!/bin/sh\n" +
		"echo run >> '" + dir + "/runs'\n" +
		"if [ -e '" + dir + "/refuse' ]; then echo 'grant refused: not on call' >&2; exit 3; fi\n" +
		"echo '{\"Version\": 1, \"AccessKeyId\": \"AKGRANT\", \"SecretAccessKey\": \"the-secret\", \"SessionToken\": \"tok\", \"Expiration\": \"" + expires + "\"}'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func runs(t *testing.T, dir string) int {
	b, _ := os.ReadFile(filepath.Join(dir, "runs"))
	return strings.Count(string(b), "run")
}

// Static keys from the environment and the S3 endpoint from
// AWS_ENDPOINT_URL_S3: no expiry, and TLS as the URL's scheme says.
func TestStoreFromEnvironment(t *testing.T) {
	awsEnv(t, "")
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	t.Setenv("AWS_REGION", "eu-north-1")
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://127.0.0.1:30070")
	st, cluster, err := storeFor(context.Background(), "", "b")
	if err != nil {
		t.Fatal(err)
	}
	if st.bucket != "b" || st.endpoint != "127.0.0.1:30070" || st.tls || st.region != "eu-north-1" || st.keyID != "k" || !st.expires.IsZero() || cluster != "" {
		t.Fatalf("%+v %q", st, cluster)
	}
}

// A context's profile with credential_process: the grant's key, its
// expiry, and the profile's endpoint and region; each resolution runs the
// process again, as a renewal does.
func TestStoreFromCredentialProcess(t *testing.T) {
	dir := awsEnv(t, "")
	grants := t.TempDir()
	script := grantScript(t, grants, 2*time.Minute)
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("[profile obs]\ncredential_process = "+script+
		"\nendpoint_url = https://objects.example.net\nregion = auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeContexts(t, dir, "current: prod\ncontexts:\n  prod:\n    bucket: node-data\n    cluster: prod-eu\n    profile: obs\n")
	st, cluster, err := storeFor(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.bucket != "node-data" || cluster != "prod-eu" || st.keyID != "AKGRANT" || st.token != "tok" || st.endpoint != "objects.example.net" || !st.tls || st.region != "auto" {
		t.Fatalf("%+v %q", st, cluster)
	}
	if left := time.Until(st.expires); left < 90*time.Second || left > 2*time.Minute || st.margin > time.Minute || st.margin < 40*time.Second {
		t.Fatalf("expires in %s, margin %s: want about 2m and 1m", left, st.margin)
	}
	// within the process the credential is kept until its renewal margin
	if _, _, err := storeFor(context.Background(), "", ""); err != nil || runs(t, grants) != 1 {
		t.Fatalf("a second resolution in the margin's time: %v, %d runs", err, runs(t, grants))
	}
	forgetStores()
	if _, _, err := storeFor(context.Background(), "", ""); err != nil || runs(t, grants) != 2 {
		t.Fatalf("a resolution after the cache: %v, %d runs", err, runs(t, grants))
	}
	// a refusal is an auth failure with the process's own message
	forgetStores()
	os.WriteFile(filepath.Join(grants, "refuse"), nil, 0o600)
	_, _, err = storeFor(context.Background(), "", "")
	if exitOf(err) != exitAuth || !strings.Contains(err.Error(), "profile obs") {
		t.Fatalf("refused: %v (exit %d)", err, exitOf(err))
	}
	if strings.Contains(err.Error(), "the-secret") {
		t.Fatal("the secret in an error")
	}
}

// The S3 endpoint: AWS_ENDPOINT_URL_S3 over the profile's services
// section, over AWS_ENDPOINT_URL and the profile's own endpoint_url; none
// is AWS's for the region.
func TestS3Endpoint(t *testing.T) {
	dir := awsEnv(t, "")
	// a named profile's credentials are its own, not the environment's
	keys := "aws_access_key_id = k\naws_secret_access_key = s\n"
	cfg := "[profile svc]\n" + keys + "services = objects\nendpoint_url = https://global.example.net\n[services objects]\ns3 =\n  endpoint_url = https://s3only.example.net\n" +
		"[profile plain]\n" + keys + "region = eu-west-1\n"
	os.WriteFile(filepath.Join(dir, "config"), []byte(cfg), 0o600)
	ctx := context.Background()
	for _, c := range []struct {
		profile, env, envS3, want string
	}{
		{"svc", "", "", "s3only.example.net"},
		{"svc", "", "http://env-s3.test:9000", "env-s3.test:9000"},
		{"plain", "https://env.example.net", "", "env.example.net"},
		{"plain", "", "", "s3.eu-west-1.amazonaws.com"},
	} {
		t.Setenv("AWS_ENDPOINT_URL", c.env)
		t.Setenv("AWS_ENDPOINT_URL_S3", c.envS3)
		st, err := awsStore(ctx, c.profile)
		if err != nil || st.endpoint != c.want {
			t.Errorf("%+v: %q %v", c, st.endpoint, err)
		}
	}
}

// The bucket: the flag, else S3_BUCKET, else the context's; none is a
// usage error.
func TestBucketPrecedence(t *testing.T) {
	dir := awsEnv(t, "")
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	ctx := context.Background()
	if _, _, err := storeFor(ctx, "", ""); exitOf(err) != exitUsage {
		t.Fatalf("no bucket: %v", err)
	}
	writeContexts(t, dir, "contexts:\n  only:\n    bucket: from-context\n")
	for _, c := range []struct{ flag, env, want string }{{"", "", "from-context"}, {"", "from-env", "from-env"}, {"from-flag", "from-env", "from-flag"}} {
		t.Setenv("S3_BUCKET", c.env)
		if st, _, err := storeFor(ctx, "", c.flag); err != nil || st.bucket != c.want {
			t.Errorf("%+v: %q %v", c, st.bucket, err)
		}
	}
	if _, _, err := storeFor(ctx, "missing", ""); exitOf(err) != exitUsage {
		t.Errorf("an unknown context: %v", err)
	}
}

func TestClusterFor(t *testing.T) {
	list := func(names ...string) func(context.Context) ([]string, error) {
		return func(context.Context) ([]string, error) { return names, nil }
	}
	for _, c := range []struct {
		flag, contextCluster string
		list                 func(context.Context) ([]string, error)
		want                 string
		code                 int
	}{
		{"x", "y", list("z"), "x", 0},
		{"", "y", list("z"), "y", 0},
		{"", "", list("only"), "only", 0},
		{"", "", list("a", "b"), "", exitUsage},
		{"", "", list(), "", exitUsage},
		{"", "", nil, "", exitUsage},
	} {
		got, err := clusterFor(context.Background(), c.flag, c.contextCluster, c.list)
		if got != c.want || exitOf(err) != c.code {
			t.Errorf("%+v: %q %v", c, got, err)
		}
	}
}

func TestContextName(t *testing.T) {
	cfg := &configFile{Current: "a", Contexts: map[string]contextConfig{"a": {}, "b": {}}}
	t.Setenv("Y_OBS_CONTEXT", "")
	if n := contextName("b", cfg); n != "b" {
		t.Errorf("flag: %s", n)
	}
	if n := contextName("", cfg); n != "a" {
		t.Errorf("current: %s", n)
	}
	t.Setenv("Y_OBS_CONTEXT", "b")
	if n := contextName("", cfg); n != "b" {
		t.Errorf("Y_OBS_CONTEXT: %s", n)
	}
	if n := contextName("", &configFile{Contexts: map[string]contextConfig{"only": {}}}); n != "b" {
		t.Errorf("Y_OBS_CONTEXT over the only one: %s", n)
	}
}

// A context holds no secrets: a field for one is refused.
func TestContextRefusesCredentials(t *testing.T) {
	dir := awsEnv(t, "")
	writeContexts(t, dir, "contexts:\n  a:\n    bucket: b\n    credentials:\n      url: https://mint\n")
	if _, _, err := loadConfig(); exitOf(err) != exitUsage {
		t.Fatalf("%v", err)
	}
}

// What is explicit wins over what the file picks: AWS_PROFILE over the
// current context's profile, and another bucket drops its cluster; a
// context named by --context keeps its profile.
func TestExplicitOverCurrent(t *testing.T) {
	dir := awsEnv(t, "")
	keys := func(id string) string { return "aws_access_key_id = " + id + "\naws_secret_access_key = s\n" }
	os.WriteFile(filepath.Join(dir, "config"), []byte("[profile ctx]\n"+keys("CTX")+"[profile env]\n"+keys("ENV")), 0o600)
	writeContexts(t, dir, "current: prod\ncontexts:\n  prod:\n    bucket: node-data\n    cluster: prod-eu\n    profile: ctx\n")
	ctx := context.Background()
	t.Setenv("AWS_PROFILE", "env")
	st, cluster, err := storeFor(ctx, "", "")
	if err != nil || st.keyID != "ENV" || cluster != "prod-eu" || !strings.Contains(st.source, "env") {
		t.Fatalf("AWS_PROFILE over the current context: %+v %q %v", st, cluster, err)
	}
	if st, _, err = storeFor(ctx, "prod", ""); err != nil || st.keyID != "CTX" {
		t.Fatalf("--context keeps its profile: %+v %v", st, err)
	}
	if _, cluster, err = storeFor(ctx, "", "other-bucket"); err != nil || cluster != "" {
		t.Fatalf("another bucket keeps the context's cluster %q: %v", cluster, err)
	}
}

// A refusing credential_process names the profile; a context with a
// credentials field says where credentials belong.
func TestMessages(t *testing.T) {
	dir := awsEnv(t, "")
	grants := t.TempDir()
	os.WriteFile(filepath.Join(grants, "refuse"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "config"), []byte("[profile obs]\ncredential_process = "+grantScript(t, grants, time.Minute)+"\n"), 0o600)
	t.Setenv("AWS_PROFILE", "obs")
	if _, err := awsStore(context.Background(), ""); exitOf(err) != exitAuth || !strings.Contains(err.Error(), "the credential_process of AWS profile obs failed") {
		t.Errorf("refused: %v", err)
	}
	writeContexts(t, dir, "contexts:\n  a:\n    bucket: b\n    credentials:\n      url: https://mint\n")
	if _, _, err := loadConfig(); exitOf(err) != exitUsage || !strings.Contains(err.Error(), "are the AWS profile's") {
		t.Errorf("credentials field: %v", err)
	}
}

func forgetStores() {
	stores.Lock()
	stores.byProfile = nil
	stores.Unlock()
}

// The environment's keys are named as such when the store refuses them.
func TestSourceOfEnvironmentKeys(t *testing.T) {
	awsEnv(t, "")
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	st, err := awsStore(context.Background(), "")
	if err != nil || st.source != "the AWS_* environment variables" {
		t.Fatalf("%q %v", st.source, err)
	}
}

// A cluster the context names needs no credential: with none to be had,
// resolveCluster still finds it.
func TestResolveClusterWithoutCredential(t *testing.T) {
	dir := awsEnv(t, "")
	writeContexts(t, dir, "contexts:\n  prod:\n    bucket: node-data\n    cluster: prod-eu\n    profile: none-such\n")
	if c, err := resolveCluster(context.Background(), &sessionFlags{}); err != nil || c != "prod-eu" {
		t.Fatalf("%q %v", c, err)
	}
}

// An endpoint with a path is refused, not cut to its host.
func TestEndpointWithAPath(t *testing.T) {
	awsEnv(t, "")
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	t.Setenv("AWS_ENDPOINT_URL_S3", "https://objects.example.net/s3")
	if _, err := awsStore(context.Background(), ""); exitOf(err) != exitUsage || !strings.Contains(err.Error(), "has a path") {
		t.Fatalf("%v", err)
	}
	t.Setenv("AWS_ENDPOINT_URL_S3", "https://objects.example.net/")
	if st, err := awsStore(context.Background(), ""); err != nil || st.endpoint != "objects.example.net" {
		t.Fatalf("a bare slash: %q %v", st.endpoint, err)
	}
}

func TestS3ErrorText(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><RequestId>x</RequestId></Error>`)
	if got := s3ErrorText(body); got != "AccessDenied: Access Denied." {
		t.Errorf("%q", got)
	}
	if got := s3ErrorText([]byte(" not xml ")); got != "not xml" {
		t.Errorf("%q", got)
	}
}

// status with nothing configured is no failure; with an endpoint it
// cannot use, it is that error's exit code, and AWS_PROFILE over the
// file's context shows in its row.
func TestStatusExitCodes(t *testing.T) {
	dir := awsEnv(t, "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var o, e strings.Builder
	if err := runStatus(context.Background(), nil, &o, &e, time.Now()); err != nil {
		t.Fatalf("nothing configured: %v %s", err, e.String())
	}
	os.WriteFile(filepath.Join(dir, "config"), []byte("[profile env]\naws_access_key_id = k\naws_secret_access_key = s\nendpoint_url = https://objects.example.net/s3\n"), 0o600)
	writeContexts(t, dir, "contexts:\n  prod:\n    bucket: node-data\n    profile: ctx\n")
	t.Setenv("AWS_PROFILE", "env")
	o.Reset()
	e.Reset()
	err := runStatus(context.Background(), nil, &o, &e, time.Now())
	if exitOf(err) != exitUsage || !strings.Contains(e.String(), "has a path") || !strings.Contains(o.String(), "AWS_PROFILE env") {
		t.Fatalf("%v\n%s\n%s", err, o.String(), e.String())
	}
}

// A store that refuses the listing: status still shows its other
// sections, says why in a note, and exits 3.
func TestStatusStoreRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied.</Message></Error>`))
	}))
	defer srv.Close()
	awsEnv(t, "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	t.Setenv("AWS_ENDPOINT_URL_S3", srv.URL)
	t.Setenv("S3_BUCKET", "b")
	var o, e strings.Builder
	err := runStatus(context.Background(), nil, &o, &e, time.Now())
	if exitOf(err) != exitAuth || !strings.Contains(o.String(), "## prometheus") || !strings.Contains(e.String(), "AccessDenied: Access Denied.") ||
		!strings.Contains(e.String(), "in bucket b;") || strings.Contains(e.String(), "<Error>") {
		t.Fatalf("%v\n%s\n%s", err, o.String(), e.String())
	}
}
