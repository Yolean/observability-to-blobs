package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestProgressLine(t *testing.T) {
	for _, c := range []struct {
		l    loadProgress
		want string
	}{
		{loadProgress{Stage: "reading", Objects: 147}, "y-obs: reading 147 objects"},
		{loadProgress{Stage: "catalog", Samples: 1568203, Series: 6599}, "y-obs: read 1568203 samples of 6599 series; loading names"},
		{loadProgress{Stage: "loading", Samples: 200, Pushed: 50}, "y-obs: loading samples: 50 of 200 (25%)"},
	} {
		if got := progressLine(c.l); got != c.want {
			t.Errorf("%+v: %q", c.l, got)
		}
	}
}

// waitLoaded on a progress file the test writes stage by stage: the
// catalog callback fires once, with the URL, and the wait ends at loaded.
func TestWaitLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.progress")
	write := func(l loadProgress) {
		b, _ := json.Marshal(l)
		os.WriteFile(path, b, 0o600)
	}
	write(loadProgress{Stage: "reading", Objects: 3})
	go func() {
		for _, l := range []loadProgress{{Stage: "catalog", Series: 2, Samples: 10, URL: "http://x"}, {Stage: "loading", Pushed: 2, Samples: 10, URL: "http://x"}, {Stage: "loaded", Pushed: 10, Samples: 10, URL: "http://x"}} {
			time.Sleep(300 * time.Millisecond)
			write(l)
		}
	}()
	var urls []string
	if err := waitLoaded(t.Context(), path, make(chan struct{}), func(u string) { urls = append(urls, u) }); err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != "http://x" {
		t.Errorf("catalog callbacks: %v", urls)
	}
	write(loadProgress{Stage: "failed", Error: "remote write: HTTP 400"})
	if err := waitLoaded(t.Context(), path, make(chan struct{}), func(string) {}); exitOf(err) != exitStore || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("failed: %v", err)
	}
	write(loadProgress{Stage: "failed", Error: "loading costs 27, over --max-objects 20", Code: exitBudget})
	if err := waitLoaded(t.Context(), path, make(chan struct{}), func(string) {}); exitOf(err) != exitBudget {
		t.Errorf("over budget: %v", err)
	}
	// the server wrote its failure and exited within one poll: its code
	write(loadProgress{Stage: "failed", Error: "over budget", Code: exitBudget})
	gone := make(chan struct{})
	close(gone)
	if err := waitLoaded(t.Context(), path, gone, func(string) {}); exitOf(err) != exitBudget {
		t.Errorf("failed, then exited: %v", err)
	}
	// a reused, loaded instance: no catalog line, no callback
	write(loadProgress{Stage: "loaded", URL: "http://x"})
	called := false
	if err := waitLoaded(t.Context(), path, nil, func(string) { called = true }); err != nil || called {
		t.Errorf("reused and loaded: %v, callback %v", err, called)
	}
	// a reused instance of an older y-obs has no progress file
	if err := waitLoaded(t.Context(), filepath.Join(t.TempDir(), "none"), nil, func(string) {}); err != nil {
		t.Errorf("no progress file on reuse: %v", err)
	}
}

func TestWindowFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	var sf sessionFlags
	sf.register(fs)
	fs.Parse([]string{"--to", "2026-10-11 10:00", "--since", "2h", "--cluster", "c"})
	if got := windowFlags(fs); got != "--since=2h --to=2026-10-11 10:00" {
		t.Errorf("%q", got)
	}
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	sf.register(fs2)
	fs2.Parse([]string{"--cluster", "c"})
	if got := windowFlags(fs2); got != "" {
		t.Errorf("no window flags: %q", got)
	}
}
