package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"testing"
)

// sigv4.go and its tests are copies of the compactor's; `go generate`
// refreshes them. Outside a checkout of the whole repository there is
// nothing to compare with.
func TestSigV4CopiedFromCompactor(t *testing.T) {
	for _, name := range []string{"sigv4.go", "sigv4_test.go"} {
		src, err := os.ReadFile("../../compactor/" + name)
		if errors.Is(err, fs.ErrNotExist) {
			t.Skipf("no compactor/%s to compare with", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		cp, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(src, cp) {
			t.Errorf("%s differs from compactor/%s: edit the compactor's, then run go generate in developer-feed/cloudflare", name, name)
		}
	}
}
