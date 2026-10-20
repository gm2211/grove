package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz_OK(t *testing.T) {
	srv := newTestServer(nil, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealthz_DegradedOnOrchardError(t *testing.T) {
	oc := &fakeOrchard{pingErr: errors.New("boom")}
	srv := newTestServer(oc, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealthz_BodyShape_Healthy(t *testing.T) {
	srv := newTestServer(nil, nil, Options{Version: "v1.2.3"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var got HealthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK {
		t.Errorf("OK = false, want true")
	}
	if got.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", got.Version)
	}
	if got.Orchard != "up" {
		t.Errorf("Orchard=%q, want up", got.Orchard)
	}
	if got.ServerTime == "" {
		t.Errorf("ServerTime is empty")
	}
}

func TestHealthz_BodyShape_Degraded(t *testing.T) {
	oc := &fakeOrchard{pingErr: errors.New("boom")}
	srv := newTestServer(oc, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var got HealthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OK {
		t.Errorf("OK = true, want false")
	}
	if got.Orchard != "down" {
		t.Errorf("Orchard=%q, want down", got.Orchard)
	}
	if strings.Contains(rec.Body.String(), "nomad") {
		t.Errorf("healthz still reports nomad: %s", rec.Body.String())
	}
}
