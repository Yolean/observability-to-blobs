package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// credential is the credential_process JSON (dev-feed-r2 mint), or the
// environment's keys.
type credential struct {
	AccessKeyId     string
	SecretAccessKey string
	SessionToken    string
	Expiration      string
}

// credentials is the agent's own: the environment's AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY if set, else what the credential_process of the
// config's aws-config prints (dev-feed-credentials, from the login
// Keychain). It writes as the agent does: PutObject under the feed's root,
// nothing else.
func credentials(configDir string) (credential, error) {
	if id := os.Getenv("AWS_ACCESS_KEY_ID"); id != "" {
		return credential{AccessKeyId: id, SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN")}, nil
	}
	path := os.Getenv("AWS_CONFIG_FILE")
	if path == "" {
		path = filepath.Join(configDir, "aws-config")
	}
	f, err := os.Open(path)
	if err != nil {
		return credential{}, fmt.Errorf("no credential: no AWS_ACCESS_KEY_ID, and %v", err)
	}
	defer f.Close()
	command := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && strings.TrimSpace(k) == "credential_process" {
			command = strings.TrimSpace(v)
		}
	}
	if command == "" {
		return credential{}, fmt.Errorf("no credential_process in %s", path)
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return credential{}, fmt.Errorf("credential_process %s: %v", command, err)
	}
	var c credential
	if err := json.Unmarshal(out, &c); err != nil || c.AccessKeyId == "" {
		return credential{}, fmt.Errorf("credential_process %s printed no credential", command)
	}
	if exp, err := time.Parse(time.RFC3339, c.Expiration); err == nil && time.Until(exp) < 10*time.Minute {
		return credential{}, fmt.Errorf("the credential expires at %s: store a new one (developer-feed/README_HUMAN.md, Renew)", c.Expiration)
	}
	return c, nil
}

// bucket puts objects, as out_s3 does: PutObject, path style, Content-MD5.
type bucket struct {
	endpoint, name, region string
	cred                   credential
	client                 *http.Client
}

func newBucket(endpoint, name, region string, cred credential) *bucket {
	return &bucket{endpoint: strings.TrimSuffix(endpoint, "/"), name: name, region: region, cred: cred,
		client: &http.Client{Timeout: 5 * time.Minute}}
}

// put tries three times; another status than 2xx or 5xx is final.
func (c *bucket) put(key string, body []byte) error {
	var err error
	for attempt, wait := 0, time.Second; attempt < 3; attempt, wait = attempt+1, wait*4 {
		if attempt > 0 {
			time.Sleep(wait)
		}
		var retry bool
		if retry, err = c.putOnce(key, body); err == nil || !retry {
			return err
		}
	}
	return err
}

func (c *bucket) putOnce(key string, body []byte) (retry bool, err error) {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return false, err
	}
	u.Path = "/" + c.name + "/" + key
	u.RawPath = s3Escape(u.Path, false)
	req, err := http.NewRequest(http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	sum := md5.Sum(body)
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	if c.cred.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.cred.SessionToken)
	}
	signV4(req, hashHex(body), c.region, c.cred.AccessKeyId, c.cred.SecretAccessKey, time.Now())
	resp, err := c.client.Do(req)
	if err != nil {
		return true, fmt.Errorf("PUT %s: %w", key, err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 == 2 {
		return false, nil
	}
	err = fmt.Errorf("PUT %s: %s: %s", key, resp.Status, strings.TrimSpace(string(msg)))
	return resp.StatusCode/100 == 5, err
}
