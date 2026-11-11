package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/gitauth"
	"github.com/gm2211/grove/internal/grant"
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
	if token, ok := store.TokenFor("https://github.com/gm2211/grove", grant.Caller{ID: "operator", Operator: true}); !ok || token != "ghp_secret" {
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
	if _, ok := store.TokenFor("https://github.com/gm2211/grove", grant.Caller{ID: "operator", Operator: true}); ok {
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
		"no repos":     `{"token":"ghp_secret"}`,
		"ttl too long": `{"token":"ghp_secret","repos":["gm2211/grove"],"ttl":"100000h"}`,
		"bad allow":    `{"token":"ghp_secret","repos":["gm2211/grove"],"allow":["a b"]}`,
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
		{http.MethodPut, `{"token":"ghp_secret","repos":["gm2211/grove"]}`},
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

// Arming with no ttl still expires, and the status carries the allow list, never the token.
func TestGitHubSourcing_DefaultTTLAndAllowInStatus(t *testing.T) {
	srv, _ := newGitAuthServer(t, Options{})
	rec := do(t, srv, http.MethodPut, "/api/v1/github/sourcing",
		`{"token":"ghp_secret","repos":["gm2211/grove"],"allow":["tag:argos","dev_laptop"]}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", rec.Code, rec.Body.String())
	}
	for _, rec := range []*httptest.ResponseRecorder{rec, do(t, srv, http.MethodGet, "/api/v1/github/sourcing", "", "")} {
		if strings.Contains(rec.Body.String(), "ghp_secret") {
			t.Fatalf("the response carried the token: %s", rec.Body.String())
		}
		status := decodeStatus(t, rec)
		if status.ExpiresAt == nil {
			t.Fatalf("armed with no expiry: %+v", status)
		}
		if strings.Join(status.Allow, ",") != "dev_laptop,tag:argos" {
			t.Fatalf("status.Allow = %v", status.Allow)
		}
	}
}

// M3 end to end at the API: the caller dispatch is told about is the authenticated principal,
// never anything in the body, and only an operator or a principal on the allow list turns into a
// token. A worker Mac's device credential (read + dispatch) is accepted as a submitter and gets
// none; its response is the same 201 either way.
func TestSubmitJob_PrivateCloneNeedsAnAllowedPrincipal(t *testing.T) {
	const repo = "https://github.com/gm2211/grove"
	access, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewAccessStore: %v", err)
	}
	workerToken, worker, err := access.Issue("mac-mini-3", []string{ScopeRead, ScopeBuild, ScopeAgent})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	laptopToken, laptop, err := access.Issue("laptop", []string{ScopeRead, ScopeBuild})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	git, err := gitauth.NewStore(filepath.Join(t.TempDir(), "github.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	peers := fakePeers{
		argosPeer:      {ID: "tailscale:nARGOS", Name: "argos", Tags: []string{"tag:argos"}, Scopes: []string{ScopeRead, ScopeDispatch}},
		"100.64.0.5:1": {ID: "tailscale:nCI", Name: "ci", Tags: []string{"tag:ci"}, Scopes: []string{ScopeRead, ScopeDispatch}},
	}
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	srv := newTestServer(nil, nil, ds, Options{Token: "operator", AccessStore: access, Tailscale: peers, GitAuth: git})

	rec := peerRequest(srv, http.MethodPut, "/api/v1/github/sourcing",
		`{"token":"ghp_secret","repos":["gm2211/grove"],"allow":["`+laptop.ID+`","tag:argos"]}`, "100.64.0.99:1", "operator")
	if rec.Code != http.StatusOK {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body)
	}

	// The body claims to be an operator; none of it may count.
	body := `{"kind":"build","pool":"linux","repo":"` + repo + `","script":"true",` +
		`"submittedBy":"operator","caller":{"ID":"operator","Operator":true,"Tags":["tag:argos"]},"Caller":{"Operator":true}}`
	for _, tc := range []struct {
		name, remote, token string
		want                bool
	}{
		{"operator", "100.64.0.99:1", "operator", true},
		{"allowed device credential", "100.64.0.99:1", laptopToken, true},
		{"allowed tailscale tag", argosPeer, "", true},
		{"worker Mac's device credential", "100.64.0.99:1", workerToken, false},
		{"tailscale peer with another tag", "100.64.0.5:1", "", false},
	} {
		before := len(ds.submitCalls)
		rec := peerRequest(srv, http.MethodPost, "/api/v1/jobs", body, tc.remote, tc.token)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: submit = %d %s (a submitter that isn't allowed is not refused)", tc.name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "ghp_secret") || strings.Contains(rec.Body.String(), "gm2211") {
			t.Fatalf("%s: the response says something about the credential: %s", tc.name, rec.Body)
		}
		if len(ds.submitCalls) != before+1 {
			t.Fatalf("%s: dispatch saw %d submits", tc.name, len(ds.submitCalls)-before)
		}
		req := ds.submitCalls[before]
		if req.Caller.ID != req.SubmittedBy || req.Caller.ID == "" {
			t.Fatalf("%s: caller %+v does not match submittedBy %q", tc.name, req.Caller, req.SubmittedBy)
		}
		if tc.token == workerToken && (req.Caller.ID != worker.ID || req.Caller.Operator || len(req.Caller.Tags) != 0) {
			t.Fatalf("%s: the body's claims reached the caller: %+v", tc.name, req.Caller)
		}
		token, ok := git.TokenFor(req.Repo, req.Caller)
		if ok != tc.want || (ok && token != "ghp_secret") {
			t.Fatalf("%s: TokenFor as %+v = %q, %v; want ok=%v", tc.name, req.Caller, token, ok, tc.want)
		}
	}
}
