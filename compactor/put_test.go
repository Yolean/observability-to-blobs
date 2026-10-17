package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readRequest parses a request as the server reads it off the wire.
func readRequest(t *testing.T, head string, body []byte) *http.Request {
	t.Helper()
	raw := strings.ReplaceAll(strings.TrimLeft(head, "\n"), "\n", "\r\n") + "\r\n"
	r, err := http.ReadRequest(bufio.NewReader(io.MultiReader(strings.NewReader(raw), bytes.NewReader(body))))
	if err != nil {
		t.Fatal(err)
	}
	r.RemoteAddr = "192.0.2.1:1234"
	return r
}

// The PUT example of the Amazon S3 API reference ("Signature calculations
// for the Authorization header: transferring payload in a single chunk"),
// with its published signature: a signer other than this repository's.
func TestVerifySigV4AWSExample(t *testing.T) {
	const head = `
PUT /test$file.text HTTP/1.1
Host: examplebucket.s3.amazonaws.com
Date: Fri, 24 May 2013 00:00:00 GMT
Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class,Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd
x-amz-date: 20130524T000000Z
x-amz-storage-class: REDUCED_REDUNDANCY
x-amz-content-sha256: 44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072
Content-Length: 21
`
	body := []byte("Welcome to Amazon S3.")
	at := time.Date(2013, 5, 24, 0, 5, 0, 0, time.UTC)
	const id, secret = "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	payload, err := verifySigV4(readRequest(t, head, body), "us-east-1", id, secret, at)
	if err != nil || payload != hashHex(body) {
		t.Fatalf("payload %s, err %v", payload, err)
	}
	if _, err := verifySigV4(readRequest(t, head, body), "us-east-1", id, secret+"x", at); s3Code(err) != "SignatureDoesNotMatch" {
		t.Fatalf("another secret: %v", err)
	}
	// an x-amz- header that is not signed
	extra := strings.Replace(head, "x-amz-date:", "x-amz-meta-x: 1\nx-amz-date:", 1)
	if _, err := verifySigV4(readRequest(t, extra, body), "us-east-1", id, secret, at); s3Code(err) != "AccessDenied" {
		t.Fatalf("an unsigned x-amz header: %v", err)
	}
}

func s3Code(err error) string {
	var e *s3Err
	if errors.As(err, &e) {
		return e.code
	}
	return ""
}

// A PutObject that stock Fluent Bit v5.1.3's out_s3 sent (use_put_object
// on, send_content_md5 on, zstd, json_lines), captured off the wire with
// AWS_ACCESS_KEY_ID=AKIDCAPTURE and AWS_SECRET_ACCESS_KEY=capturedSecret.
const (
	capturedSecret = "capture-secret-not-a-real-one"
	capturedHead   = `
PUT /node-agent/raw1/capture/2026-10-04/07/node-1/3650-8BCzE9qE.jsonl.zst HTTP/1.1
Host: host.docker.internal:18099
Content-Length: 166
User-Agent: aws-fluent-bit-plugin
Content-Encoding: zstd
Content-MD5: GDoURcOIfEEo3vQvekub6g==
x-amz-date: 20261004T073657Z
x-amz-content-sha256: 4312476efc102669b5c7d4c4282be3a45d220e4fbe20bfe3f93bdb54b3ae87bd
Authorization: AWS4-HMAC-SHA256 Credential=AKIDCAPTURE/20261004/us-east-1/s3/aws4_request, SignedHeaders=content-encoding;content-length;content-md5;host;user-agent;x-amz-content-sha256;x-amz-date, Signature=04d015b73ed70b81434e19b6542f4f68eadd5bc00caa3f51e5c1523812f308d3
Connection: keep-alive
`
	capturedBody = "KLUv/WCoAuUEAIJJHRtga9se57qi9xgbjMlDEC+URCTSa22VoymKGMBPqX5spEmdsc/q/22kzshI9WYip2tm48nlVWfcw61+RvszakpVBzyOJQ5MwMAYjWahFG4dW5TWGOKQa4uCuXWMISjNITc4Vz3H7IXOSingTFITtLm3rjXnCA0AApMPQPkAtBRA+QCUD0B5APHHCLYAygegpQDKB1B1IiacAQ=="
	capturedKey  = "raw1/capture/2026-10-04/07/node-1/3650-8BCzE9qE.jsonl.zst"
)

var capturedAt = time.Date(2026, 10, 4, 7, 37, 30, 0, time.UTC)

// testPut is a put endpoint for cluster "capture" over a spool in a temp
// directory, with the captured request's key pair.
type testPut struct {
	t *testing.T
	p *putServer
}

func newTestPut(t *testing.T) *testPut {
	t.Helper()
	sp, err := openSpool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &testPut{t: t, p: &putServer{l: layout{"capture"}, bucket: "node-agent", region: "us-east-1",
		keyID: "AKIDCAPTURE", secret: capturedSecret, maxBytes: 1 << 20, spool: sp, m: newMetrics("capture"),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return capturedAt }}}
}

func (tp *testPut) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	tp.p.ServeHTTP(rec, r)
	return rec
}

func (tp *testPut) captured() *http.Request {
	body, err := base64.StdEncoding.DecodeString(capturedBody)
	if err != nil {
		tp.t.Fatal(err)
	}
	return readRequest(tp.t, capturedHead, body)
}

// signed is a PUT of body to path signed by this repository's own client
// (s3.go), with Content-MD5 when md5b64 is set, as of capturedAt.
func (tp *testPut) signed(path string, body []byte, secret, md5b64 string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "http://compactor:9000"+path, bytes.NewReader(body))
	if md5b64 != "" {
		r.Header.Set("Content-MD5", md5b64)
	}
	signV4(r, hashHex(body), "us-east-1", "AKIDCAPTURE", secret, capturedAt)
	return r
}

func (tp *testPut) stored(key string) []byte {
	b, err := os.ReadFile(tp.p.spool.URL(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		tp.t.Fatal(err)
	}
	return b
}

func errorCode(rec *httptest.ResponseRecorder) string {
	body := rec.Body.String()
	_, rest, _ := strings.Cut(body, "<Code>")
	code, _, _ := strings.Cut(rest, "</Code>")
	return code
}

func TestPutCapturedOutS3Request(t *testing.T) {
	tp := newTestPut(t)
	rec := tp.do(tp.captured())
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	md, _ := base64.StdEncoding.DecodeString("GDoURcOIfEEo3vQvekub6g==")
	if got := rec.Header().Get("ETag"); got != `"`+hex.EncodeToString(md)+`"` {
		t.Fatalf("ETag %s", got)
	}
	body, _ := base64.StdEncoding.DecodeString(capturedBody)
	if !bytes.Equal(tp.stored(capturedKey), body) {
		t.Fatal("the spool does not hold the body under the key")
	}
	// out_s3 retries an upload whose answer it did not get: the same key
	// with the same content is a no-op
	if rec := tp.do(tp.captured()); rec.Code != http.StatusOK || tp.p.m.puts["duplicate"] != 1 {
		t.Fatalf("the repeat: %d %s, %v", rec.Code, rec.Body, tp.p.m.puts)
	}
	// other content under the key: the first stays, the answer is 200
	other := tp.signed("/node-agent/"+capturedKey, []byte("other"), capturedSecret, "")
	if rec := tp.do(other); rec.Code != http.StatusOK || tp.p.m.puts["conflict"] != 1 {
		t.Fatalf("a conflict: %d %s, %v", rec.Code, rec.Body, tp.p.m.puts)
	}
	if !bytes.Equal(tp.stored(capturedKey), body) {
		t.Fatal("a conflict replaced the first content")
	}
	var b strings.Builder
	tp.p.m.spool = tp.p.spool.stats
	tp.p.m.write(&b)
	for _, want := range []string{
		`blobs_compactor_put_requests_total{cluster="capture",result="stored"} 1`,
		`blobs_compactor_put_requests_total{cluster="capture",result="duplicate"} 1`,
		`blobs_compactor_put_requests_total{cluster="capture",result="conflict"} 1`,
		`blobs_compactor_put_bytes_total{cluster="capture"} 166`,
		`blobs_compactor_spool_files{cluster="capture"} 1`,
		`blobs_compactor_spool_bytes{cluster="capture"} 166`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, b.String())
		}
	}
}

func TestPutRefuses(t *testing.T) {
	body := []byte("a body")
	sum := md5.Sum(body)
	goodMD5 := base64.StdEncoding.EncodeToString(sum[:])
	key := "/node-agent/raw1/capture/2026-10-04/07/node-1/0000-x.jsonl.zst"
	for _, tc := range []struct {
		name   string
		req    func(tp *testPut) *http.Request
		status int
		code   string
	}{
		{"another secret", func(tp *testPut) *http.Request {
			return tp.signed(key, body, "wrong", "")
		}, 403, "SignatureDoesNotMatch"},
		{"another key id", func(tp *testPut) *http.Request {
			r := tp.captured()
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "AKIDCAPTURE", "AKIDOTHER", 1))
			return r
		}, 403, "SignatureDoesNotMatch"},
		{"no signature", func(tp *testPut) *http.Request {
			r := tp.captured()
			r.Header.Del("Authorization")
			return r
		}, 403, "AccessDenied"},
		{"a body other than the signed hash", func(tp *testPut) *http.Request {
			b, _ := base64.StdEncoding.DecodeString(capturedBody)
			b[len(b)-1] ^= 1
			return readRequest(tp.t, capturedHead, b)
		}, 400, "XAmzContentSHA256Mismatch"},
		{"a body shorter than Content-Length", func(tp *testPut) *http.Request {
			r := tp.signed(key, body, capturedSecret, "")
			r.Body = io.NopCloser(bytes.NewReader(body[:3]))
			return r
		}, 400, "IncompleteBody"},
		{"a Content-MD5 other than the body's", func(tp *testPut) *http.Request {
			return tp.signed(key, body, capturedSecret, "GDoURcOIfEEo3vQvekub6g==")
		}, 400, "BadDigest"},
		{"a Content-MD5 that is not one", func(tp *testPut) *http.Request {
			return tp.signed(key, body, capturedSecret, "bm8=")
		}, 400, "InvalidDigest"},
		{"UNSIGNED-PAYLOAD", func(tp *testPut) *http.Request {
			r := httptest.NewRequest(http.MethodPut, "http://compactor:9000"+key, bytes.NewReader(body))
			signV4(r, "UNSIGNED-PAYLOAD", "us-east-1", "AKIDCAPTURE", capturedSecret, capturedAt)
			return r
		}, 403, "AccessDenied"},
		{"a clock 20 minutes ahead", func(tp *testPut) *http.Request {
			tp.p.now = func() time.Time { return capturedAt.Add(20 * time.Minute) }
			return tp.captured()
		}, 403, "RequestTimeTooSkewed"},
		{"another region", func(tp *testPut) *http.Request {
			tp.p.region = "eu-central"
			return tp.captured()
		}, 403, "SignatureDoesNotMatch"},
		{"a clock 20 minutes ahead and another secret", func(tp *testPut) *http.Request {
			tp.p.now = func() time.Time { return capturedAt.Add(20 * time.Minute) }
			return tp.signed(key, body, "wrong", "")
		}, 403, "SignatureDoesNotMatch"},
		{"no x-amz-date", func(tp *testPut) *http.Request {
			r := tp.captured()
			r.Header.Del("X-Amz-Date")
			return r
		}, 403, "SignatureDoesNotMatch"},
		{"not SigV4", func(tp *testPut) *http.Request {
			r := tp.captured()
			r.Header.Set("Authorization", "AWS AKIDCAPTURE:c2ln")
			return r
		}, 403, "SignatureDoesNotMatch"},
		{"another bucket", func(tp *testPut) *http.Request {
			return tp.signed(strings.Replace(key, "node-agent", "other", 1), body, capturedSecret, "")
		}, 404, "NoSuchBucket"},
		{"another cluster", func(tp *testPut) *http.Request {
			return tp.signed(strings.Replace(key, "/capture/", "/other/", 1), body, capturedSecret, "")
		}, 403, "AccessDenied"},
		{"a body over PUT_MAX_BYTES", func(tp *testPut) *http.Request {
			tp.p.maxBytes = 5
			return tp.signed(key, body, capturedSecret, goodMD5)
		}, 400, "EntityTooLarge"},
		{"a GET", func(tp *testPut) *http.Request {
			r := httptest.NewRequest(http.MethodGet, "http://compactor:9000"+key, nil)
			signV4(r, hashHex(nil), "us-east-1", "AKIDCAPTURE", capturedSecret, capturedAt)
			return r
		}, 501, "NotImplemented"},
		{"a multipart upload", func(tp *testPut) *http.Request {
			r := httptest.NewRequest(http.MethodPut, "http://compactor:9000"+key+"?partNumber=1&uploadId=x", bytes.NewReader(body))
			signV4(r, hashHex(body), "us-east-1", "AKIDCAPTURE", capturedSecret, capturedAt)
			return r
		}, 501, "NotImplemented"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPut(t)
			rec := tp.do(tc.req(tp))
			if rec.Code != tc.status || errorCode(rec) != tc.code {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
			// an unauthenticated caller learns neither the key id nor
			// the region nor the clock
			if tc.code == "SignatureDoesNotMatch" {
				for _, secret := range []string{tp.p.keyID, tp.p.region, "AKIDOTHER", "x-amz-date", "server's clock"} {
					if strings.Contains(rec.Body.String(), secret) {
						t.Fatalf("the answer says %q: %s", secret, rec.Body)
					}
				}
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/xml" {
				t.Fatalf("Content-Type %s", ct)
			}
			if tp.p.m.puts[tc.code] != 1 {
				t.Fatalf("not counted: %v", tp.p.m.puts)
			}
			files, err := tp.p.spool.stats()
			if err != nil || files.files != 0 {
				t.Fatalf("the spool holds %d files (%v)", files.files, err)
			}
			if tmp, _ := os.ReadDir(filepath.Join(tp.p.spool.root, spoolTmp)); len(tmp) != 0 {
				t.Fatalf("_tmp holds %d files", len(tmp))
			}
		})
	}
	tp := newTestPut(t)
	if rec := tp.do(tp.signed(key, body, capturedSecret, goodMD5)); rec.Code != http.StatusOK {
		t.Fatalf("the request the cases above vary: %d %s", rec.Code, rec.Body)
	}
}

func TestPutKeys(t *testing.T) {
	tp := newTestPut(t)
	for _, k := range []string{
		"raw1/capture/2026-10-04/07/node-1/3650-8BCzE9qE.jsonl.zst",
		"raw1/capture/2026-10-04/23/ip-10-0-0-1.eu-north-1.compute.internal/0000-a_b.jsonl.zst",
		"raw1/capture/2026-10-04/07/_k8sagent/0000-a.jsonl.zst",
	} {
		if got, err := tp.p.key("/node-agent/" + k); err != nil || got != k {
			t.Errorf("%s: %s %v", k, got, err)
		}
	}
	for _, k := range []string{
		"raw1/capture/2026-10-04/07/../0000-a.jsonl.zst",
		"raw1/capture/2026-10-04/07/./0000-a.jsonl.zst",
		"raw1/capture/2026-10-04/07/n/..jsonl.zst",
		"raw1/capture/2026-10-04/07/n/_0000-a.jsonl.zst",
		"raw1/capture/2026-10-04/24/n/0000-a.jsonl.zst",
		"raw1/capture/2026-13-04/07/n/0000-a.jsonl.zst",
		"raw1/capture/2026-10-04/07/n/0000-a.json",
		"raw1/capture/2026-10-04/07/n/sub/0000-a.jsonl.zst",
		"raw1/capture/2026-10-04/07/n%2F/0000-a.jsonl.zst",
		"raw1/other/2026-10-04/07/n/0000-a.jsonl.zst",
		"hourly/capture/logs/2026-10-04/07.parquet",
		"_tmp/x.jsonl.zst",
	} {
		if _, err := tp.p.key("/node-agent/" + k); s3Code(err) != "AccessDenied" {
			t.Errorf("%s: %v, want AccessDenied", k, err)
		}
	}
	for _, p := range []string{"/other/" + capturedKey, "/" + capturedKey, "/"} {
		if _, err := tp.p.key(p); s3Code(err) != "NoSuchBucket" {
			t.Errorf("%s: %v, want NoSuchBucket", p, err)
		}
	}
}

// answerRecorder notes when the answer is written.
type answerRecorder struct {
	*httptest.ResponseRecorder
	events *[]string
}

func (a answerRecorder) WriteHeader(code int) {
	*a.events = append(*a.events, "answer")
	a.ResponseRecorder.WriteHeader(code)
}

// A put answers only once the file is synced, renamed to its key, and the
// key's directory synced, each new directory's parent before it.
func TestPutIsDurableBeforeItsAnswer(t *testing.T) {
	tp := newTestPut(t)
	sp := tp.p.spool
	dst := sp.URL(capturedKey)
	var events []string
	sp.syncFile = func(f *os.File) error {
		if _, err := os.Stat(dst); err == nil {
			t.Error("the key exists before its file is synced")
		}
		if filepath.Dir(f.Name()) != filepath.Join(sp.root, spoolTmp) {
			t.Errorf("synced %s, not a file in _tmp", f.Name())
		}
		events = append(events, "file")
		return f.Sync()
	}
	sp.syncDir = func(p string) error {
		rel, _ := filepath.Rel(sp.root, p)
		if _, err := os.Stat(dst); err == nil {
			rel += " with the key"
		}
		events = append(events, "dir "+filepath.ToSlash(rel))
		return syncDir(p)
	}
	tp.p.ServeHTTP(answerRecorder{httptest.NewRecorder(), &events}, tp.captured())
	want := []string{"file", "dir .", "dir raw1", "dir raw1/capture", "dir raw1/capture/2026-10-04",
		"dir raw1/capture/2026-10-04/07", "dir raw1/capture/2026-10-04/07/node-1 with the key", "answer"}
	if strings.Join(events, ", ") != strings.Join(want, ", ") {
		t.Fatalf("events\n%s\nwant\n%s", strings.Join(events, ", "), strings.Join(want, ", "))
	}

	// a failed sync answers 500 and leaves nothing at the key
	for _, fail := range []string{"file", "dir"} {
		tp := newTestPut(t)
		sp := tp.p.spool
		broken := errors.New("sync failed")
		if fail == "file" {
			sp.syncFile = func(*os.File) error { return broken }
		} else {
			sp.syncDir = func(string) error { return broken }
		}
		rec := tp.do(tp.captured())
		if rec.Code != http.StatusInternalServerError || errorCode(rec) != "InternalError" {
			t.Fatalf("a failed %s sync: %d %s", fail, rec.Code, rec.Body)
		}
		if tp.stored(capturedKey) != nil {
			t.Fatalf("a failed %s sync left the key", fail)
		}
	}
}

// A put that answered 500 because a directory sync failed leaves the next
// put of the key to sync that directory entry before its 200, whether the
// failed put's undo (removing the directory it made, or the file it
// renamed) worked or failed too: a directory made by a new directory's
// failed parent sync, and the key's directory after the rename.
func TestPutRetryAfterAFailedSyncSyncsWhatItDependsOn(t *testing.T) {
	for _, tc := range []struct {
		name, failing string
		removeFails   bool
	}{
		{"a new directory's parent", "raw1/capture/2026-10-04", false},
		{"a new directory's parent, its removal failing", "raw1/capture/2026-10-04", true},
		{"the key's directory after the rename", "raw1/capture/2026-10-04/07/node-1", false},
		{"the key's directory after the rename, the file's removal failing", "raw1/capture/2026-10-04/07/node-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPut(t)
			sp := tp.p.spool
			dst := sp.URL(capturedKey)
			failOnce := true
			var events []string
			sp.syncDir = func(p string) error {
				rel, _ := filepath.Rel(sp.root, p)
				rel = filepath.ToSlash(rel)
				if _, err := os.Stat(dst); err == nil {
					rel += " with the key"
				}
				events = append(events, "dir "+rel)
				if failOnce && strings.HasPrefix(rel, tc.failing) {
					failOnce = false
					return errors.New("sync failed")
				}
				return syncDir(p)
			}
			if tc.removeFails {
				sp.remove = func(string) error { return errors.New("remove failed") }
			}
			if rec := tp.do(tp.captured()); rec.Code != http.StatusInternalServerError {
				t.Fatalf("the put whose sync fails: %d %s", rec.Code, rec.Body)
			}
			if failOnce {
				t.Fatalf("no sync of %s: %v", tc.failing, events)
			}
			events = nil
			tp.p.ServeHTTP(answerRecorder{httptest.NewRecorder(), &events}, tp.captured())
			want := "dir " + tc.failing
			if strings.HasSuffix(tc.failing, "node-1") {
				want += " with the key"
			}
			synced := false
			for _, e := range events {
				if e == "answer" {
					break
				}
				synced = synced || e == want
			}
			if !synced || events[len(events)-1] != "answer" {
				t.Fatalf("the retry: events %v, want %q before the answer", events, want)
			}
			body, _ := base64.StdEncoding.DecodeString(capturedBody)
			if !bytes.Equal(tp.stored(capturedKey), body) {
				t.Fatal("the retry did not leave the body at the key")
			}
			// a third put of the key finds every directory known durable:
			// it syncs only the key's directory, for the duplicate
			events = nil
			if rec := tp.do(tp.captured()); rec.Code != http.StatusOK {
				t.Fatalf("the third put: %d %s", rec.Code, rec.Body)
			}
			if strings.Join(events, ", ") != "dir raw1/capture/2026-10-04/07/node-1 with the key" {
				t.Fatalf("the third put synced %v", events)
			}
		})
	}
}

// countRecorder fails a test whose put is counted before it is answered.
type countRecorder struct {
	*httptest.ResponseRecorder
	t *testing.T
	m *metrics
}

func (c countRecorder) WriteHeader(code int) {
	c.m.mu.Lock()
	n := len(c.m.puts)
	c.m.mu.Unlock()
	if n != 0 {
		c.t.Errorf("the put was counted before its answer: %v", c.m.puts)
	}
	c.ResponseRecorder.WriteHeader(code)
}

// A scrape walks the spool without the metrics' lock, and a put is
// answered before it is counted: a slow walk delays no acknowledgement.
func TestPutIsAnsweredWhileAScrapeWalksTheSpool(t *testing.T) {
	tp := newTestPut(t)
	walking, release, scraped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	tp.p.m.spool = func() (spoolStats, error) {
		close(walking)
		<-release
		return tp.p.spool.stats()
	}
	go func() {
		var b strings.Builder
		tp.p.m.write(&b)
		if !strings.Contains(b.String(), "blobs_compactor_spool_files{cluster=\"capture\"} 1\n") {
			t.Errorf("the scrape:\n%s", b.String())
		}
		close(scraped)
	}()
	<-walking
	answered := make(chan int)
	go func() {
		rec := countRecorder{httptest.NewRecorder(), t, tp.p.m}
		tp.p.ServeHTTP(rec, tp.captured())
		answered <- rec.Code
	}()
	select {
	case code := <-answered:
		if code != http.StatusOK {
			t.Fatalf("the put: %d", code)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the put waited for the scrape's walk of the spool")
	}
	close(release)
	<-scraped
	tp.p.m.mu.Lock()
	defer tp.p.m.mu.Unlock()
	if tp.p.m.puts["stored"] != 1 {
		t.Fatalf("counted %v", tp.p.m.puts)
	}
}

// Refusals are logged with what the answer did not say, at most
// refusalLogs a minute, the next line counting the ones dropped; a body
// over PUT_MAX_BYTES is an error, with a message that says what to change.
func TestPutRefusalLogs(t *testing.T) {
	tp := newTestPut(t)
	var logs bytes.Buffer
	tp.p.log = slog.New(slog.NewJSONHandler(&logs, nil))
	other := func() *http.Request {
		r := tp.captured()
		r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "AKIDCAPTURE", "AKIDOTHER", 1))
		return r
	}
	for i := 0; i < refusalLogs+5; i++ {
		tp.do(other())
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != refusalLogs {
		t.Fatalf("%d log lines for %d refusals in a minute, want %d:\n%s", len(lines), refusalLogs+5, refusalLogs, logs.String())
	}
	if !strings.Contains(lines[0], `"detail":"the access key id AKIDOTHER is not PUT_ACCESS_KEY_ID"`) ||
		!strings.Contains(lines[0], `"level":"WARN"`) {
		t.Fatalf("the log line: %s", lines[0])
	}
	logs.Reset()
	tp.p.now = func() time.Time { return capturedAt.Add(61 * time.Second) }
	tp.p.maxBytes = 5
	body := []byte("a body")
	rec := tp.do(tp.signed("/node-agent/raw1/capture/2026-10-04/07/node-1/0000-x.jsonl.zst", body, capturedSecret, ""))
	if errorCode(rec) != "EntityTooLarge" || !strings.Contains(rec.Body.String(), "total_file_size") {
		t.Fatalf("a body over PUT_MAX_BYTES: %d %s", rec.Code, rec.Body)
	}
	if l := logs.String(); !strings.Contains(l, `"level":"ERROR"`) || !strings.Contains(l, `"refusals_not_logged":5`) {
		t.Fatalf("the next minute's first line: %s", l)
	}
}
