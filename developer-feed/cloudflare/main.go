// dev-feed-r2: credentials for a developer feed on Cloudflare R2.
//
//	dev-feed-r2 mint   [flags]   sign a temporary credential, print it as credential_process JSON
//	dev-feed-r2 verify [flags]   check a credential read from stdin against the bucket
//
// README.md has the setup and the reasons. Nothing here writes a secret to
// a file: the parent's secret is read from the terminal (or stdin), and the
// minted credential goes to stdout, which must not be a terminal.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "mint":
		err = cmdMint(os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
	case "verify":
		err = cmdVerify(os.Args[2:], os.Stdin, os.Stdout)
	case "-h", "-help", "--help", "help":
		usage(os.Stdout)
		return
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dev-feed-r2:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: dev-feed-r2 mint   [--parent-key-id ID] [--role write|read] [--ttl 30d] [flags]
       dev-feed-r2 verify [--role write|read] [flags] < credential.json
Run a subcommand with -h for its flags. Their defaults are DEV_FEED_ACCOUNT,
DEV_FEED_BUCKET, DEV_FEED_JURISDICTION, CLUSTER_NAME, KEY_ROOT and
DEV_FEED_PARENT_KEY_ID (DEV_FEED_READ_PARENT_KEY_ID for --role read), from the environment, else from the user-level host
agent's config ($OBSERVABILITY_HOST_CONFIG/env, default
~/.config/observability-host/v1/env), where setup-bucket.sh and install.sh
put them. mint keeps the parent's key id there, verify when it passed.
`)
}

// feedFlags are the flags both subcommands share: where the bucket is and
// which feed's prefix a credential is for.
type feedFlags struct {
	account, bucket, jurisdiction, endpoint string
	root, cluster, prefix, role             string
}

// register defines the shared flags, each defaulting to its variable in the
// environment, else in the config file (config.go).
func (f *feedFlags) register(fs *flag.FlagSet, cfg map[string]string) {
	fs.StringVar(&f.account, "account", setting(cfg, "DEV_FEED_ACCOUNT", ""), "Cloudflare account id ($DEV_FEED_ACCOUNT)")
	fs.StringVar(&f.bucket, "bucket", setting(cfg, "DEV_FEED_BUCKET", ""), "R2 bucket ($DEV_FEED_BUCKET)")
	fs.StringVar(&f.jurisdiction, "jurisdiction", setting(cfg, "DEV_FEED_JURISDICTION", ""), "the bucket's jurisdiction: default or eu ($DEV_FEED_JURISDICTION)")
	fs.StringVar(&f.endpoint, "endpoint", "", "the S3 endpoint, instead of the one account and jurisdiction give")
	fs.StringVar(&f.root, "root", setting(cfg, "KEY_ROOT", "dev1"), "the key root ($KEY_ROOT)")
	fs.StringVar(&f.cluster, "cluster", setting(cfg, "CLUSTER_NAME", ""), "the feed's cluster name, the key segment below the root ($CLUSTER_NAME); no default")
	fs.StringVar(&f.prefix, "prefix", "", "the key prefix, instead of {root}/{cluster}/")
	fs.StringVar(&f.role, "role", "write", "write (PutObject, the agent's) or read (Get, Head, List; the developer's)")
}

func (f *feedFlags) resolve() (endpointURL, prefix string, actions []string, err error) {
	actions, ok := roles[f.role]
	if !ok {
		return "", "", nil, fmt.Errorf("role %q: expected write or read", f.role)
	}
	if f.account == "" || f.bucket == "" {
		return "", "", nil, errors.New("--account and --bucket are required")
	}
	endpointURL = f.endpoint
	if endpointURL == "" {
		if endpointURL, err = endpoint(f.account, f.jurisdiction); err != nil {
			return "", "", nil, err
		}
	}
	prefix = f.prefix
	if prefix == "" {
		if prefix, err = feedPrefix(f.root, f.cluster); err != nil {
			return "", "", nil, err
		}
	}
	if err = checkPrefix(prefix); err != nil {
		return "", "", nil, err
	}
	return endpointURL, prefix, actions, nil
}

func cmdMint(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mint", flag.ContinueOnError)
	cfgPath := configFile()
	cfg := readConfig(cfgPath)
	var f feedFlags
	f.register(fs, cfg)
	parentKeyID := fs.String("parent-key-id", "", "the parent R2 API token's access key id; not secret, kept in the config, as DEV_FEED_PARENT_KEY_ID for the write role and DEV_FEED_READ_PARENT_KEY_ID for read")
	ttl := fs.String("ttl", "30d", "how long the credential is valid: a Go duration, or days as 30d")
	toTerminal := fs.Bool("to-terminal", false, "print the credential even when stdout is a terminal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ep, prefix, actions, err := f.resolve()
	if err != nil {
		return err
	}
	// each role has a parent of its own, so each keeps its key id apart
	parentKey := "DEV_FEED_PARENT_KEY_ID"
	if f.role == "read" {
		parentKey = "DEV_FEED_READ_PARENT_KEY_ID"
	}
	if *parentKeyID == "" {
		*parentKeyID = setting(cfg, parentKey, "")
	}
	if *parentKeyID == "" {
		return errors.New("--parent-key-id is required")
	}
	d, err := parseTTL(*ttl)
	if err != nil {
		return err
	}
	if isTerminal(stdout) && !*toTerminal {
		return errors.New("stdout is a terminal: pipe the credential to where it is kept (README.md), or pass --to-terminal")
	}
	secret, err := readSecret(stdin, stderr, "parent secret access key for "+*parentKeyID+": ")
	if err != nil {
		return err
	}
	c, err := mint(mintRequest{
		Endpoint: ep, Account: f.account, Bucket: f.bucket,
		ParentKeyID: *parentKeyID, ParentSecret: secret,
		Actions: actions, Prefixes: []string{prefix}, TTL: d, Now: time.Now(),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "minted: %s on %s/%s, expires %s\n", strings.Join(actions, ","), f.bucket, prefix, c.Expiration)
	if err := json.NewEncoder(stdout).Encode(c); err != nil {
		return err
	}
	// the parent's key id is not secret: kept, so that the next mint needs no flag
	return setConfig(cfgPath, parentKey, *parentKeyID, stderr)
}

func cmdVerify(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	cfgPath := configFile()
	cfg := readConfig(cfgPath)
	var f feedFlags
	f.register(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ep, prefix, _, err := f.resolve()
	if err != nil {
		return err
	}
	var c credential
	if err := json.NewDecoder(stdin).Decode(&c); err != nil {
		return fmt.Errorf("credential on stdin: %w", err)
	}
	if c.Version != 1 || c.AccessKeyId == "" || !hexOK(c.SecretAccessKey) {
		return errors.New("credential on stdin: not credential_process JSON of a minted credential")
	}
	left, err := expiresIn(c, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "credential %s, %s role, prefix %s, expires %s (in %s)\n",
		c.AccessKeyId, f.role, prefix, c.Expiration, left.Round(time.Minute))
	client := &s3{endpoint: ep, bucket: f.bucket, cred: c, client: &http.Client{Timeout: 30 * time.Second}, now: time.Now}
	if failed := report(stdout, runChecks(verifyChecks(client, f.role, prefix))); failed > 0 {
		return fmt.Errorf("%d of the checks failed", failed)
	}
	if f.role != "write" {
		return nil
	}
	return setConfig(cfgPath, "DEV_FEED_VERIFIED", time.Now().UTC().Format(time.RFC3339)+"/"+c.Expiration, stdout)
}

// readSecret reads one line: from the terminal with echo off when stdin is
// one, else from stdin (a password manager's CLI piped in).
func readSecret(stdin io.Reader, prompt io.Writer, label string) (string, error) {
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		fmt.Fprint(prompt, label)
		if err := stty(f, "-echo"); err != nil {
			return "", err
		}
		defer func() {
			_ = stty(f, "echo")
			fmt.Fprintln(prompt)
		}()
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", fmt.Errorf("reading the parent secret: %w", err)
	}
	s := strings.TrimSpace(line)
	if s == "" {
		return "", errors.New("empty parent secret")
	}
	return s, nil
}

// stty runs stty on the terminal f, the one dependency-free way to turn
// echo off on both macOS and Linux. Its output is discarded.
func stty(f *os.File, arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = f
	return cmd.Run()
}

// isTerminal asks stty, which succeeds only on a terminal: a character
// device such as /dev/null is not one.
func isTerminal(w any) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	return stty(f, "-g") == nil
}
