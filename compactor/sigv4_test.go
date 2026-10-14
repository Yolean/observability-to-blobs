// The tests of sigv4.go, copied with it (see there).

package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The SigV4 examples of the Amazon S3 API reference ("Signature
// calculations for the Authorization header"), with their published
// signatures.
func TestSignV4Examples(t *testing.T) {
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	const id, secret = "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	get, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	get.Header.Set("Range", "bytes=0-9")
	signV4(get, hashHex(nil), "us-east-1", id, secret, at)
	if want := "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"; !strings.HasSuffix(get.Header.Get("Authorization"), want) {
		t.Errorf("GET object: %s", get.Header.Get("Authorization"))
	}
	list, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?"+canonicalQuery(map[string][]string{"max-keys": {"2"}, "prefix": {"J"}}), nil)
	signV4(list, hashHex(nil), "us-east-1", id, secret, at)
	if want := "SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"; !strings.HasSuffix(list.Header.Get("Authorization"), want) {
		t.Errorf("GET bucket: %s", list.Header.Get("Authorization"))
	}
}

func TestS3Escape(t *testing.T) {
	if got := s3Escape("/b/daily/namespace=a b/x~y", false); got != "/b/daily/namespace%3Da%20b/x~y" {
		t.Fatal(got)
	}
	if got := canonicalQuery(map[string][]string{"prefix": {"raw1/c/"}, "list-type": {"2"}, "delete": {""}}); got != "delete=&list-type=2&prefix=raw1%2Fc%2F" {
		t.Fatal(got)
	}
}
