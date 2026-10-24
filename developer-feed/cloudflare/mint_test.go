package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testAccount = "0123456789abcdef0123456789abcdef"
	testKeyID   = "parentkeyid0123"
	testSecret  = "parent-secret-0123456789"
	testBucket  = "dev-feed"
)

var testNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func testMint(t *testing.T, ep string, actions []string, prefix string) credential {
	t.Helper()
	c, err := mint(mintRequest{
		Endpoint: ep, Account: testAccount, Bucket: testBucket,
		ParentKeyID: testKeyID, ParentSecret: testSecret,
		Actions: actions, Prefixes: []string{prefix}, TTL: 30 * 24 * time.Hour, Now: testNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// decodeToken is what R2 does with a session token, as its docs describe
// the format: base64 of "jwt/" + a JWT whose signature is the parent's
// secret's HMAC. It returns the JWT and its claims.
func decodeToken(t *testing.T, token string) (string, map[string]any) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("session token: %v", err)
	}
	jwt, ok := strings.CutPrefix(string(raw), "jwt/")
	if !ok {
		t.Fatalf("session token %q: no jwt/ prefix", raw)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q: %d parts", jwt, len(parts))
	}
	enc := base64.RawURLEncoding
	m := hmac.New(sha256.New, []byte(testSecret))
	m.Write([]byte(parts[0] + "." + parts[1]))
	if got := enc.EncodeToString(m.Sum(nil)); got != parts[2] {
		t.Fatalf("jwt signature %s, want %s", parts[2], got)
	}
	var header map[string]string
	h, _ := enc.DecodeString(parts[0])
	if err := json.Unmarshal(h, &header); err != nil || header["alg"] != "HS256" || header["typ"] != "JWT" {
		t.Fatalf("jwt header %s", h)
	}
	var claims map[string]any
	p, _ := enc.DecodeString(parts[1])
	if err := json.Unmarshal(p, &claims); err != nil {
		t.Fatal(err)
	}
	return jwt, claims
}

// The claims and derivations of R2's local-signing example
// (developers.cloudflare.com/r2/examples/authenticate-r2-temp-credentials).
func TestMintAsR2Example(t *testing.T) {
	c := testMint(t, "https://"+testAccount+".eu.r2.cloudflarestorage.com", []string{"PutObject"}, "dev1/staffan/")
	if c.Version != 1 || c.AccessKeyId != testKeyID {
		t.Errorf("version %d, key id %q", c.Version, c.AccessKeyId)
	}
	jwt, claims := decodeToken(t, c.SessionToken)
	sum := sha256.Sum256([]byte(jwt))
	if c.SecretAccessKey != hashHex([]byte(jwt)) || !hexOK(c.SecretAccessKey) || len(sum) != 32 {
		t.Errorf("secret %q is not the SHA-256 hex of the jwt", c.SecretAccessKey)
	}
	want := map[string]any{
		"bucket": testBucket, "sub": testAccount, "iss": testKeyID,
		"aud": testAccount + ".eu.r2.cloudflarestorage.com",
		"iat": float64(testNow.Unix()), "exp": float64(testNow.Add(30 * 24 * time.Hour).Unix()),
	}
	for k, v := range want {
		if claims[k] != v {
			t.Errorf("claim %s = %v, want %v", k, claims[k], v)
		}
	}
	if a, _ := json.Marshal(claims["actions"]); string(a) != `["PutObject"]` {
		t.Errorf("actions %s", a)
	}
	if p, _ := json.Marshal(claims["paths"]); string(p) != `{"objectPaths":[],"prefixPaths":["dev1/staffan/"]}` {
		t.Errorf("paths %s", p)
	}
	if _, ok := claims["scope"]; ok {
		t.Error("scope and actions are exclusive")
	}
	if c.Expiration != "2026-11-23T12:00:00Z" {
		t.Errorf("expiration %s", c.Expiration)
	}
	out, _ := json.Marshal(c)
	if bytes.Contains(out, []byte(testSecret)) {
		t.Error("the parent's secret is in the minted credential")
	}
}

func TestMintRefuses(t *testing.T) {
	ok := mintRequest{Endpoint: "https://x.r2.cloudflarestorage.com", Account: "a", Bucket: "b",
		ParentKeyID: "k", ParentSecret: "s", Actions: []string{"PutObject"}, TTL: time.Hour, Now: testNow}
	for name, mod := range map[string]func(*mintRequest){
		"no secret":   func(r *mintRequest) { r.ParentSecret = "" },
		"no actions":  func(r *mintRequest) { r.Actions = nil },
		"zero ttl":    func(r *mintRequest) { r.TTL = 0 },
		"no endpoint": func(r *mintRequest) { r.Endpoint = "" },
	} {
		r := ok
		mod(&r)
		if _, err := mint(r); err == nil {
			t.Errorf("%s: minted", name)
		}
	}
}

func TestFeedPrefixAndFlags(t *testing.T) {
	if p, err := feedPrefix("dev1", "staffan"); err != nil || p != "dev1/staffan/" {
		t.Errorf("%q %v", p, err)
	}
	for _, bad := range [][2]string{{"dev1", ""}, {"", "x"}, {"dev1", "a/b"}, {"dev1", ".x"}, {"dev1", "_x"}} {
		if _, err := feedPrefix(bad[0], bad[1]); err == nil {
			t.Errorf("feedPrefix%q accepted", bad)
		}
	}
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := parseTTL(in); err != nil || got != want {
			t.Errorf("parseTTL(%q) = %s, %v", in, got, err)
		}
	}
	for _, bad := range []string{"0d", "-1d", "xd", "soon"} {
		if _, err := parseTTL(bad); err == nil {
			t.Errorf("parseTTL(%q) accepted", bad)
		}
	}
	if ep, _ := endpoint("acct", "eu"); ep != "https://acct.eu.r2.cloudflarestorage.com" {
		t.Error(ep)
	}
	if _, err := endpoint("acct", "us"); err == nil {
		t.Error("jurisdiction us accepted")
	}
}

// cmdMint with the secret piped in, as a password manager's CLI would.
func TestCmdMintFromStdin(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("OBSERVABILITY_HOST_CONFIG", cfgDir)
	var out, errOut bytes.Buffer
	err := cmdMint([]string{"--account", testAccount, "--bucket", testBucket, "--cluster", "staffan",
		"--parent-key-id", testKeyID, "--ttl", "7d"}, strings.NewReader(testSecret+"\n"), &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	var c credential
	if err := json.Unmarshal(out.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	_, claims := decodeToken(t, c.SessionToken)
	if p, _ := json.Marshal(claims["paths"]); !strings.Contains(string(p), `"dev1/staffan/"`) {
		t.Errorf("paths %s", p)
	}
	if !strings.Contains(errOut.String(), "PutObject on dev-feed/dev1/staffan/") {
		t.Errorf("stderr %q", errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), testSecret) {
		t.Error("the parent's secret was printed")
	}
	cfgFile, _ := os.ReadFile(filepath.Join(cfgDir, "env"))
	if !strings.Contains(string(cfgFile), "DEV_FEED_PARENT_KEY_ID="+testKeyID+"\n") || strings.Contains(string(cfgFile), testSecret) {
		t.Errorf("config after mint: %q", cfgFile)
	}
	if !strings.Contains(errOut.String(), "config: DEV_FEED_PARENT_KEY_ID="+testKeyID+" (added)") {
		t.Errorf("no feedback on the config: %q", errOut.String())
	}
}

// The defaults come from the config, after the environment; mint needs no
// flags once setup-bucket.sh and a first mint have written it.
func TestCmdMintFromConfig(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("OBSERVABILITY_HOST_CONFIG", cfgDir)
	for _, k := range []string{"DEV_FEED_ACCOUNT", "DEV_FEED_BUCKET", "DEV_FEED_JURISDICTION", "CLUSTER_NAME", "KEY_ROOT", "DEV_FEED_PARENT_KEY_ID"} {
		t.Setenv(k, "")
	}
	cfg := filepath.Join(cfgDir, "env")
	var fb bytes.Buffer
	for k, v := range map[string]string{"DEV_FEED_ACCOUNT": testAccount, "DEV_FEED_BUCKET": testBucket,
		"DEV_FEED_JURISDICTION": "eu", "CLUSTER_NAME": "staffan", "DEV_FEED_PARENT_KEY_ID": testKeyID} {
		if err := setConfig(cfg, k, v, &fb); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if err := cmdMint(nil, strings.NewReader(testSecret+"\n"), &out, &errOut); err != nil {
		t.Fatalf("mint with no flags: %v (%s)", err, errOut.String())
	}
	var c credential
	if err := json.Unmarshal(out.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	_, claims := decodeToken(t, c.SessionToken)
	if claims["aud"] != testAccount+".eu.r2.cloudflarestorage.com" || claims["iss"] != testKeyID {
		t.Errorf("claims from the config: %v", claims)
	}
	if !strings.Contains(errOut.String(), "DEV_FEED_PARENT_KEY_ID="+testKeyID+" (unchanged)") {
		t.Errorf("feedback: %q", errOut.String())
	}
	t.Setenv("CLUSTER_NAME", "other")
	out.Reset()
	errOut.Reset()
	if err := cmdMint(nil, strings.NewReader(testSecret+"\n"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "dev1/other/") {
		t.Errorf("the environment did not win over the config: %q", errOut.String())
	}
	// a read mint keeps its parent's key id under its own key
	out.Reset()
	errOut.Reset()
	if err := cmdMint([]string{"--role", "read", "--prefix", "dev1/", "--parent-key-id", "readparent"},
		strings.NewReader(testSecret+"\n"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if m := readConfig(cfg); m["DEV_FEED_READ_PARENT_KEY_ID"] != "readparent" || m["DEV_FEED_PARENT_KEY_ID"] != testKeyID {
		t.Errorf("parent key ids after a read mint: %v", m)
	}
}

// setConfig as lib/config.sh's cfg_set: the same file and the same words.
func TestSetConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1", "env")
	var fb bytes.Buffer
	for _, kv := range [][2]string{{"S3_BUCKET", "a"}, {"S3_BUCKET", "a"}, {"S3_BUCKET", "b"}, {"KEY_ROOT", "dev1"}} {
		if err := setConfig(path, kv[0], kv[1], &fb); err != nil {
			t.Fatal(err)
		}
	}
	want := "config: S3_BUCKET=a (added)\nconfig: S3_BUCKET=a (unchanged)\nconfig: S3_BUCKET=b (was a)\nconfig: KEY_ROOT=dev1 (added)\n"
	if fb.String() != want {
		t.Errorf("feedback %q, want %q", fb.String(), want)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(b), "S3_BUCKET=b\nKEY_ROOT=dev1\n") || !strings.HasPrefix(string(b), "# ") {
		t.Errorf("file %q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	if err := setConfig(path, "X", "has space", &fb); err == nil {
		t.Error("a value with a space was written")
	}
	if m := readConfig(path); m["S3_BUCKET"] != "b" || m["KEY_ROOT"] != "dev1" {
		t.Errorf("read back %v", m)
	}
}

// fakeR2 authorizes as R2's temporary credentials are documented to: the
// session token's JWT signed by the parent, the request signed with the
// SHA-256 of that JWT, and the call one of the JWT's actions on a key (or a
// list prefix) under one of its prefixPaths. allowAll stands in for a
// credential that is broader than it should be.
func fakeR2(t *testing.T, allowAll bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deny := func(code string) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("<Error><Code>" + code + "</Code></Error>"))
		}
		token := r.Header.Get("X-Amz-Security-Token")
		if token == "" {
			deny("AccessDenied")
			return
		}
		jwt, claims := decodeToken(t, token)
		if !signatureOK(r, hashHex([]byte(jwt))) {
			deny("SignatureDoesNotMatch")
			return
		}
		if allowAll {
			w.WriteHeader(http.StatusOK)
			return
		}
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"+testBucket), "/")
		action := ""
		switch {
		case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
			action = "CopyObject"
		case r.Method == http.MethodPut:
			action = "PutObject"
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			action = "CreateMultipartUpload"
		case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
			action, key = "ListObjectsV2", r.URL.Query().Get("prefix")
		case r.Method == http.MethodGet:
			action = "GetObject"
		case r.Method == http.MethodHead:
			action = "HeadObject"
		case r.Method == http.MethodDelete:
			action = "DeleteObject"
		}
		var actions []string
		for _, a := range claims["actions"].([]any) {
			actions = append(actions, a.(string))
		}
		under := false
		for _, p := range claims["paths"].(map[string]any)["prefixPaths"].([]any) {
			under = under || strings.HasPrefix(key, p.(string))
		}
		if !slices.Contains(actions, action) || !under {
			deny("AccessDenied")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// signatureOK recomputes the request's SigV4 signature from its signed
// headers with sigv4.go's own sigV4.
func signatureOK(r *http.Request, secret string) bool {
	auth := r.Header.Get("Authorization")
	_, signed, ok := strings.Cut(auth, "SignedHeaders=")
	if !ok {
		return false
	}
	signed, sig, _ := strings.Cut(signed, ", Signature=")
	var canon strings.Builder
	for _, h := range strings.Split(signed, ";") {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		canon.WriteString(h + ":" + v + "\n")
	}
	canonical := strings.Join([]string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, canon.String(), signed,
		r.Header.Get("X-Amz-Content-Sha256")}, "\n")
	return sig == sigV4(secret, region, r.Header.Get("X-Amz-Date"), canonical)
}

func testClient(srv *httptest.Server, c credential) *s3 {
	return &s3{endpoint: srv.URL, bucket: testBucket, cred: c, client: srv.Client(), now: func() time.Time { return testNow }}
}

func TestVerifyWrite(t *testing.T) {
	srv := fakeR2(t, false)
	defer srv.Close()
	c := testMint(t, srv.URL, roles["write"], "dev1/staffan/")
	var out bytes.Buffer
	if failed := report(&out, runChecks(verifyChecks(testClient(srv, c), "write", "dev1/staffan/"))); failed != 0 {
		t.Errorf("%d failed:\n%s", failed, out.String())
	}
	if n := strings.Count(out.String(), "ok  "); n != 10 {
		t.Errorf("%d checks ok, want 10:\n%s", n, out.String())
	}
}

func TestVerifyRead(t *testing.T) {
	srv := fakeR2(t, false)
	defer srv.Close()
	c := testMint(t, srv.URL, roles["read"], "dev1/staffan/")
	var out bytes.Buffer
	if failed := report(&out, runChecks(verifyChecks(testClient(srv, c), "read", "dev1/staffan/"))); failed != 0 {
		t.Errorf("%d failed:\n%s", failed, out.String())
	}
}

// A credential that can do more than PutObject fails every denial check.
func TestVerifyCatchesABroadCredential(t *testing.T) {
	srv := fakeR2(t, true)
	defer srv.Close()
	c := testMint(t, srv.URL, roles["write"], "dev1/staffan/")
	var out bytes.Buffer
	failed := report(&out, runChecks(verifyChecks(testClient(srv, c), "write", "dev1/staffan/")))
	// the session token check is still refused: the fake denies a request without one
	if failed != 8 {
		t.Errorf("%d failed, want 8:\n%s", failed, out.String())
	}
}

// A secret other than the derived one is refused before any action is
// considered, so verify sees the signature fail.
func TestVerifyWrongSecret(t *testing.T) {
	srv := fakeR2(t, false)
	defer srv.Close()
	c := testMint(t, srv.URL, roles["write"], "dev1/staffan/")
	c.SecretAccessKey = hashHex([]byte("not the jwt"))
	outcome := runChecks(verifyChecks(testClient(srv, c), "write", "dev1/staffan/"))[0]
	if outcome.ok() || outcome.result.code != "SignatureDoesNotMatch" {
		t.Errorf("put with a wrong secret: %s", outcome.result)
	}
}
