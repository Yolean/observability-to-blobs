package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// hangingTarget is a target whose endpoint never answers until the request
// is abandoned.
func hangingTarget(t *testing.T) target {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	return target{key: "test", scheme: "http", host: host, port: p, path: "/metrics"}
}

func upOf(s scrape) (float64, bool) {
	for _, r := range s.Samples {
		if r.Name == "up" {
			return r.Value, true
		}
	}
	return 0, false
}

func TestScrapeTimeoutEmitsUpZero(t *testing.T) {
	var got []scrape
	sc := newScraper(time.Minute, 50*time.Millisecond, 1<<20, func(s scrape) { got = append(got, s) })
	if err := sc.scrapeOnce(context.Background(), hangingTarget(t), "pod", time.Now()); err == nil {
		t.Fatal("expected a timeout error")
	}
	if len(got) != 1 {
		t.Fatalf("emitted %d scrapes, want 1", len(got))
	}
	if up, ok := upOf(got[0]); !ok || up != 0 {
		t.Fatalf("up = %v (present %v), want 0", up, ok)
	}
}

// A scraper whose target was replaced or removed mid-request must not
// report the target down.
func TestScrapeCancelledByParentEmitsNothing(t *testing.T) {
	var got []scrape
	sc := newScraper(time.Minute, 10*time.Second, 1<<20, func(s scrape) { got = append(got, s) })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := sc.scrapeOnce(ctx, hangingTarget(t), "pod", time.Now()); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if len(got) != 0 {
		t.Fatalf("emitted %+v after the parent was cancelled", got)
	}
}
