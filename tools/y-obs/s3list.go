package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
		s.listRequests++
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
// does have: the clusters, or the cluster's first and last hour of raw
// and dates of the tiers.
func (s *session) explainEmpty(ctx context.Context) error {
	clusters, err := s.clusters(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(clusters, s.cluster) {
		if len(clusters) == 0 {
			return fail(exitNotFound, "the bucket holds no cluster's data under raw1/, hourly/ or daily/")
		}
		return fail(exitNotFound, "no cluster %q in the bucket; clusters with data: %s", s.cluster, strings.Join(clusters, ", "))
	}
	var have []string
	if dates, err := s.dates(ctx, "raw1/"+s.cluster+"/"); err != nil {
		return err
	} else if len(dates) > 0 {
		first, last := dates[0], dates[len(dates)-1]
		fh, err := s.hourEdge(ctx, "raw1/"+s.cluster+"/"+first+"/", false)
		if err != nil {
			return err
		}
		lh, err := s.hourEdge(ctx, "raw1/"+s.cluster+"/"+last+"/", true)
		if err != nil {
			return err
		}
		have = append(have, fmt.Sprintf("raw %sT%s..%sT%s", first, fh, last, lh))
	}
	for _, t := range []struct{ name, prefix string }{
		{"hourly", "hourly/" + s.cluster + "/logs/"},
		{"daily", "daily/" + s.cluster + "/logs/"},
	} {
		dates, err := s.dates(ctx, t.prefix)
		if err != nil {
			return err
		}
		if len(dates) > 0 {
			have = append(have, fmt.Sprintf("%s %s..%s", t.name, dates[0], dates[len(dates)-1]))
		}
	}
	if len(have) == 0 {
		return fail(exitNotFound, "cluster %s has no data in %s, and no dated data at all", s.cluster, s.w)
	}
	return fail(exitNotFound, "cluster %s has no data in %s; it has %s (UTC): move or widen the window", s.cluster, s.w, strings.Join(have, ", "))
}

var dateName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// dates are the date names under prefix, sorted.
func (s *session) dates(ctx context.Context, prefix string) ([]string, error) {
	names, err := s.listPrefixes(ctx, prefix)
	if err != nil {
		return nil, err
	}
	names = slices.DeleteFunc(names, func(d string) bool { return !dateName.MatchString(d) })
	sort.Strings(names)
	return names, nil
}

// hourEdge is the first, or with last the last, hour name under a date.
func (s *session) hourEdge(ctx context.Context, prefix string, last bool) (string, error) {
	hours, err := s.listPrefixes(ctx, prefix)
	if err != nil || len(hours) == 0 {
		return "??", err
	}
	sort.Strings(hours)
	if last {
		return hours[len(hours)-1], nil
	}
	return hours[0], nil
}

// clusters are the names under raw1/, hourly/ and daily/ that hold a
// cluster's data: raw dates, or a tier's tables. Other prefixes there,
// such as an access check's, are not clusters.
func (s *session) clusters(ctx context.Context) ([]string, error) {
	found := map[string]bool{}
	for _, tier := range []string{"raw1/", "hourly/", "daily/"} {
		names, err := s.listPrefixes(ctx, tier)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if found[n] {
				continue
			}
			children, err := s.listPrefixes(ctx, tier+n+"/")
			if err != nil {
				return nil, err
			}
			for _, c := range children {
				if tier == "raw1/" && dateName.MatchString(c) || tier != "raw1/" && slices.Contains(tierTables, c) {
					found[n] = true
					break
				}
			}
		}
	}
	var out []string
	for n := range found {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// noObjects is a plan with nothing to read.
func (s *session) noObjects() bool { return s.planned() == 0 }

// listedObject is one key of a full listing.
type listedObject struct {
	key      string
	size     int64
	modified time.Time
}

// listObjects lists every key under prefix with its size and upload time:
// ListObjectsV2 pages of 1,000 keys; for --blobs-dir a directory walk.
func (s *session) listObjects(ctx context.Context, prefix string, fn func(listedObject)) error {
	if !strings.HasPrefix(s.root, "s3://") {
		base := filepath.Join(s.root, prefix)
		return filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(s.root, path)
			fn(listedObject{key: filepath.ToSlash(rel), size: info.Size(), modified: info.ModTime().UTC()})
			return nil
		})
	}
	st := s.store
	scheme := "http"
	if st.tls {
		scheme = "https"
	}
	client := &http.Client{Timeout: 60 * time.Second}
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		path := "/" + st.bucket + "/"
		u := &url.URL{Scheme: scheme, Host: st.endpoint, Path: path, RawPath: s3Escape(path, false), RawQuery: canonicalQuery(q)}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		if st.token != "" {
			req.Header.Set("X-Amz-Security-Token", st.token)
		}
		signV4(req, hashHex(nil), st.region, st.keyID, st.secret, time.Now())
		s.listRequests++
		res, err := client.Do(req)
		if err != nil {
			return s.wrap(err)
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<20))
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return s.wrap(fmt.Errorf("listing %s: HTTP %d %s", prefix, res.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)]))))
		}
		var page struct {
			IsTruncated           bool
			NextContinuationToken string
			Contents              []struct {
				Key          string
				Size         int64
				LastModified time.Time
			}
		}
		if err := xml.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("listing %s: %w", prefix, err)
		}
		for _, c := range page.Contents {
			fn(listedObject{key: c.Key, size: c.Size, modified: c.LastModified})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return nil
		}
		token = page.NextContinuationToken
	}
}
