package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"sigs.k8s.io/yaml"
)

// A context names where y-obs reads: a bucket, its cluster when there is
// one to default to, and the AWS profile to read it with. It holds no
// credentials: those, the endpoint and the region come from the AWS SDK's
// default chain for that profile (README.md, "S3 access").
type contextConfig struct {
	Bucket  string `json:"bucket"`
	Cluster string `json:"cluster,omitempty"`
	Profile string `json:"profile,omitempty"`
}

type configFile struct {
	Current  string                   `json:"current,omitempty"`
	Contexts map[string]contextConfig `json:"contexts"`
}

// renewBefore is how long before expiry a session's credential is
// renewed, or half its remaining lifetime when that is shorter.
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
		if strings.Contains(err.Error(), "unknown field") {
			return nil, p, fail(exitUsage, "%s: %v: a context names only bucket, cluster and profile; credentials, the endpoint and the region are the AWS profile's (y-obs contexts -h)", p, err)
		}
		return nil, p, fail(exitUsage, "%s: %v", p, err)
	}
	if c.Contexts == nil {
		c.Contexts = map[string]contextConfig{}
	}
	return &c, p, nil
}

// contextName picks the context: the flag, else Y_OBS_CONTEXT, else the
// config's current one, else its only one; "" for none.
func contextName(flagValue string, c *configFile) string {
	name, _ := contextChoice(flagValue, c)
	return name
}

// contextChoice is contextName, and whether the context was named
// explicitly, by the flag or Y_OBS_CONTEXT, rather than by the file.
func contextChoice(flagValue string, c *configFile) (string, bool) {
	if flagValue != "" {
		return flagValue, true
	}
	if v := os.Getenv("Y_OBS_CONTEXT"); v != "" {
		return v, true
	}
	return implicitContext(c), false
}

func implicitContext(c *configFile) string {
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

// storeFor resolves where a session reads: the bucket from bucketFlag,
// else S3_BUCKET, else the context; everything else from the AWS SDK's
// default chain, for the context's profile when it names one. It also
// returns the context's cluster, "" when it names none. What is explicit
// wins over the file: AWS_PROFILE over the profile of a context the file
// picks (its current or only one), and a bucket other than the context's
// drops the context's cluster.
func storeFor(ctx context.Context, contextFlag, bucketFlag string) (storeConfig, string, error) {
	cfg, path, err := loadConfig()
	if err != nil {
		return storeConfig{}, "", err
	}
	var cc contextConfig
	name, explicit := contextChoice(contextFlag, cfg)
	if name != "" {
		var ok bool
		if cc, ok = cfg.Contexts[name]; !ok {
			return storeConfig{}, "", fail(exitUsage, "no context %q in %s; y-obs contexts lists them", name, path)
		}
	}
	bucket := bucketFlag
	if bucket == "" {
		bucket = os.Getenv("S3_BUCKET")
	}
	if bucket == "" {
		bucket = cc.Bucket
	}
	if bucket == "" {
		return storeConfig{}, "", fail(exitUsage, "no bucket: pass --bucket, set S3_BUCKET, or name one in a context (y-obs contexts -h)")
	}
	cluster := cc.Cluster
	if bucket != cc.Bucket {
		cluster = ""
	}
	profile := cc.Profile
	if !explicit && os.Getenv("AWS_PROFILE") != "" {
		profile = ""
	}
	st, err := awsStore(ctx, profile)
	if err != nil {
		return storeConfig{}, "", err
	}
	st.bucket = bucket
	return st, cluster, nil
}

// awsStore is the AWS SDK's default chain, for profile when it is set,
// else AWS_PROFILE or the default profile: credentials from the
// environment, the shared files, credential_process, SSO, a container
// or instance role; the region; and the S3 endpoint, AWS_ENDPOINT_URL_S3,
// else the profile's services s3 endpoint_url, else AWS_ENDPOINT_URL or
// the profile's endpoint_url, else AWS's own for the region. Requests are
// path-style, which every S3 implementation takes.
func awsStore(ctx context.Context, profile string) (storeConfig, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return storeConfig{}, fail(exitAuth, "the AWS configuration%s: %v", ofProfile(profile), err)
	}
	source := "the AWS default chain"
	if p := os.Getenv("AWS_PROFILE"); profile == "" && p != "" {
		source = "AWS profile " + p
	} else if profile != "" {
		source = "AWS profile " + profile
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	var pe *processcreds.ProviderError
	if errors.As(err, &pe) {
		// the SDK passes the process's stderr through, and keeps none of it
		return storeConfig{}, fail(exitAuth, "the credential_process of %s failed (%v); its own message, if it printed one, is above", source, pe.Err)
	}
	if err != nil {
		return storeConfig{}, fail(exitAuth, "no AWS credentials from %s: %v", source, err)
	}
	st := storeConfig{region: cfg.Region, keyID: creds.AccessKeyID, secret: creds.SecretAccessKey, token: creds.SessionToken, tls: true, source: source}
	if st.region == "" {
		st.region = "us-east-1"
	}
	endpoint := s3Endpoint(ctx, cfg)
	if endpoint == "" {
		st.endpoint = "s3." + st.region + ".amazonaws.com"
	} else {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			return storeConfig{}, fail(exitUsage, "the S3 endpoint %q%s is not a URL such as https://objects.example.net", endpoint, ofProfile(profile))
		}
		st.endpoint, st.tls = u.Host, u.Scheme != "http"
	}
	if creds.CanExpire {
		st.expires = creds.Expires
		st.margin = min(renewBefore, time.Until(creds.Expires)/2)
	}
	return st, nil
}

func ofProfile(profile string) string {
	if profile == "" {
		return ""
	}
	return " of profile " + profile
}

// s3Endpoint is the SDK's resolution of a service's base endpoint, as its
// S3 client does it: the service-specific setting of each source in
// order (the environment first), else the global one.
func s3Endpoint(ctx context.Context, cfg aws.Config) string {
	type serviceEndpoint interface {
		GetServiceBaseEndpoint(ctx context.Context, sdkID string) (string, bool, error)
	}
	for _, src := range cfg.ConfigSources {
		if p, ok := src.(serviceEndpoint); ok {
			if v, found, err := p.GetServiceBaseEndpoint(ctx, "S3"); err == nil && found && v != "" {
				return v
			}
		}
	}
	return aws.ToString(cfg.BaseEndpoint)
}

const contextsUsage = `Usage: y-obs contexts

The contexts of the config file (Y_OBS_CONFIG, default
~/.config/y-obs/contexts.yaml). A context names a bucket, the cluster to
read when a command names none, and the AWS profile to read it with; the
credentials, the endpoint and the region are that profile's, as the AWS
SDK resolves them (credential_process included), never this file's:

  current: prod
  contexts:
    prod:
      bucket: node-data
      cluster: prod-eu
      profile: observability-read

with, in ~/.aws/config:

  [profile observability-read]
  credential_process = obs-grant --read prod-eu
  endpoint_url = https://objects.example.net
  region = auto
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
	fmt.Println("context\tcurrent\tbucket\tcluster\tprofile")
	for _, n := range names {
		cc := cfg.Contexts[n]
		mark := ""
		if n == current {
			mark = "*"
		}
		profile := cc.Profile
		if profile == "" {
			profile = "(default chain)"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", n, mark, cc.Bucket, cc.Cluster, profile)
	}
	return nil
}

// credentialNote says where a store's credential is from and until when,
// never the secret.
func credentialNote(profile string, st storeConfig) string {
	what := "the default chain"
	if profile != "" && profile != "(default chain)" {
		what = "profile " + profile
	}
	if st.expires.IsZero() {
		return what + ", no expiry"
	}
	return fmt.Sprintf("%s, expires %s (in %s)", what, st.expires.UTC().Format(time.RFC3339), time.Until(st.expires).Round(time.Second))
}

// resolveCluster is the cluster a session of sf would read, for a command
// that needs it before it opens one.
func resolveCluster(ctx context.Context, sf *sessionFlags) (string, error) {
	if sf.cluster != "" || sf.blobsDir != "" {
		return clusterFor(ctx, sf.cluster, "", nil)
	}
	st, contextCluster, err := storeFor(ctx, sf.context, sf.bucket)
	if err != nil {
		return "", err
	}
	s := &session{store: st, root: "s3://" + st.bucket}
	return clusterFor(ctx, "", contextCluster, s.clusters)
}
