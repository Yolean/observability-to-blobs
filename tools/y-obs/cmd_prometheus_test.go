package main

import (
	"strings"
	"testing"
)

func TestServeArgs(t *testing.T) {
	got := strings.Join(serveArgs([]string{"--since", "6h", "--no-open", "--stop=false", "-status", "--cluster=prod", "--port", "9091"}), " ")
	if got != "--since 6h --cluster=prod --port 9091" {
		t.Errorf("%s", got)
	}
}

func TestPromKey(t *testing.T) {
	for in, want := range map[[2]string]string{{"", "prod"}: "env-prod", {"prod eu", "a/b"}: "prod_eu-a_b"} {
		if got := promKey(in[0], in[1]); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
}
