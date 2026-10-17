package main

//go:generate cp ../../compactor/sigv4.go ../../compactor/sigv4_test.go .

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// s3Store is the bucket on an S3-compatible endpoint: ListObjectsV2 and
// GetObject, path-style, signed by sigv4.go (the compactor's signer), so
// the image needs no S3 SDK for its two calls.
type s3Store struct {
	bucket   string
	endpoint string // host[:port]
	tls      bool
	region   string
	keyID    string
	secret   string
	timeout  time.Duration // for a whole list, or a get with its body
	http     *http.Client
}

func (s *s3Store) list(ctx context.Context, prefix string) ([]string, error) {
	keys, _, err := s.listing(ctx, prefix, "")
	return keys, err
}

func (s *s3Store) dirs(ctx context.Context, prefix string) ([]string, error) {
	_, dirs, err := s.listing(ctx, prefix, "/")
	return dirs, err
}

// listing is a ListObjectsV2 of prefix, every page: the keys, and with a
// delimiter the common prefixes.
func (s *s3Store) listing(ctx context.Context, prefix, delimiter string) (keys, dirs []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if delimiter != "" {
			q.Set("delimiter", delimiter)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		res, err := s.do(ctx, "", q)
		if err != nil {
			return nil, nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		var page struct {
			IsTruncated           bool
			NextContinuationToken string
			Contents              []struct{ Key string }
			CommonPrefixes        []struct{ Prefix string }
		}
		err = xml.NewDecoder(res.Body).Decode(&page)
		res.Body.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, c := range page.Contents {
			keys = append(keys, c.Key)
		}
		for _, p := range page.CommonPrefixes {
			dirs = append(dirs, p.Prefix)
		}
		if !page.IsTruncated {
			return keys, dirs, nil
		}
		if page.NextContinuationToken == "" {
			return nil, nil, fmt.Errorf("list %s: truncated page without a continuation token", prefix)
		}
		token = page.NextContinuationToken
	}
}

func (s *s3Store) get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	res, err := s.do(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

// do sends one signed GET for key ("" for the bucket) and returns the
// response if it is a 200. A 404 is errNotFound, as the minio-go client
// this replaced reported it: a missing bucket too.
func (s *s3Store) do(ctx context.Context, key string, q url.Values) (*http.Response, error) {
	scheme := "http"
	if s.tls {
		scheme = "https"
	}
	path := "/" + s.bucket + "/" + key
	u := &url.URL{Scheme: scheme, Host: s.endpoint, Path: path, RawPath: s3Escape(path, false), RawQuery: canonicalQuery(q)}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	signV4(req, hashHex(nil), s.region, s.keyID, s.secret, time.Now())
	res, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusOK {
		return res, nil
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	err = fmt.Errorf("HTTP %d %s", res.StatusCode, s3ErrorText(body))
	if res.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %v", errNotFound, err)
	}
	return nil, err
}

// s3ErrorText is an S3 error body's code and message on one line, or the
// start of the body when it is no S3 error document.
func s3ErrorText(body []byte) string {
	var e struct{ Code, Message string }
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return strings.TrimSpace(string(body[:min(len(body), 512)]))
}
