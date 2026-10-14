package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// s3Bucket is a bucket on an S3-compatible endpoint. DuckDB's httpfs reads
// and writes the objects (s3:// paths, the secret set up by open); this
// client makes the four calls DuckDB cannot: ListObjectsV2, which returns
// upload times that a DuckDB glob does not, HeadObject, CopyObject, which
// moves a checked object to its key, and DeleteObjects. It is stdlib only
// (SigV4, path-style URLs), so the module graph stays the engine's: four
// calls do not justify an SDK and its dependencies in the pinned image.
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
	err := b.list(ctx, prefix, "", func(objs []Object, _ []string) {
		out = append(out, objs...)
	})
	return out, err
}

// Dirs returns the common prefixes right under prefix, which ends in "/":
// a ListObjectsV2 with delimiter "/", which returns one entry per
// "directory" however many keys it holds.
func (b *s3Bucket) Dirs(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	err := b.list(ctx, prefix, "/", func(_ []Object, dirs []string) {
		out = append(out, dirs...)
	})
	return out, err
}

// list pages through a ListObjectsV2, handing each page to fn.
func (b *s3Bucket) list(ctx context.Context, prefix, delimiter string, fn func([]Object, []string)) error {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if delimiter != "" {
			q.Set("delimiter", delimiter)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		body, err := b.do(ctx, http.MethodGet, "", q, nil, http.StatusOK)
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		var page struct {
			IsTruncated           bool
			NextContinuationToken string
			Contents              []struct {
				Key          string
				LastModified time.Time
			}
			CommonPrefixes []struct{ Prefix string }
		}
		if err := xml.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		objs := make([]Object, 0, len(page.Contents))
		for _, c := range page.Contents {
			objs = append(objs, Object{Key: c.Key, Modified: c.LastModified})
		}
		dirs := make([]string, 0, len(page.CommonPrefixes))
		for _, p := range page.CommonPrefixes {
			dirs = append(dirs, p.Prefix)
		}
		fn(objs, dirs)
		if !page.IsTruncated {
			return nil
		}
		if page.NextContinuationToken == "" {
			return fmt.Errorf("list %s: truncated page without a continuation token", prefix)
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

// Move copies from to to with CopyObject and then deletes from. The copy is
// server-side and replaces to whole, so a reader of to sees the old object
// or the new one, never a part. CopyObject takes objects up to 5 GiB.
func (b *s3Bucket) Move(ctx context.Context, from, to string) error {
	hdr := http.Header{"X-Amz-Copy-Source": {s3Escape("/"+b.bucket+"/"+from, false)}}
	body, err := b.send(ctx, http.MethodPut, to, nil, hdr, nil, http.StatusOK)
	if err != nil {
		return fmt.Errorf("copy %s to %s: %w", from, to, err)
	}
	// CopyObject can fail after it has sent 200, with an Error document
	var res struct{ XMLName xml.Name }
	if err := xml.Unmarshal(body, &res); err != nil || res.XMLName.Local != "CopyObjectResult" {
		return fmt.Errorf("copy %s to %s: %s", from, to, strings.TrimSpace(string(body[:min(len(body), 512)])))
	}
	return b.Delete(ctx, []string{from})
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

func (b *s3Bucket) do(ctx context.Context, method, key string, q url.Values, payload []byte, want int) ([]byte, error) {
	return b.send(ctx, method, key, q, nil, payload, want)
}

// send sends one signed request for key ("" for the bucket), with the
// headers hdr signed too, and returns the body of a response with status
// want.
func (b *s3Bucket) send(ctx context.Context, method, key string, q url.Values, hdr http.Header, payload []byte, want int) ([]byte, error) {
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
	for k, v := range hdr {
		req.Header[http.CanonicalHeaderKey(k)] = v
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
