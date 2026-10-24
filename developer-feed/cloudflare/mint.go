package main

//go:generate cp ../../compactor/sigv4.go ../../compactor/sigv4_test.go .

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A role is what a minted credential may do: the S3 actions of R2's local
// signing (developers.cloudflare.com/r2/api/s3/temporary-credentials,
// "Actions"). write is the agent's, read the developer's own queries.
var roles = map[string][]string{
	"write": {"PutObject"},
	"read":  {"GetObject", "HeadObject", "ListObjectsV2"},
}

// credential is what an AWS credential_process prints, the form fluent-bit's
// profile provider reads (src/aws/flb_aws_credentials_process.c) and the AWS
// SDKs read.
type credential struct {
	Version         int    `json:"Version"`
	AccessKeyId     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
}

type mintRequest struct {
	Endpoint     string // https://{account}[.eu].r2.cloudflarestorage.com
	Account      string
	Bucket       string
	ParentKeyID  string
	ParentSecret string
	Actions      []string
	Prefixes     []string
	TTL          time.Duration
	Now          time.Time
}

// mint signs a temporary credential locally, as R2's own example does
// (developers.cloudflare.com/r2/examples/authenticate-r2-temp-credentials):
// an HS256 JWT keyed with the parent's secret, the derived secret the
// SHA-256 hex of the JWT, the session token base64("jwt/" + JWT), and the
// parent's key id reused. The parent's secret is used for the signature
// only and is in nothing returned.
func mint(r mintRequest) (credential, error) {
	if r.Account == "" || r.Bucket == "" || r.ParentKeyID == "" || r.ParentSecret == "" {
		return credential{}, errors.New("account, bucket, parent key id and parent secret are all required")
	}
	if len(r.Actions) == 0 {
		return credential{}, errors.New("no actions")
	}
	if r.TTL <= 0 {
		return credential{}, fmt.Errorf("ttl %s is not positive", r.TTL)
	}
	u, err := url.Parse(r.Endpoint)
	if err != nil || u.Host == "" {
		return credential{}, fmt.Errorf("endpoint %q: not a URL with a host", r.Endpoint)
	}
	iat := r.Now.Unix()
	exp := r.Now.Add(r.TTL).Unix()
	claims := map[string]any{
		"bucket":  r.Bucket,
		"actions": r.Actions,
		"paths":   map[string][]string{"prefixPaths": r.Prefixes, "objectPaths": {}},
		"sub":     r.Account,
		"iss":     r.ParentKeyID,
		"aud":     u.Host,
		"iat":     iat,
		"exp":     exp,
	}
	jwt, err := signJWT(claims, r.ParentSecret)
	if err != nil {
		return credential{}, err
	}
	return credential{
		Version:         1,
		AccessKeyId:     r.ParentKeyID,
		SecretAccessKey: hashHex([]byte(jwt)),
		SessionToken:    base64.StdEncoding.EncodeToString([]byte("jwt/" + jwt)),
		Expiration:      time.Unix(exp, 0).UTC().Format(time.RFC3339),
	}, nil
}

// signJWT is a compact JWS with HS256 (RFC 7515, 7519): base64url without
// padding of the header and the claims, then of the HMAC over both.
func signJWT(claims map[string]any, secret string) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(m.Sum(nil)), nil
}

// endpoint is R2's S3 API for an account, in the EU jurisdiction when asked:
// a bucket created with --jurisdiction eu is reachable only there.
func endpoint(account, jurisdiction string) (string, error) {
	switch jurisdiction {
	case "", "default":
		return "https://" + account + ".r2.cloudflarestorage.com", nil
	case "eu":
		return "https://" + account + ".eu.r2.cloudflarestorage.com", nil
	}
	return "", fmt.Errorf("jurisdiction %q: expected default or eu", jurisdiction)
}

// feedPrefix is the key prefix of one feed, {root}/{cluster}/: the host
// agent's key layout, /{KEY_ROOT}/{CLUSTER_NAME}/{date}/{HH}/{NODE_NAME}/...
// (HOST_MODE.md). The machine is below the date, so no prefix narrows a
// credential to one machine; one parent token per machine is what lets a
// single machine's credentials be revoked.
func feedPrefix(root, cluster string) (string, error) {
	for _, s := range []struct{ name, v string }{{"root", root}, {"cluster", cluster}} {
		if s.v == "" {
			return "", fmt.Errorf("%s is required", s.name)
		}
		if strings.ContainsAny(s.v, "/ ") || strings.HasPrefix(s.v, ".") || strings.HasPrefix(s.v, "_") {
			return "", fmt.Errorf("%s %q: a single key segment, not starting with . or _", s.name, s.v)
		}
	}
	return root + "/" + cluster + "/", nil
}

// parseTTL is a Go duration, or a whole number of days with a d suffix.
func parseTTL(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("ttl %q: expected a positive number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	t, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("ttl %q: %w", s, err)
	}
	return t, nil
}

// expiresIn is the time left on a credential's Expiration, negative once
// past it.
func expiresIn(c credential, now time.Time) (time.Duration, error) {
	t, err := time.Parse(time.RFC3339, c.Expiration)
	if err != nil {
		return 0, fmt.Errorf("Expiration %q: %w", c.Expiration, err)
	}
	return t.Sub(now), nil
}

// hexOK reports whether s is the 64 hex digits of a SHA-256, as a derived
// secret is.
func hexOK(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}
