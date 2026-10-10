package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The put endpoint (PUT_LISTEN): the S3 PutObject that Fluent Bit's stock
// out_s3 sends with use_put_object on, so that agents upload their raw
// objects to the compactor's spool instead of to the bucket
// (compactor/README.md, "The put endpoint and spool mode"). Only what
// out_s3 sends is implemented: a path-style PUT of one object, signed with
// AWS Signature Version 4 over a payload hash, optionally with
// Content-MD5. Every other request is an S3 error document.
type putServer struct {
	l        layout
	bucket   string
	region   string
	keyID    string
	secret   string
	maxBytes int64
	spool    *spool
	// folded has the keys of committed sidecars (folded.go)
	folded *foldedKeys
	m      *metrics
	log    *slog.Logger
	now    func() time.Time
	// refusals limits the refusals' log lines
	refusals logLimit
}

// refusalLogs is how many refusals a minute are logged.
const refusalLogs = 10

// logLimit allows refusalLogs log lines a minute, and counts the lines it
// drops for the next one it allows.
type logLimit struct {
	mu         sync.Mutex
	start      time.Time
	n, dropped int
}

func (l *logLimit) allow(now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= time.Minute || now.Before(l.start) {
		l.start, l.n = now, 0
	}
	if l.n >= refusalLogs {
		l.dropped++
		return false, 0
	}
	l.n++
	dropped := l.dropped
	l.dropped = 0
	return true, dropped
}

// maxSkew is how far a request's x-amz-date may be from the clock, as S3
// allows.
const maxSkew = 15 * time.Minute

// s3Err is an S3 error response, and the detail the log adds to it.
type s3Err struct {
	status        int
	code, message string
	detail        string
}

func (e *s3Err) Error() string { return e.code + ": " + e.message }

func denied(code, msg string) *s3Err {
	return &s3Err{status: http.StatusForbidden, code: code, message: msg}
}

// ServeHTTP answers a request, and only then counts and logs it.
func (p *putServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	etag, result, err := p.put(r)
	if err != nil {
		var e *s3Err
		if !errors.As(err, &e) {
			e = &s3Err{status: http.StatusInternalServerError, code: "InternalError", message: "the object was not stored"}
		}
		writeS3Error(w, r, e)
		http.NewResponseController(w).Flush()
		p.m.put(e.code, 0)
		if e.code == "InternalError" {
			p.log.Error("put", "path", r.URL.Path, "err", err)
			return
		}
		// a refusal is the caller's, and anyone can send them: logged at
		// most refusalLogs a minute, the next line counting those dropped
		ok, dropped := p.refusals.allow(p.now())
		if !ok {
			return
		}
		level := slog.LevelWarn
		if e.code == "EntityTooLarge" {
			// the shipper retries it forever: its config must change
			level = slog.LevelError
		}
		attrs := []any{"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "code", e.code, "message", e.message}
		if e.detail != "" {
			attrs = append(attrs, "detail", e.detail)
		}
		if dropped > 0 {
			attrs = append(attrs, "refusals_not_logged", dropped)
		}
		p.log.Log(r.Context(), level, "put refused", attrs...)
		return
	}
	w.Header().Set("ETag", `"`+hex.EncodeToString(etag)+`"`)
	w.WriteHeader(http.StatusOK)
	http.NewResponseController(w).Flush()
	p.m.put(result, r.ContentLength)
	switch result {
	case "conflict":
		p.log.Error("put: the key holds other content, which it keeps", "key", strings.TrimPrefix(r.URL.Path, "/"+p.bucket+"/"))
	case "already_folded":
		p.log.Warn("put: the key was folded and its file deleted, so its content cannot be compared; nothing is stored",
			"key", strings.TrimPrefix(r.URL.Path, "/"+p.bucket+"/"))
	}
}

// put checks a request and stores its body. It returns the stored
// object's MD5 and what the write did: stored, duplicate, conflict or
// already_folded.
func (p *putServer) put(r *http.Request) ([]byte, string, error) {
	if r.Method != http.MethodPut || r.URL.RawQuery != "" {
		return nil, "", &s3Err{status: http.StatusNotImplemented, code: "NotImplemented", message: "only PutObject of a whole object is implemented"}
	}
	payload, err := verifySigV4(r, p.region, p.keyID, p.secret, p.now())
	if err != nil {
		return nil, "", err
	}
	if !sha256HexRE.MatchString(payload) {
		// UNSIGNED-PAYLOAD and the aws-chunked streaming forms; out_s3
		// signs the payload's hash
		return nil, "", denied("AccessDenied", "x-amz-content-sha256 must be the SHA-256 of the payload, not "+payload)
	}
	key, err := p.key(r.URL.Path)
	if err != nil {
		return nil, "", err
	}
	if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
		return nil, "", &s3Err{status: http.StatusLengthRequired, code: "MissingContentLength", message: "Content-Length is required"}
	}
	if r.ContentLength > p.maxBytes {
		return nil, "", &s3Err{status: http.StatusBadRequest, code: "EntityTooLarge",
			message: fmt.Sprintf("the body is %d bytes, over this endpoint's limit of %d (PUT_MAX_BYTES): keep out_s3's total_file_size well below it",
				r.ContentLength, p.maxBytes)}
	}
	if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return nil, "", &s3Err{status: http.StatusNotImplemented, code: "NotImplemented", message: "aws-chunked uploads are not implemented"}
	}
	var contentMD5 []byte
	if v := r.Header.Get("Content-MD5"); v != "" {
		if contentMD5, err = base64.StdEncoding.DecodeString(v); err != nil || len(contentMD5) != 16 {
			return nil, "", &s3Err{status: http.StatusBadRequest, code: "InvalidDigest", message: "Content-MD5 is not the base64 of 16 bytes"}
		}
	}
	var folded func() (bool, error)
	if p.folded != nil {
		h, _ := p.l.rawHour(key)
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		// the hour's sidecar, read now on a miss rather than under the
		// spool's lock
		if _, err := p.folded.load(ctx, h); err != nil {
			return nil, "", fmt.Errorf("read the sidecar of %s: %w", key, err)
		}
		folded = func() (bool, error) { return p.folded.has(ctx, h, key) }
	}
	etag, did, err := p.spool.write(key, r.Body, r.ContentLength, payload, contentMD5, folded)
	var eb *errBody
	if errors.As(err, &eb) {
		return nil, "", &s3Err{status: http.StatusBadRequest, code: eb.code, message: eb.msg}
	}
	if err != nil {
		return nil, "", err
	}
	return etag, [...]string{spoolStored: "stored", spoolDuplicate: "duplicate", spoolConflict: "conflict",
		spoolAlreadyFolded: "already_folded"}[did], nil
}

var sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// spoolKeyRE is a raw key the spool takes: raw1/{cluster}/{date}/{HH}/
// {node}/{name}.jsonl.zst, the node and the name of the characters a node
// name and out_s3's $UUID have, the node starting with a letter, a digit
// or "_" (the cluster agent's producer segment is _k8sagent), the name
// with a letter or a digit, so no segment is "." or "..".
var spoolKeyRE = regexp.MustCompile(`^raw1/([^/]+)/(\d{4}-\d{2}-\d{2})/(\d{2})/[A-Za-z0-9_][A-Za-z0-9._-]{0,252}/[A-Za-z0-9][A-Za-z0-9._-]{0,200}\.jsonl\.zst$`)

// key returns the raw key of a path-style request path, /{bucket}/{key}:
// the bucket must be S3_BUCKET, and the key one of this cluster's raw
// keys.
func (p *putServer) key(path string) (string, error) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if bucket != p.bucket {
		return "", &s3Err{status: http.StatusNotFound, code: "NoSuchBucket", message: "the bucket is " + p.bucket}
	}
	m := spoolKeyRE.FindStringSubmatch(key)
	if m == nil || m[1] != p.l.cluster {
		return "", denied("AccessDenied", "the key is not under "+p.l.rawPrefix()+"{date}/{HH}/{node}/ or not a .jsonl.zst")
	}
	if _, err := time.Parse(dateLayout+"/15", m[2]+"/"+m[3]); err != nil {
		return "", denied("AccessDenied", "the key's date and hour are not one: "+m[2]+"/"+m[3])
	}
	return key, nil
}

// verifySigV4 checks a request's AWS Signature Version 4 Authorization
// header against the key pair, the region and the clock, and returns the
// payload hash it signs (x-amz-content-sha256), which the caller holds
// the body to. The canonical request is S3's: the path URI-encoded once,
// the query sorted, the signed headers' values trimmed.
func verifySigV4(r *http.Request, region, keyID, secret string, now time.Time) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", denied("AccessDenied", "anonymous requests are refused: sign with AWS Signature Version 4")
	}
	params, ok := strings.CutPrefix(auth, "AWS4-HMAC-SHA256 ")
	if !ok {
		return "", unverified("the Authorization header is not AWS4-HMAC-SHA256")
	}
	var cred, signedList, sig string
	for _, f := range strings.Split(params, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		switch k {
		case "Credential":
			cred = v
		case "SignedHeaders":
			signedList = v
		case "Signature":
			sig = v
		}
	}
	if cred == "" || signedList == "" || sig == "" {
		return "", unverified("Credential, SignedHeaders and Signature are required")
	}
	scope := strings.Split(cred, "/")
	if len(scope) != 5 || scope[3] != "s3" || scope[4] != "aws4_request" {
		return "", unverified("the credential is not {key id}/{date}/{region}/s3/aws4_request")
	}
	if scope[0] != keyID {
		return "", unverified("the access key id " + scope[0] + " is not PUT_ACCESS_KEY_ID")
	}
	if scope[2] != region {
		return "", unverified("the credential's region " + scope[2] + " is not S3_REGION " + region)
	}
	amzDate := r.Header.Get("X-Amz-Date")
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return "", unverified("no valid x-amz-date header")
	}
	if scope[1] != amzDate[:8] {
		return "", unverified("the credential's date is not x-amz-date's")
	}
	signed := strings.Split(signedList, ";")
	if !sort.StringsAreSorted(signed) {
		return "", unverified("SignedHeaders are not sorted")
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	var headers strings.Builder
	for _, h := range signed {
		headers.WriteString(h + ":" + canonicalHeaderValue(r, h) + "\n")
	}
	canonical := strings.Join([]string{
		r.Method, s3Escape(r.URL.Path, false), canonicalQuery(r.URL.Query()), headers.String(), signedList, payload,
	}, "\n")
	want := sigV4(secret, region, amzDate, canonical)
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return "", unverified("the signature is not the one PUT_SECRET_ACCESS_KEY makes")
	}

	// the caller holds the key pair: from here the answers say what is wrong
	if d := now.Sub(t); d > maxSkew || d < -maxSkew {
		return "", denied("RequestTimeTooSkewed", fmt.Sprintf("x-amz-date %s is %s from the server's clock", amzDate, d.Round(time.Second)))
	}
	isSigned := map[string]bool{}
	for _, h := range signed {
		isSigned[h] = true
	}
	for _, h := range []string{"host", "x-amz-date", "x-amz-content-sha256"} {
		if !isSigned[h] {
			return "", denied("AccessDenied", h+" must be signed")
		}
	}
	for h := range r.Header {
		if l := strings.ToLower(h); strings.HasPrefix(l, "x-amz-") && !isSigned[l] {
			return "", denied("AccessDenied", "the header "+l+" is not signed")
		}
	}
	return payload, nil
}

// unverified is the one answer to a signed request whose signature is not
// verified, whatever the reason: a caller without the key pair learns
// neither the key id nor the region nor the clock. detail, the reason, is
// for the log.
func unverified(detail string) *s3Err {
	return &s3Err{status: http.StatusForbidden, code: "SignatureDoesNotMatch",
		message: "The request signature we calculated does not match the signature you provided. Check your key and signing method.",
		detail:  detail}
}

// canonicalHeaderValue is a header's values as SigV4 signs them: joined by
// commas, each trimmed, inner runs of spaces folded to one. Go's server
// moves Host out of the header map, and may Content-Length.
func canonicalHeaderValue(r *http.Request, name string) string {
	var vs []string
	switch name {
	case "host":
		vs = []string{r.Host}
	case "content-length":
		vs = r.Header.Values(name)
		if len(vs) == 0 && r.ContentLength >= 0 {
			vs = []string{strconv.FormatInt(r.ContentLength, 10)}
		}
	default:
		vs = r.Header.Values(name)
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(out, ",")
}

// writeS3Error writes e as S3's XML error document.
func writeS3Error(w http.ResponseWriter, r *http.Request, e *s3Err) {
	var id [8]byte
	rand.Read(id[:])
	doc := struct {
		XMLName   xml.Name `xml:"Error"`
		Code      string
		Message   string
		Resource  string
		RequestId string
	}{Code: e.code, Message: e.message, Resource: r.URL.Path, RequestId: hex.EncodeToString(id[:])}
	b, _ := xml.Marshal(doc)
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(xml.Header)+len(b)))
	// the body was not read: the connection cannot carry another request
	w.Header().Set("Connection", "close")
	w.WriteHeader(e.status)
	if r.Method != http.MethodHead {
		w.Write([]byte(xml.Header))
		w.Write(b)
	}
}
