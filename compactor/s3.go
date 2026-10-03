package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// s3Bucket is a bucket on an S3-compatible endpoint. DuckDB's httpfs reads
// and writes the objects (s3:// paths, the secret set up by open); this
// client makes the three calls DuckDB cannot: ListObjectsV2, which returns
// upload times that a DuckDB glob does not, HeadObject, and DeleteObjects.
// It is stdlib only (SigV4, path-style URLs), so the module graph stays the
// engine's: three calls do not justify an SDK and its dependencies in the
// pinned image.
type s3Bucket struct {
	bucket   string
	endpoint string // host[:port]
	tls      bool
	region   string
	keyID    string
	secret   string
	http     *http.Client
	now      func() time.Time
}

func (b *s3Bucket) URL(key string) string { return "s3://" + b.bucket + "/" + key }

func (b *s3Bucket) Prepare(string) error { return nil }

// List returns every key under prefix with its upload time, one
// ListObjectsV2 page per 1,000 keys.
func (b *s3Bucket) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		body, err := b.do(ctx, http.MethodGet, "", q, nil, http.StatusOK)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		var page struct {
			IsTruncated           bool
			NextContinuationToken string
			Contents              []struct {
				Key          string
				LastModified time.Time
			}
		}
		if err := xml.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, c := range page.Contents {
			out = append(out, Object{Key: c.Key, Modified: c.LastModified})
		}
		if !page.IsTruncated {
			return out, nil
		}
		if page.NextContinuationToken == "" {
			return nil, fmt.Errorf("list %s: truncated page without a continuation token", prefix)
		}
		token = page.NextContinuationToken
	}
}

func (b *s3Bucket) Stat(ctx context.Context, key string) (bool, error) {
	_, err := b.do(ctx, http.MethodHead, key, nil, nil, http.StatusOK)
	if se, ok := err.(*s3Error); ok && se.status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("head %s: %w", key, err)
	}
	return true, nil
}

// Delete removes keys with DeleteObjects, 1,000 per request. A key that is
// already gone counts as deleted; any other per-key error fails the call.
func (b *s3Bucket) Delete(ctx context.Context, keys []string) error {
	for len(keys) > 0 {
		n := min(len(keys), 1000)
		type obj struct{ Key string }
		req := struct {
			XMLName xml.Name `xml:"Delete"`
			Quiet   bool
			Object  []obj
		}{Quiet: true}
		for _, k := range keys[:n] {
			req.Object = append(req.Object, obj{k})
		}
		payload, err := xml.Marshal(req)
		if err != nil {
			return err
		}
		body, err := b.do(ctx, http.MethodPost, "", url.Values{"delete": {""}}, payload, http.StatusOK)
		if err != nil {
			return fmt.Errorf("delete %d keys: %w", n, err)
		}
		var res struct {
			Error []struct{ Key, Code, Message string }
		}
		if err := xml.Unmarshal(body, &res); err != nil {
			return fmt.Errorf("delete %d keys: %w", n, err)
		}
		for _, e := range res.Error {
			if e.Code != "NoSuchKey" {
				return fmt.Errorf("delete %s: %s %s", e.Key, e.Code, e.Message)
			}
		}
		keys = keys[n:]
	}
	return nil
}

type s3Error struct {
	status int
	body   string
}

func (e *s3Error) Error() string { return fmt.Sprintf("HTTP %d %s", e.status, e.body) }

// do sends one signed request for key ("" for the bucket) and returns the
// body of a response with status want.
func (b *s3Bucket) do(ctx context.Context, method, key string, q url.Values, payload []byte, want int) ([]byte, error) {
	scheme := "https"
	if !b.tls {
		scheme = "http"
	}
	path := "/" + b.bucket + "/" + key
	u := &url.URL{Scheme: scheme, Host: b.endpoint, Path: path, RawPath: s3Escape(path, false), RawQuery: canonicalQuery(q)}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	if payload != nil {
		sum := md5.Sum(payload)
		req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
		req.Header.Set("Content-Type", "application/xml")
	}
	signV4(req, hashHex(payload), b.region, b.keyID, b.secret, b.now())
	res, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != want {
		return nil, &s3Error{status: res.StatusCode, body: strings.TrimSpace(string(body[:min(len(body), 512)]))}
	}
	return body, nil
}

func hashHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// signV4 adds x-amz-date and an AWS Signature Version 4 Authorization header,
// signing host and every header already set on req.
func signV4(req *http.Request, payloadHash, region, keyID, secret string, t time.Time) {
	amzDate := t.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{
		req.Method, req.URL.EscapedPath(), req.URL.RawQuery, canonHeaders.String(), signed, payloadHash,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashHex([]byte(canonical))
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+keyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSHA256(k, toSign)))
}

// canonicalQuery is SigV4's query string: sorted, every name and value
// escaped, "=" kept for empty values.
func canonicalQuery(q url.Values) string {
	parts := make([]string, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			parts = append(parts, s3Escape(k, true)+"="+s3Escape(v, true))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

// s3Escape percent-encodes every byte but the unreserved ones, and "/"
// unless slash is set: SigV4's URI encoding.
func s3Escape(s string, slash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/' && !slash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
