package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/gitauth"
)

func newGitAuthServer(t *testing.T, opts Options) (*Server, *gitauth.Store) {
	t.Helper()
	store, err := gitauth.NewStore(filepath.Join(t.TempDir(), "github.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	opts.GitAuth = store
	return newTestServer(nil, nil, nil, opts), store
}

func do(t *testing.T, srv *Server, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) gitauth.Status {
	t.Helper()
	var status gitauth.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status from %q: %v", rec.Body.String(), err)
	}
	return status
}

func TestGitHubSourcing_OffByDefault(t *testing.T) {
	srv, _ := newGitAuthServer(t, Options{})
	rec := do(t, srv, http.MethodGet, "/api/v1/github/sourcing", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if status := decodeStatus(t, rec); status.Enabled {
		t.Fatalf("private-repo sourcing is on by default: %+v", status)
	}
}

func TestGitHubSourcing_EnableThenDisable(t *testing.T) {
	srv, store := newGitAuthServer(t, Options{})

	rec := do(t, srv, http.MethodPut, "/api/v1/github/sourcing",
		`{"token":"ghp_secret","repos":["gm2211/grove"],"ttl":"2h"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ghp_secret") {
		t.Fatalf("the enable response echoed the token: %s", rec.Body.String())
	}
	status := decodeStatus(t, rec)
	if !status.Enabled || status.ExpiresAt == nil || len(status.Repos) != 1 {
		t.Fatalf("unexpected status %+v", status)
	}
	if token, ok := store.TokenFor("https://github.com/gm2211/grove"); !ok || token != "ghp_secret" {
		t.Fatalf("the store was not armed: %q %v", token, ok)
	}

	rec = do(t, srv, http.MethodGet, "/api/v1/github/sourcing", "", "")
	if got := decodeStatus(t, rec); !got.Enabled || got.TokenFingerprint != gitauth.Fingerprint("ghp_secret") {
		t.Fatalf("GET after enable = %+v", got)
	}
	if strings.Contains(rec.Body.String(), "ghp_secret") {
		t.Fatalf("GET leaked the token: %s", rec.Body.String())
	}

	rec = do(t, srv, http.MethodDelete, "/api/v1/github/sourcing", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove"); ok {
		t.Fatal("the store stayed armed after DELETE")
	}
}

func TestGitHubSourcing_RejectsBadBody(t *testing.T) {
	srv, store := newGitAuthServer(t, Options{})
	for name, body := range map[string]string{
		"not json":     `{`,
		"no token":     `{"token":""}`,
		"bad ttl":      `{"token":"ghp_secret","ttl":"soon"}`,
		"bad repo":     `{"token":"ghp_secret","repos":["grove"]}`,
		"host w/ port": `{"token":"ghp_secret","hosts":["github.com:443"]}`,
	} {
		rec := do(t, srv, http.MethodPut, "/api/v1/github/sourcing", body, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body=%s", name, rec.Code, rec.Body.String())
		}
	}
	if store.Status().Enabled {
		t.Fatal("a rejected request armed the store")
	}
}

// Everything under /github/ is operator-only, reads included: the status names the repositories
// and the window the control plane is willing to clone privately.
func TestGitHubSourcing_RequiresOperatorScope(t *testing.T) {
	store, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewAccessStore: %v", err)
	}
	dispatcherToken, _, err := store.Issue("laptop", []string{ScopeDispatch})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	srv, _ := newGitAuthServer(t, Options{Token: "operator-secret", AccessStore: store})

	for _, tc := range []struct {
		method, body string
	}{
		{http.MethodGet, ""},
		{http.MethodPut, `{"token":"ghp_secret"}`},
		{http.MethodDelete, ""},
	} {
		rec := do(t, srv, tc.method, "/api/v1/github/sourcing", tc.body, dispatcherToken)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s as dispatch-scoped device: status = %d, want 403", tc.method, rec.Code)
		}
		rec = do(t, srv, tc.method, "/api/v1/github/sourcing", tc.body, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated: status = %d, want 401", tc.method, rec.Code)
		}
		rec = do(t, srv, tc.method, "/api/v1/github/sourcing", tc.body, "operator-secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s as operator: status = %d, body=%s", tc.method, rec.Code, rec.Body.String())
		}
	}
}

func TestGitHubSourcing_UnavailableWithoutAStore(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := do(t, srv, method, "/api/v1/github/sourcing", `{"token":"ghp_secret"}`, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", method, rec.Code)
		}
	}
}
