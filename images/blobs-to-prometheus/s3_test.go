package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A bucket that pages its listing two keys at a time and checks that every
// request is path-style, escaped and signed.
func TestS3Store(t *testing.T) {
	keys := []string{"raw1/c/a b.parquet", "raw1/c/x+y=z.parquet", "raw1/c/ö.parquet"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=k/") {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/b/" {
			from := 0
			if r.URL.Query().Get("continuation-token") == "t2" {
				from = 2
			}
			fmt.Fprint(w, "<ListBucketResult>")
			for _, k := range keys[from:min(from+2, len(keys))] {
				fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", k)
			}
			if from == 0 {
				fmt.Fprint(w, "<IsTruncated>true</IsTruncated><NextContinuationToken>t2</NextContinuationToken>")
			}
			fmt.Fprint(w, "</ListBucketResult>")
			return
		}
		if r.URL.EscapedPath() == "/b/raw1/c/a%20b.parquet" {
			fmt.Fprint(w, "body")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>")
	}))
	defer srv.Close()
	st := &s3Store{bucket: "b", endpoint: strings.TrimPrefix(srv.URL, "http://"), region: "us-east-1",
		keyID: "k", secret: "s", timeout: 5 * time.Second, http: srv.Client()}
	ctx := context.Background()

	got, err := st.list(ctx, "raw1/c/")
	if err != nil || strings.Join(got, ",") != strings.Join(keys, ",") {
		t.Fatalf("list: %v %v", got, err)
	}
	body, err := st.get(ctx, keys[0])
	if err != nil || string(body) != "body" {
		t.Fatalf("get: %q %v", body, err)
	}
	_, err = st.get(ctx, "raw1/c/gone.parquet")
	if !errors.Is(err, errNotFound) || !strings.HasSuffix(err.Error(), "HTTP 404 NoSuchKey: The specified key does not exist.") {
		t.Fatalf("missing key: %v", err)
	}
	st.keyID = "other"
	if _, err := st.list(ctx, "raw1/c/"); err == nil || errors.Is(err, errNotFound) || !strings.Contains(err.Error(), "HTTP 403 unsigned") {
		t.Fatalf("refused list: %v", err)
	}
}
