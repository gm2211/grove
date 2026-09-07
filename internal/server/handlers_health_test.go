package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz_OK(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealthz_DegradedOnOrchardError(t *testing.T) {
	oc := &fakeOrchard{pingErr: errors.New("boom")}
	srv := newTestServer(oc, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealthz_DegradedOnNomadError(t *testing.T) {
	nc := &fakeNomad{pingErr: errors.New("boom")}
	srv := newTestServer(nil, nc, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}
