package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gm2211/grove/internal/dispatch"
)

func newTestServer(oc *fakeOrchard, nc *fakeNomad, ds *fakeDispatch, opts Options) *Server {
	if oc == nil {
		oc = &fakeOrchard{}
	}
	if nc == nil {
		nc = &fakeNomad{}
	}
	if ds == nil {
		ds = &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	}
	return New(oc, nc, ds, nil, opts)
}

func TestAuth_TokenRequired(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{Token: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuth_WrongToken(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{Token: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuth_CorrectToken(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{Token: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuth_DeviceScopes(t *testing.T) {
	store, err := NewAccessStore(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatal(err)
	}
	readToken, _, err := store.Issue("viewer", []string{ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	dispatchToken, _, err := store.Issue("laptop", []string{ScopeRead, ScopeDispatch})
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(nil, nil, nil, Options{Token: "operator", AccessStore: store})

	request := func(method, path, token string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request(http.MethodGet, "/api/v1/jobs", readToken); got != http.StatusOK {
		t.Fatalf("read token GET status=%d", got)
	}
	if got := request(http.MethodPost, "/api/v1/jobs", readToken); got != http.StatusForbidden {
		t.Fatalf("read token POST status=%d want 403", got)
	}
	if got := request(http.MethodPost, "/api/v1/jobs", dispatchToken); got == http.StatusForbidden || got == http.StatusUnauthorized {
		t.Fatalf("dispatch token rejected with status=%d", got)
	}
	if got := request(http.MethodPost, "/api/v1/workers/example/pause", dispatchToken); got != http.StatusForbidden {
		t.Fatalf("dispatch token operator action status=%d want 403", got)
	}
}

func TestAuth_NoTokenConfiguredAllowsAll(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAuth_HealthzBypassesAuth(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{Token: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestCORS_SameOriginReflected(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Host = "grove.example.com"
	req.Header.Set("Origin", "http://grove.example.com")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://grove.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want http://grove.example.com", got)
	}
}

func TestCORS_CrossOriginNotReflected(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Host = "grove.example.com"
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestUIFallback_MountedAtRoot(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
