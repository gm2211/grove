package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/broker"
	"github.com/gm2211/grove/internal/dispatch"
)

func brokerEntry(t *testing.T, job, token string) broker.Entry {
	t.Helper()
	ph, err := broker.NewPlaceholder()
	if err != nil {
		t.Fatal(err)
	}
	return broker.Entry{Placeholder: ph, Job: job, Kind: broker.KindGitHub, Token: token}
}

func TestBrokerEntries_OnlyTheWorkersOwnAndOnlyBrokerScope(t *testing.T) {
	access, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}, brokerEntries: map[string][]broker.Entry{
		"mac-1": {brokerEntry(t, "job-1", "ghs_one")},
		"mac-2": {brokerEntry(t, "job-2", "ghs_two")},
	}}
	peers := fakePeers{
		"100.64.0.20:1": {ID: "tailscale:nMAC1", Name: "mac-1", Scopes: []string{ScopeBroker}},
	}
	srv := newTestServer(nil, nil, ds, Options{Token: "operator", AccessStore: access, Tailscale: peers})

	brokerTok, _, err := access.Issue("mac-1", []string{ScopeBroker})
	if err != nil {
		t.Fatal(err)
	}
	mixedTok, _, err := access.Issue("mac-1", []string{ScopeBroker, ScopeRead})
	if err != nil {
		t.Fatal(err)
	}

	rec := peerRequest(srv, http.MethodGet, "/api/v1/broker/entries", "", "127.0.0.1:1", brokerTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("broker credential: %d %s", rec.Code, rec.Body)
	}
	var resp BrokerEntriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Job != "job-1" || resp.Entries[0].Token != "ghs_one" {
		t.Fatalf("entries = %+v, want only mac-1's job-1", resp.Entries)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}

	for name, tc := range map[string]struct{ remote, token string }{
		"operator token":         {"127.0.0.1:1", "operator"},
		"broker plus read":       {"127.0.0.1:1", mixedTok},
		"tailscale broker grant": {"100.64.0.20:1", ""},
	} {
		rec := peerRequest(srv, http.MethodGet, "/api/v1/broker/entries", "", tc.remote, tc.token)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "ghs_") {
			t.Fatalf("%s: %d %s, want refused", name, rec.Code, rec.Body)
		}
	}

	// A broker credential reaches nothing else.
	for _, path := range []string{"/api/v1/jobs", "/api/v1/whoami", "/api/v1/secrets"} {
		if rec := peerRequest(srv, http.MethodGet, path, "", "127.0.0.1:1", brokerTok); rec.Code != http.StatusForbidden {
			t.Fatalf("broker credential on %s: %d, want 403", path, rec.Code)
		}
	}

	// A worker with nothing placed on it gets an empty list, not null.
	other, _, err := access.Issue("mac-3", []string{ScopeBroker})
	if err != nil {
		t.Fatal(err)
	}
	rec = peerRequest(srv, http.MethodGet, "/api/v1/broker/entries", "", "127.0.0.1:1", other)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"entries":[]}` {
		t.Fatalf("mac-3: %d %s", rec.Code, rec.Body)
	}
}

func TestUpdateJobCredentials_OwnerOrOperatorOnly(t *testing.T) {
	access, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	argosTok, argos, err := access.Issue("argos", []string{ScopeDispatch})
	if err != nil {
		t.Fatal(err)
	}
	otherTok, _, err := access.Issue("other", []string{ScopeDispatch})
	if err != nil {
		t.Fatal(err)
	}
	readTok, _, err := access.Issue("reader", []string{ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{
		"job-1": {ID: "job-1", Request: dispatch.JobRequest{SubmittedBy: argos.ID}},
	}}
	srv := newTestServer(nil, nil, ds, Options{Token: "operator", AccessStore: access})
	body := `{"credentials":[{"env":"GH_TOKEN","token":"ghs_fresh"}]}`

	for name, tc := range map[string]struct {
		token string
		want  int
	}{
		"other dispatcher": {otherTok, http.StatusForbidden},
		"read only":        {readTok, http.StatusForbidden},
		"submitter":        {argosTok, http.StatusNoContent},
		"operator":         {"operator", http.StatusNoContent},
	} {
		rec := peerRequest(srv, http.MethodPut, "/api/v1/jobs/job-1/credentials", body, "127.0.0.1:1", tc.token)
		if rec.Code != tc.want {
			t.Fatalf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}
	if got := ds.credentialUpdates["job-1"]; len(got) != 1 || got[0].Token != "ghs_fresh" {
		t.Fatalf("updates = %+v", got)
	}

	ds.credentialErr = dispatch.ErrJobEnded
	if rec := peerRequest(srv, http.MethodPut, "/api/v1/jobs/job-1/credentials", body, "127.0.0.1:1", argosTok); rec.Code != http.StatusConflict {
		t.Fatalf("ended job: %d, want 409", rec.Code)
	}
	if rec := peerRequest(srv, http.MethodPut, "/api/v1/jobs/nope/credentials", body, "127.0.0.1:1", argosTok); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job: %d, want 404", rec.Code)
	}
}
