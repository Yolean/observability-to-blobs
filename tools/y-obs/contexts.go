package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// A context is what one credential can read: one bucket at one endpoint,
// and the mint that hands out its short-lived read credentials. The
// credential's scope may narrow the bucket to some clusters
// (README.md, "auth and contexts").
type contextConfig struct {
	Bucket      string `json:"bucket"`
	Endpoint    string `json:"endpoint"`
	Region      string `json:"region,omitempty"`
	TLS         *bool  `json:"tls,omitempty"`
	Credentials struct {
		// URL is the mint, POSTed with the token as a bearer
		URL string `json:"url"`
		// Token is a command whose stdout is the bearer token, run by sh
		Token string `json:"token,omitempty"`
	} `json:"credentials"`
}

type configFile struct {
	Current  string                   `json:"current,omitempty"`
	Contexts map[string]contextConfig `json:"contexts"`
}

// credential is what the mint returns, and what the cache keeps.
type credential struct {
	AccessKeyID     string    `json:"accessKeyId"`
	SecretAccessKey string    `json:"secretAccessKey"`
	SessionToken    string    `json:"sessionToken,omitempty"`
	Expiration      time.Time `json:"expiration"`
	Bucket          string    `json:"bucket,omitempty"`
	Endpoint        string    `json:"endpoint,omitempty"`
	Region          string    `json:"region,omitempty"`
	// Clusters is the scope, when the credential covers only some
	Clusters []string `json:"clusters,omitempty"`
	// Obtained is when y-obs got it, kept in the cache for margin
	Obtained time.Time `json:"obtained,omitempty"`
}

// margin is how long before expiry a credential is renewed: renewBefore,
// or half its lifetime when that is shorter, so that a short-lived one is
// still used for a while rather than renewed at once.
func (c *credential) margin() time.Duration {
	if c.Obtained.IsZero() {
		return renewBefore
	}
	return min(renewBefore, c.Expiration.Sub(c.Obtained)/2)
}

// renewBefore is how long before expiry a cached credential is renewed,
// so that a command started with it does not outlive it.
const renewBefore = 2 * time.Minute

func configPath() string {
	if p := os.Getenv("Y_OBS_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "y-obs", "contexts.yaml")
}

func loadConfig() (*configFile, string, error) {
	p := configPath()
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &configFile{Contexts: map[string]contextConfig{}}, p, nil
	}
	if err != nil {
		return nil, p, err
	}
	var c configFile
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, p, fail(exitUsage, "%s: %v", p, err)
	}
	if c.Contexts == nil {
		c.Contexts = map[string]contextConfig{}
	}
	return &c, p, nil
}

// contextName picks the context: the flag, else Y_OBS_CONTEXT, else the
// config's current one, unless the environment names a bucket (S3_BUCKET),
// which then wins as local/blobs.sh's static keys.
func contextName(flagValue string, c *configFile) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("Y_OBS_CONTEXT"); v != "" {
		return v
	}
	if os.Getenv("S3_BUCKET") != "" {
		return ""
	}
	if c.Current != "" {
		return c.Current
	}
	if len(c.Contexts) == 1 {
		for n := range c.Contexts {
			return n
		}
	}
	return ""
}

func cacheDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "y-obs")
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), fmt.Sprintf("y-obs-%d", os.Getuid()))
	}
	return filepath.Join(d, "y-obs")
}

func cachePath(name string) string { return filepath.Join(cacheDir(), name+".json") }

// cached returns the context's cached credential while it has more than
// renewBefore left.
func cached(name string, now time.Time) (*credential, bool) {
	b, err := os.ReadFile(cachePath(name))
	if err != nil {
		return nil, false
	}
	var c credential
	if json.Unmarshal(b, &c) != nil || now.Add(c.margin()).After(c.Expiration) {
		return nil, false
	}
	return &c, true
}

func storeCached(name string, c *credential) error {
	if err := os.MkdirAll(cacheDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(c)
	tmp := cachePath(name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cachePath(name))
}

// mint runs the token command and asks the mint for a credential.
func mint(ctx context.Context, name string, cc contextConfig) (*credential, error) {
	if cc.Credentials.URL == "" {
		return nil, fail(exitUsage, "context %s has no credentials.url", name)
	}
	token := ""
	if cc.Credentials.Token != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", cc.Credentials.Token)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fail(exitAuth, "context %s: the token command failed (%v): log in again, then run y-obs auth", name, err)
		}
		token = strings.TrimSpace(string(out))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cc.Credentials.URL, nil)
	if err != nil {
		return nil, fail(exitUsage, "context %s: credentials.url: %v", name, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, fail(exitAuth, "context %s: the mint did not answer: %v", name, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return nil, fail(exitAuth, "context %s: the mint answered HTTP %d: %s", name, res.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var c credential
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fail(exitAuth, "context %s: the mint's answer is not a credential: %v", name, err)
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.Expiration.IsZero() {
		return nil, fail(exitAuth, "context %s: the mint's credential lacks accessKeyId, secretAccessKey or expiration", name)
	}
	c.Obtained = time.Now().UTC()
	return &c, nil
}

// credentialFor returns a context's credential, from the cache or minted.
func credentialFor(ctx context.Context, name string, cc contextConfig, renew bool) (*credential, string, error) {
	if !renew {
		if c, ok := cached(name, time.Now()); ok {
			return c, "cached", nil
		}
	}
	c, err := mint(ctx, name, cc)
	if err != nil {
		return nil, "", err
	}
	if err := storeCached(name, c); err != nil {
		fmt.Fprintf(os.Stderr, "y-obs: the credential is not cached: %v\n", err)
	}
	return c, "minted", nil
}

// storeFor resolves where a session reads: a context's bucket with its
// credential, else the environment's static keys. It returns the scope's
// clusters, empty when the credential covers the whole bucket.
func storeFor(ctx context.Context, contextFlag string) (storeConfig, []string, error) {
	cfg, path, err := loadConfig()
	if err != nil {
		return storeConfig{}, nil, err
	}
	name := contextName(contextFlag, cfg)
	if name == "" {
		return storeFromEnv(), nil, nil
	}
	cc, ok := cfg.Contexts[name]
	if !ok {
		return storeConfig{}, nil, fail(exitUsage, "no context %q in %s; y-obs contexts lists them", name, path)
	}
	c, _, err := credentialFor(ctx, name, cc, false)
	if err != nil {
		return storeConfig{}, nil, err
	}
	st := storeConfig{bucket: cc.Bucket, endpoint: cc.Endpoint, region: cc.Region, tls: cc.TLS == nil || *cc.TLS,
		keyID: c.AccessKeyID, secret: c.SecretAccessKey, token: c.SessionToken}
	if c.Bucket != "" {
		st.bucket = c.Bucket
	}
	if c.Endpoint != "" {
		st.endpoint = c.Endpoint
	}
	if c.Region != "" {
		st.region = c.Region
	}
	if st.region == "" {
		st.region = "us-east-1"
	}
	st.endpoint = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(st.endpoint, "https://"), "http://"), "/")
	st.expires, st.margin = c.Expiration, c.margin()
	return st, c.Clusters, nil
}

const authUsage = `Usage: y-obs auth [--context NAME] [--renew] [-- COMMAND ARGS]

Get the context's read credential: from the cache while it has more than
2 minutes (or half its lifetime, when shorter) left, else from its mint. Prints the scope and expiry, never
the secret. With a command after --, runs it with the credential in its
environment (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN,
S3_ENDPOINT, S3_BUCKET, S3_REGION, S3_TLS), for tools y-obs does not cover.

Examples:
  y-obs auth
  y-obs auth --context prod --renew
  y-obs auth -- duckdb
`

func cmdAuth(ctx context.Context, args []string) error {
	var command []string
	for i, a := range args {
		if a == "--" {
			args, command = args[:i], args[i+1:]
			break
		}
	}
	fs := newFlagSet("auth", authUsage)
	contextFlag := fs.String("context", "", "the context; default Y_OBS_CONTEXT, else the config's current")
	renew := fs.Bool("renew", false, "mint a new credential even when the cached one is valid")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, path, err := loadConfig()
	if err != nil {
		return err
	}
	name := contextName(*contextFlag, cfg)
	if name == "" && os.Getenv("S3_BUCKET") != "" {
		// static keys from the environment: nothing to mint or renew
		st := storeFromEnv()
		key := "AWS_ACCESS_KEY_ID unset"
		if st.keyID != "" {
			key = "AWS_ACCESS_KEY_ID set"
		}
		fmt.Fprintf(os.Stderr, "y-obs: no context: static keys from the environment, bucket %s at %s, %s, no expiry; contexts (%s) are for short-lived credentials\n",
			st.bucket, st.endpoint, key, path)
		if len(command) == 0 {
			return nil
		}
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return &exitError{code: ee.ExitCode()}
			}
			return fail(exitUsage, "%s: %v", command[0], err)
		}
		return nil
	}
	if name == "" {
		return fail(exitUsage, "no context: pass --context, or add one to %s (y-obs contexts -h shows the format)", path)
	}
	cc, ok := cfg.Contexts[name]
	if !ok {
		return fail(exitUsage, "no context %q in %s; y-obs contexts lists them", name, path)
	}
	c, how, err := credentialFor(ctx, name, cc, *renew)
	if err != nil {
		return err
	}
	scope := "every cluster in the bucket"
	if len(c.Clusters) > 0 {
		scope = "clusters " + strings.Join(c.Clusters, ", ")
	}
	bucket := cc.Bucket
	if c.Bucket != "" {
		bucket = c.Bucket
	}
	fmt.Fprintf(os.Stderr, "y-obs: context %s, bucket %s, %s; %s, expires %s (in %s)\n", name, bucket, scope, how,
		c.Expiration.UTC().Format(time.RFC3339), time.Until(c.Expiration).Round(time.Second))
	if len(command) == 0 {
		return nil
	}
	st, _, err := storeFor(ctx, name)
	if err != nil {
		return err
	}
	tls := "off"
	if st.tls {
		tls = "on"
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "AWS_ACCESS_KEY_ID="+st.keyID, "AWS_SECRET_ACCESS_KEY="+st.secret, "AWS_SESSION_TOKEN="+st.token,
		"S3_ENDPOINT="+st.endpoint, "S3_BUCKET="+st.bucket, "S3_REGION="+st.region, "S3_TLS="+tls)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &exitError{code: ee.ExitCode()}
		}
		return fail(exitUsage, "%s: %v", command[0], err)
	}
	return nil
}

const contextsUsage = `Usage: y-obs contexts

The contexts of the config file (Y_OBS_CONFIG, default
~/.config/y-obs/contexts.yaml): their bucket and the clusters their cached
credential covers. A context is what one credential can read:

  current: prod
  contexts:
    prod:
      bucket: node-data
      endpoint: objects.example.net
      region: auto
      credentials:
        url: https://obs-creds.example.net/v1/read     # the mint, POSTed with the token
        token: y-remote-iap print https://obs-creds.example.net
`

func cmdContexts(ctx context.Context, args []string) error {
	fs := newFlagSet("contexts", contextsUsage)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, path, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Contexts) == 0 {
		return fail(exitNoRows, "no contexts in %s; y-obs contexts -h shows the format", path)
	}
	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	current := contextName("", cfg)
	fmt.Println("context\tcurrent\tbucket\tendpoint\tclusters\tcredential")
	for _, n := range names {
		cc := cfg.Contexts[n]
		clusters, cred := "", "none cached: y-obs auth --context "+n
		if c, ok := cached(n, time.Now()); ok {
			clusters = strings.Join(c.Clusters, ",")
			if clusters == "" {
				clusters = "*"
			}
			cred = "expires " + c.Expiration.UTC().Format(time.RFC3339)
		}
		mark := ""
		if n == current {
			mark = "*"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n", n, mark, cc.Bucket, cc.Endpoint, clusters, cred)
	}
	return nil
}

const mintStubUsage = `Usage: y-obs mint-stub [flags]

DEVELOPMENT ONLY. A credential mint that hands out the static keys it was
started with (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY) under an expiry
--ttl from now: the protocol of a real mint, none of its security. For
trying contexts and auth against a local bucket.

Example:
  AWS_ACCESS_KEY_ID=demoaccess AWS_SECRET_ACCESS_KEY=demosecret \
    y-obs mint-stub --listen 127.0.0.1:37171 --ttl 10m --clusters itest --token dev
`

func cmdMintStub(ctx context.Context, args []string) error {
	fs := newFlagSet("mint-stub", mintStubUsage)
	listen := fs.String("listen", "127.0.0.1:37171", "address")
	ttl := fs.Duration("ttl", 10*time.Minute, "credential lifetime")
	clusters := fs.String("clusters", "", "the scope, comma separated; empty for the whole bucket")
	token := fs.String("token", "", "the bearer token to require; empty requires none")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	keyID, secret := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if keyID == "" || secret == "" {
		return fail(exitUsage, "mint-stub hands out AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY: set them")
	}
	var scope []string
	if *clusters != "" {
		scope = strings.Split(*clusters, ",")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/read", func(w http.ResponseWriter, r *http.Request) {
		if *token != "" && r.Header.Get("Authorization") != "Bearer "+*token {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		c := credential{AccessKeyID: keyID, SecretAccessKey: secret, Expiration: time.Now().Add(*ttl).UTC().Truncate(time.Second), Clusters: scope}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(c)
	})
	srv := &http.Server{Addr: *listen, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	fmt.Fprintf(os.Stderr, "y-obs: mint-stub on http://%s/v1/read, static keys, ttl %s; development only\n", *listen, *ttl)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fail(exitUsage, "%v", err)
	}
	return nil
}
