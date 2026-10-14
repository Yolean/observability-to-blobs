// The SigV4 signer of the stdlib S3 clients. compactor/sigv4.go is the
// source; images/blobs-to-prometheus has a byte-identical copy, which
// `go generate` there refreshes and a test there keeps in sync. Edit only
// the compactor's.

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

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
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+keyID+"/"+day+"/"+region+"/s3/aws4_request"+
		", SignedHeaders="+signed+", Signature="+sigV4(secret, region, amzDate, canonical))
}

// sigV4 is the hex signature of a canonical request made at amzDate
// (20060102T150405Z), with the scope {day}/{region}/s3/aws4_request: what
// signV4 signs with and what the compactor's put endpoint (compactor/put.go)
// checks against.
func sigV4(secret, region, amzDate, canonical string) string {
	day := amzDate[:8]
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashHex([]byte(canonical))
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	return hex.EncodeToString(hmacSHA256(k, toSign))
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
