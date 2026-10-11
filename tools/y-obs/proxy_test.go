package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func TestReadOnlyProxy(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	h, err := readOnly(&rest.Config{Host: upstream.URL}, "t1")
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
