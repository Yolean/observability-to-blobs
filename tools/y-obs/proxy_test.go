package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestReadOnlyProxy(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	h, err := readOnly(&rest.Config{Host: upstream.URL}, "t1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(m, "/api/v1/pods", nil)
		req.Header.Set("Authorization", "Bearer client")
		req.Header.Set("Impersonate-User", "someone")
		req.Header.Set("Impersonate-Group", "system:masters")
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", m, rec.Code)
		}
		for k := range seen {
			if k == "Authorization" || strings.HasPrefix(k, "Impersonate-") {
				t.Errorf("%s: %s reached the API server", m, k)
			}
		}
		if seen.Get("Accept") != "application/json" {
			t.Errorf("%s: Accept %q", m, seen.Get("Accept"))
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		seen = nil
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/api/v1/namespaces", strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), `"reason":"MethodNotAllowed"`) ||
			!strings.Contains(rec.Body.String(), "at t1") {
			t.Errorf("%s: %d %s", m, rec.Code, rec.Body.String())
		}
		if seen != nil {
			t.Errorf("%s reached the API server", m)
		}
	}
}

// A table's age columns count to the state's time, not to now: Age from
// the row's creationTimestamp, Last Seen shifted by now minus at.
func TestReadOnlyProxyAgesAt(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC()
	created := at.Add(-6*time.Hour - 24*time.Minute).Format(time.RFC3339)
	table := `{"kind":"Table","apiVersion":"meta.k8s.io/v1","metadata":{},
"columnDefinitions":[{"name":"Name","type":"string"},{"name":"Age","type":"string"},{"name":"Last Seen","type":"string"},{"name":"Started","type":"date"},{"name":"Restarts","type":"integer"}],
"rows":[{"cells":["buckety-controller","7h24m","3h","<unknown>",2],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":{"name":"buckety-controller","creationTimestamp":"` + created + `"}}},
{"cells":["no-object","90m","30m","2d",0]}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" && r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("Accept-Encoding %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(table))
	}))
	defer upstream.Close()
	h, err := readOnly(&rest.Config{Host: upstream.URL}, "t1", at)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io,application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{`["buckety-controller","6h24m","120m","<unknown>",2]`, `["no-object","30m","<invalid>","47h",0]}`} {
		if !strings.Contains(body, want) {
			t.Errorf("want %s in %s", want, body)
		}
	}
	if strings.Contains(body, `"object":null`) || !strings.Contains(body, `"type":"date"}`) {
		t.Errorf("other fields changed: %s", body)
	}
	// a plain object is not touched
	if got := shiftDuration("not-a-duration", time.Hour); got != "" {
		t.Errorf("shiftDuration took %q", got)
	}
}
