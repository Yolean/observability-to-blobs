package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// R2's S3 API takes any region name and documents "auto".
const region = "auto"

// s3 is a minimal path-style S3 client for one bucket, signed by sigv4.go
// with a temporary credential's session token.
type s3 struct {
	endpoint string
	bucket   string
	cred     credential
	client   *http.Client
	now      func() time.Time
}

type s3Result struct {
	status int
	code   string // the error document's Code, if any
}

func (r s3Result) String() string {
	if r.code != "" {
		return fmt.Sprintf("%d %s", r.status, r.code)
	}
	return fmt.Sprintf("%d", r.status)
}

// do sends one request: method, a key (empty for the bucket), a query, the
// body and extra headers. Every header is signed, the session token too.
func (c *s3) do(method, key string, query url.Values, body []byte, headers map[string]string) (s3Result, error) {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return s3Result{}, err
	}
	u.Path = "/" + c.bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath = s3Escape(u.Path, false)
	if len(query) > 0 {
		u.RawQuery = canonicalQuery(query)
	}
	req, err := http.NewRequest(method, u.String(), bytes.NewReader(body))
	if err != nil {
		return s3Result{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if c.cred.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.cred.SessionToken)
	}
	signV4(req, hashHex(body), region, c.cred.AccessKeyId, c.cred.SecretAccessKey, c.now())
	resp, err := c.client.Do(req)
	if err != nil {
		return s3Result{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var doc struct {
		Code string `xml:"Code"`
	}
	_ = xml.Unmarshal(b, &doc)
	return s3Result{status: resp.StatusCode, code: doc.Code}, nil
}

// check is one call and what it must answer: allowed, a 2xx; denied, a 403.
type check struct {
	name    string
	allowed bool
	call    func() (s3Result, error)
}

type checkOutcome struct {
	check
	result s3Result
	err    error
}

func (o checkOutcome) ok() bool {
	if o.err != nil {
		return false
	}
	if o.allowed {
		return o.result.status/100 == 2
	}
	return o.result.status == http.StatusForbidden
}

// verifyChecks is what a credential of a role must and must not do, against
// the feed's prefix. For write, the put-only check of HOST_MODE.md ("Put
// only"): one PutObject under the prefix, and every other call refused.
// The object it puts, {prefix}.verify/{stamp}-{random}.txt, stays: the
// credential cannot delete it. Its depth differs from the raw layout's, so
// the readers' fixed-depth globs never match it.
func verifyChecks(c *s3, role, prefix string) []check {
	stamp := c.now().UTC().Format("20060102T150405Z") + "-" + randomHex(4)
	own := prefix + ".verify/" + stamp + ".txt"
	outside := ".verify-outside/" + stamp + ".txt"
	body := []byte("developer-feed verify " + stamp + "\n")
	unsigned := *c
	unsigned.cred.SessionToken = ""
	switch role {
	case "write":
		return []check{
			{"PutObject under the prefix", true, func() (s3Result, error) { return c.do(http.MethodPut, own, nil, body, nil) }},
			{"GetObject of that object", false, func() (s3Result, error) { return c.do(http.MethodGet, own, nil, nil, nil) }},
			{"HeadObject of that object", false, func() (s3Result, error) { return c.do(http.MethodHead, own, nil, nil, nil) }},
			{"DeleteObject of that object", false, func() (s3Result, error) { return c.do(http.MethodDelete, own, nil, nil, nil) }},
			{"ListObjectsV2 of the bucket", false, func() (s3Result, error) {
				return c.do(http.MethodGet, "", url.Values{"list-type": {"2"}}, nil, nil)
			}},
			{"ListObjectsV2 of the prefix", false, func() (s3Result, error) {
				return c.do(http.MethodGet, "", url.Values{"list-type": {"2"}, "prefix": {prefix}}, nil, nil)
			}},
			{"PutObject outside the prefix", false, func() (s3Result, error) { return c.do(http.MethodPut, outside, nil, body, nil) }},
			{"CopyObject under the prefix", false, func() (s3Result, error) {
				return c.do(http.MethodPut, own+".copy", nil, nil, map[string]string{"X-Amz-Copy-Source": "/" + c.bucket + "/" + own})
			}},
			{"CreateMultipartUpload under the prefix", false, func() (s3Result, error) {
				return c.do(http.MethodPost, own+".multipart", url.Values{"uploads": {""}}, nil, nil)
			}},
			{"PutObject without the session token", false, func() (s3Result, error) {
				return unsigned.do(http.MethodPut, own+".unsigned", nil, body, nil)
			}},
		}
	case "read":
		return []check{
			{"ListObjectsV2 of the prefix", true, func() (s3Result, error) {
				return c.do(http.MethodGet, "", url.Values{"list-type": {"2"}, "prefix": {prefix}}, nil, nil)
			}},
			{"PutObject under the prefix", false, func() (s3Result, error) { return c.do(http.MethodPut, own, nil, body, nil) }},
			{"DeleteObject under the prefix", false, func() (s3Result, error) { return c.do(http.MethodDelete, own, nil, nil, nil) }},
			{"ListObjectsV2 outside the prefix", false, func() (s3Result, error) {
				return c.do(http.MethodGet, "", url.Values{"list-type": {"2"}, "prefix": {".verify-outside/"}}, nil, nil)
			}},
		}
	}
	return nil
}

func runChecks(checks []check) []checkOutcome {
	out := make([]checkOutcome, 0, len(checks))
	for _, ch := range checks {
		r, err := ch.call()
		out = append(out, checkOutcome{check: ch, result: r, err: err})
	}
	return out
}

func report(w io.Writer, outcomes []checkOutcome) (failed int) {
	for _, o := range outcomes {
		want := "denied (403)"
		if o.allowed {
			want = "allowed (2xx)"
		}
		got := o.result.String()
		if o.err != nil {
			got = "error: " + o.err.Error()
		}
		mark := "ok  "
		if !o.ok() {
			mark = "FAIL"
			failed++
		}
		fmt.Fprintf(w, "%s %-40s want %-14s got %s\n", mark, o.name, want, got)
	}
	return failed
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// checkPrefix guards a configured prefix: one or more segments, each ending
// in a slash, as R2's prefixPaths match keys "starting with" a prefix.
func checkPrefix(p string) error {
	if p == "" || !strings.HasSuffix(p, "/") || strings.HasPrefix(p, "/") {
		return fmt.Errorf("prefix %q: expected segments ending in /, not starting with /", p)
	}
	return nil
}
