package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/secrets"
	"github.com/gm2211/grove/internal/tsauth"
)

// fakePeers authenticates by exact RemoteAddr.
type fakePeers map[string]tsauth.Result

func (f fakePeers) Authenticate(_ context.Context, remoteAddr string) (tsauth.Result, bool) {
	r, ok := f[remoteAddr]
	return r, ok
}

const argosPeer = "100.64.0.7:41000"

func tailscaleServer(t *testing.T, ds *fakeDispatch) (*Server, *secrets.Store) {
	t.Helper()
	store, err := secrets.NewStore(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	peers := fakePeers{
		argosPeer:      {ID: "tailscale:nARGOS", Name: "argos", Tags: []string{"tag:argos"}, Scopes: []string{ScopeRead, ScopeDispatch}},
		"100.64.0.8:1": {ID: "tailscale:nODD", Name: "odd", Scopes: []string{"made-up-scope"}},
		"100.64.0.9:1": {ID: "tailscale:nOP", Name: "giulio", Scopes: []string{ScopeOperator}},
	}
	return newTestServer(nil, nil, ds, Options{Token: "operator", AccessStore: access, Tailscale: peers, Secrets: store}), store
}

func peerRequest(srv *Server, method, path, body, remote, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestTailscaleIdentity_AuthenticatesWithoutToken(t *testing.T) {
	srv, _ := tailscaleServer(t, nil)
	rec := peerRequest(srv, http.MethodGet, "/api/v1/whoami", "", argosPeer, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami = %d %s", rec.Code, rec.Body)
	}
	var p Principal
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.ID != "tailscale:nARGOS" || strings.Join(p.Tags, ",") != "tag:argos" {
		t.Fatalf("principal = %+v", p)
	}
	// dispatch scope does not reach operator routes.
	if rec := peerRequest(srv, http.MethodGet, "/api/v1/access/devices", "", argosPeer, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("operator route = %d, want 403", rec.Code)
	}
}

func TestTailscaleIdentity_UnknownPeerAndUnknownScopes(t *testing.T) {
	srv, _ := tailscaleServer(t, nil)
	if rec := peerRequest(srv, http.MethodGet, "/api/v1/jobs", "", "100.64.0.99:1", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown peer = %d, want 401", rec.Code)
	}
	if rec := peerRequest(srv, http.MethodGet, "/api/v1/jobs", "", "100.64.0.8:1", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("peer granted only unknown scopes = %d, want 401", rec.Code)
	}
}

// A bad token from a tailnet peer is a 401, not a fall-through to the peer's identity.
func TestTailscaleIdentity_BadTokenDoesNotFallThrough(t *testing.T) {
	srv, _ := tailscaleServer(t, nil)
	if rec := peerRequest(srv, http.MethodGet, "/api/v1/jobs", "", argosPeer, "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", rec.Code)
	}
	if rec := peerRequest(srv, http.MethodGet, "/api/v1/jobs", "", argosPeer, "operator"); rec.Code != http.StatusOK {
		t.Fatalf("operator token from a peer = %d, want 200", rec.Code)
	}
}

func TestSecrets_OperatorManagesThem_NoValueInResponses(t *testing.T) {
	srv, _ := tailscaleServer(t, nil)
	body := `{"value":"sk-ant-oat-value","allow":["tag:argos"]}`
	if rec := peerRequest(srv, http.MethodPut, "/api/v1/secrets/CLAUDE_CODE_OAUTH_TOKEN", body, argosPeer, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-operator set = %d, want 403", rec.Code)
	}
	if rec := peerRequest(srv, http.MethodGet, "/api/v1/secrets", "", argosPeer, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-operator list = %d, want 403", rec.Code)
	}
	rec := peerRequest(srv, http.MethodPut, "/api/v1/secrets/CLAUDE_CODE_OAUTH_TOKEN", body, "100.64.0.9:1", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "sk-ant-oat-value") {
		t.Fatalf("operator set = %d %s", rec.Code, rec.Body)
	}
	rec = peerRequest(srv, http.MethodGet, "/api/v1/secrets", "", "", "operator")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "sk-ant-oat-value") ||
		!strings.Contains(rec.Body.String(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	if rec := peerRequest(srv, http.MethodPut, "/api/v1/secrets/bad-name", body, "", "operator"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name = %d, want 400", rec.Code)
	}
	if rec := peerRequest(srv, http.MethodDelete, "/api/v1/secrets/CLAUDE_CODE_OAUTH_TOKEN", "", "", "operator"); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := peerRequest(srv, http.MethodDelete, "/api/v1/secrets/CLAUDE_CODE_OAUTH_TOKEN", "", "", "operator"); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
}

func TestSubmit_NamedSecretsFollowTheirGrants(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	srv, store := tailscaleServer(t, ds)
	if _, err := store.Set("CLAUDE_CODE_OAUTH_TOKEN", "v", []string{"tag:argos"}, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("DEPLOY_KEY", "v", nil, "operator"); err != nil {
		t.Fatal(err)
	}
	job := func(secret string) string {
		return `{"kind":"agent","pool":"linux","repo":"https://github.com/o/r","script":"claude","secrets":["` + secret + `"]}`
	}
	if rec := peerRequest(srv, http.MethodPost, "/api/v1/jobs", job("CLAUDE_CODE_OAUTH_TOKEN"), argosPeer, ""); rec.Code != http.StatusCreated {
		t.Fatalf("granted secret = %d %s", rec.Code, rec.Body)
	}
	if rec := peerRequest(srv, http.MethodPost, "/api/v1/jobs", job("DEPLOY_KEY"), argosPeer, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("operator-only secret = %d, want 403", rec.Code)
	}
	if rec := peerRequest(srv, http.MethodPost, "/api/v1/jobs", job("MISSING"), argosPeer, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("missing secret = %d, want 403", rec.Code)
	}
	if rec := peerRequest(srv, http.MethodPost, "/api/v1/jobs", job("DEPLOY_KEY"), "", "operator"); rec.Code != http.StatusCreated {
		t.Fatalf("operator with operator-only secret = %d %s", rec.Code, rec.Body)
	}
	if len(ds.submitCalls) != 2 || ds.submitCalls[0].SubmittedBy != "tailscale:nARGOS" {
		t.Fatalf("submit calls = %+v", ds.submitCalls)
	}
}
