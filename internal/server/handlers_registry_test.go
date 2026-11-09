package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/registryauth"
)

func newRegistryAuthServer(t *testing.T, opts Options) (*Server, *registryauth.Store) {
	t.Helper()
	store, err := registryauth.NewStore(filepath.Join(t.TempDir(), "registry-pull.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	opts.RegistryAuth = store
	return newTestServer(nil, nil, nil, opts), store
}

func decodeRegistryStatus(t *testing.T, rec *httptest.ResponseRecorder) registryauth.Status {
	t.Helper()
	var status registryauth.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status from %q: %v", rec.Body.String(), err)
	}
	return status
}

const registryBody = `{"user":"gm2211","token":"ghp_registrySecret","repos":["gm2211/argos-ci"]}`

func TestRegistryPull_OffByDefault(t *testing.T) {
	srv, _ := newRegistryAuthServer(t, Options{})
	rec := do(t, srv, http.MethodGet, "/api/v1/registry/pull", "", "")
	if rec.Code != http.StatusOK || decodeRegistryStatus(t, rec).Enabled {
		t.Fatalf("status = %d, body=%s; want 200 and off", rec.Code, rec.Body.String())
	}
}

func TestRegistryPull_EnableThenDisable_NeverEchoesTheToken(t *testing.T) {
	srv, store := newRegistryAuthServer(t, Options{})

	rec := do(t, srv, http.MethodPut, "/api/v1/registry/pull", registryBody, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ghp_registrySecret") {
		t.Fatalf("the enable response echoed the token: %s", rec.Body.String())
	}
	status := decodeRegistryStatus(t, rec)
	if !status.Enabled || status.Registry != "ghcr.io" || status.User != "gm2211" || len(status.Repos) != 1 ||
		status.TokenFingerprint != registryauth.Fingerprint("ghp_registrySecret") {
		t.Fatalf("unexpected status %+v", status)
	}
	if _, ok := store.CredentialFor("ghcr.io/gm2211/argos-ci"); !ok {
		t.Fatal("the store was not armed")
	}

	rec = do(t, srv, http.MethodGet, "/api/v1/registry/pull", "", "")
	if strings.Contains(rec.Body.String(), "ghp_registrySecret") || strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("GET exposed the token: %s", rec.Body.String())
	}
	if got := decodeRegistryStatus(t, rec); got.TokenFingerprint == "" {
		t.Fatalf("GET after enable = %+v, want a fingerprint", got)
	}

	rec = do(t, srv, http.MethodDelete, "/api/v1/registry/pull", "", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "ghp_registrySecret") {
		t.Fatalf("disable status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.CredentialFor("ghcr.io/gm2211/argos-ci"); ok {
		t.Fatal("the store stayed armed after DELETE")
	}
}

func TestRegistryPull_RejectsBadBody(t *testing.T) {
	srv, store := newRegistryAuthServer(t, Options{})
	for name, body := range map[string]string{
		"not json": `{`,
		"no token": `{"user":"gm2211","repos":["gm2211/argos-ci"]}`,
		"no user":  `{"token":"ghp_registrySecret","repos":["gm2211/argos-ci"]}`,
		"no repos": `{"user":"gm2211","token":"ghp_registrySecret"}`,
		"bad repo": `{"user":"gm2211","token":"ghp_registrySecret","repos":["argos-ci"]}`,
	} {
		rec := do(t, srv, http.MethodPut, "/api/v1/registry/pull", body, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body=%s", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "ghp_registrySecret") {
			t.Fatalf("%s: the error echoed the token: %s", name, rec.Body.String())
		}
	}
	if store.Status().Enabled {
		t.Fatal("a rejected request armed the store")
	}
}

func TestRegistryPull_NotConfigured(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if rec := do(t, srv, method, "/api/v1/registry/pull", registryBody, ""); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s with no store: status = %d, want 503", method, rec.Code)
		}
	}
}

// Everything under /registry/ is operator-only, reads included.
func TestRegistryPull_RequiresOperatorScope(t *testing.T) {
	store, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewAccessStore: %v", err)
	}
	dispatcherToken, _, err := store.Issue("laptop", []string{ScopeDispatch})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	srv, _ := newRegistryAuthServer(t, Options{Token: "operator-secret", AccessStore: store})

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if rec := do(t, srv, method, "/api/v1/registry/pull", registryBody, dispatcherToken); rec.Code != http.StatusForbidden {
			t.Fatalf("%s as dispatch-scoped device: status = %d, want 403", method, rec.Code)
		}
		if rec := do(t, srv, method, "/api/v1/registry/pull", registryBody, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated: status = %d, want 401", method, rec.Code)
		}
		if rec := do(t, srv, method, "/api/v1/registry/pull", registryBody, "operator-secret"); rec.Code != http.StatusOK {
			t.Fatalf("%s as operator: status = %d, body=%s", method, rec.Code, rec.Body.String())
		}
	}
}
