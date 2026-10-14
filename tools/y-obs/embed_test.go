package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"testing"
)

// The embedded views are copies; `go generate` refreshes them. Outside a
// checkout of the whole repository there is nothing to compare with.
func TestSQLCopied(t *testing.T) {
	for name, src := range sqlSources {
		want, err := os.ReadFile(src)
		if errors.Is(err, fs.ErrNotExist) {
			t.Skipf("no %s to compare with", src)
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := sqlFS.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("sql/%s differs from %s: edit the source, then run go generate in tools/y-obs", name, src)
		}
	}
}

func TestSigV4Copied(t *testing.T) {
	for _, name := range []string{"sigv4.go", "sigv4_test.go"} {
		want, err := os.ReadFile("../../compactor/" + name)
		if errors.Is(err, fs.ErrNotExist) {
			t.Skipf("no compactor/%s to compare with", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from compactor/%s: edit the compactor's, then run go generate in tools/y-obs", name, name)
		}
	}
}
