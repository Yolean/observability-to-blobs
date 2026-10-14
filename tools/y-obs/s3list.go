package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// listPrefixes returns the names right under prefix ("raw1/" gives the
// clusters, "raw1/c/" the dates): a ListObjectsV2 with delimiter "/", one
// entry per "directory" however many keys it holds; for --blobs-dir the
// directory's entries.
func (s *session) listPrefixes(ctx context.Context, prefix string) ([]string, error) {
	if !strings.HasPrefix(s.root, "s3://") {
		entries, err := os.ReadDir(filepath.Join(s.root, prefix))
		if err != nil {
			return nil, nil
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
		return names, nil
	}
	st := s.store
	scheme := "http"
	if st.tls {
		scheme = "https"
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var names []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "delimiter": {"/"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		path := "/" + st.bucket + "/"
		u := &url.URL{Scheme: scheme, Host: st.endpoint, Path: path, RawPath: s3Escape(path, false), RawQuery: canonicalQuery(q)}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		if st.token != "" {
			req.Header.Set("X-Amz-Security-Token", st.token)
		}
		signV4(req, hashHex(nil), st.region, st.keyID, st.secret, time.Now())
		res, err := client.Do(req)
		if err != nil {
			return nil, s.wrap(err)
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, s.wrap(fmt.Errorf("listing %s: HTTP %d %s", prefix, res.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)]))))
		}
		var page struct {
			IsTruncated           bool
			NextContinuationToken string
			CommonPrefixes        []struct{ Prefix string }
		}
		if err := xml.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("listing %s: %w", prefix, err)
		}
		for _, p := range page.CommonPrefixes {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(p.Prefix, prefix), "/"))
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return names, nil
		}
		token = page.NextContinuationToken
	}
}

// explainEmpty is exit 4 for a window no tier has, saying what the bucket
// does have: the clusters, or the cluster's first and last dates per tier.
func (s *session) explainEmpty(ctx context.Context) error {
	clusters := map[string]bool{}
	for _, tier := range []string{"raw1/", "hourly/", "daily/"} {
		names, err := s.listPrefixes(ctx, tier)
		if err != nil {
			return err
		}
		for _, n := range names {
			clusters[n] = true
		}
	}
	if !clusters[s.cluster] {
		if len(clusters) == 0 {
			return fail(exitNotFound, "the bucket holds no raw1/, hourly/ or daily/ data")
		}
		var names []string
		for n := range clusters {
			names = append(names, n)
		}
		sort.Strings(names)
		return fail(exitNotFound, "no cluster %q in the bucket; clusters with data: %s", s.cluster, strings.Join(names, ", "))
	}
	var have []string
	for _, t := range []struct{ name, prefix string }{
		{"raw", "raw1/" + s.cluster + "/"},
		{"hourly", "hourly/" + s.cluster + "/logs/"},
		{"daily", "daily/" + s.cluster + "/logs/"},
	} {
		dates, err := s.listPrefixes(ctx, t.prefix)
		if err != nil {
			return err
		}
		dates = slices.DeleteFunc(dates, func(d string) bool { return len(d) != 10 })
		sort.Strings(dates)
		if len(dates) > 0 {
			have = append(have, fmt.Sprintf("%s %s..%s", t.name, dates[0], dates[len(dates)-1]))
		}
	}
	return fail(exitNotFound, "cluster %s has no data in %s; it has %s: move or widen the window", s.cluster, s.w, strings.Join(have, ", "))
}

// noObjects is a plan with nothing to read.
func (s *session) noObjects() bool { return s.planned() == 0 }
