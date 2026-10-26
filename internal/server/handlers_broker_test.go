package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/broker"
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
	reg := broker.NewRegistry(0)
	if err := reg.Add("mac-1", brokerEntry(t, "job-1", "ghs_one")); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add("mac-2", brokerEntry(t, "job-2", "ghs_two")); err != nil {
		t.Fatal(err)
	}
	peers := fakePeers{
		"100.64.0.20:1": {ID: "tailscale:nMAC1", Name: "mac-1", Scopes: []string{ScopeBroker}},
	}
	srv := newTestServer(nil, nil, nil, Options{Token: "operator", AccessStore: access, Tailscale: peers, Broker: reg})

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
}

func TestBrokerEntries_NoRegistryIsEmpty(t *testing.T) {
	access, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(nil, nil, nil, Options{Token: "operator", AccessStore: access, Tailscale: fakePeers{}})
	tok, _, err := access.Issue("mac-1", []string{ScopeBroker})
	if err != nil {
		t.Fatal(err)
	}
	rec := peerRequest(srv, http.MethodGet, "/api/v1/broker/entries", "", "127.0.0.1:1", tok)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"entries":[]}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}
