package main

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
)

// release.env's DUCKDB_VERSION must be the engine this module links: the
// embedded httpfs extension loads only into its own version.
func TestReleaseEnv(t *testing.T) {
	f, err := os.Open("release.env")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pins := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && !strings.HasPrefix(k, "#") {
			pins[k] = v
		}
	}
	s := &session{plan: map[string][]string{}}
	if err := s.openEngine(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	v, err := s.strings(context.Background(), "SELECT version()")
	if err != nil || len(v) == 0 || v[0] != pins["DUCKDB_VERSION"] {
		t.Errorf("the engine is %v (%v), release.env pins %s", v, err, pins["DUCKDB_VERSION"])
	}
	for _, p := range []string{"linux_amd64", "linux_arm64", "osx_amd64", "osx_arm64"} {
		if len(pins["HTTPFS_SHA256_"+p]) != 64 {
			t.Errorf("no httpfs checksum for %s", p)
		}
	}
}
