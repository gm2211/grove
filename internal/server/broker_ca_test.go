package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/tsauth"
)

// A worker's broker says on every sync when it made the CA it serves, and the fleet reconciler
// rebuilds the VMs that booted before then (fleet.BrokerCAs). Only the worker's own bound broker
// credential is listened to: anything else that claimed a new CA could have idle VMs rebuilt.
func TestBrokerSyncReportsWhenTheBrokerMadeItsCA(t *testing.T) {
	h := newJoinHarness(t)
	cas := &fleet.BrokerCAs{}
	h.srv.opts.BrokerCAs = cas
	h.tail.nodes["100.64.0.20"] = tsauth.Identity{StableID: "nMAC1", Node: "mac-1.tail1234.ts.net"}
	got := h.join("mac-1", "100.64.0.20", false)

	sync := func(remote, token, made string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/broker/entries", nil)
		req.Host = "grove.tail1234.ts.net:6120"
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+token)
		if made != "" {
			req.Header.Set(BrokerCAMadeHeader, made)
		}
		rec := httptest.NewRecorder()
		h.srv.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := sync("100.64.0.20:5123", got.BrokerToken, ""); code != http.StatusOK {
		t.Fatalf("sync without the header: %d", code)
	}
	if _, ok := cas.Since("mac-1"); ok {
		t.Fatal("a sync without the header recorded a CA time")
	}
	if code := sync("100.64.0.20:5123", got.BrokerToken, "not a time"); code != http.StatusOK {
		t.Fatalf("a malformed header must not fail the sync: %d", code)
	}

	// From another machine the credential is refused, and nothing is recorded.
	h.tail.nodes["100.64.0.66"] = tsauth.Identity{StableID: "nEVIL", Node: "laptop.tail1234.ts.net"}
	if code := sync("100.64.0.66:5123", got.BrokerToken, "2026-11-22T05:24:05Z"); code != http.StatusForbidden {
		t.Fatalf("from another machine: %d, want 403", code)
	}
	if _, ok := cas.Since("mac-1"); ok {
		t.Fatal("a refused sync recorded a CA time")
	}

	if code := sync("100.64.0.20:5123", got.BrokerToken, "2026-11-22T05:24:05Z"); code != http.StatusOK {
		t.Fatalf("sync: %d", code)
	}
	if made, ok := cas.Since("mac-1"); !ok || !made.Equal(time.Date(2026, 11, 22, 5, 24, 5, 0, time.UTC)) {
		t.Fatalf("CA time = %v %v, want the header's", made, ok)
	}
}

// A broker credential from before bindings existed works from anywhere, so its claim is ignored.
func TestUnboundBrokerCredentialCannotReportACA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	const token = "legacy-token"
	legacy := `{"version":1,"credentials":[{"id":"aaaa","name":"mac-1","scopes":["broker"],"source":"join",` +
		`"createdAt":"2026-01-01T00:00:00Z","tokenHash":"` + hashToken(token) + `"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	access, err := NewAccessStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cas := &fleet.BrokerCAs{}
	srv := newTestServer(nil, nil, &fakeDispatch{}, Options{Token: "operator", AccessStore: access, BrokerCAs: cas})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/broker/entries", nil)
	req.RemoteAddr = "100.64.0.99:1"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(BrokerCAMadeHeader, "2026-11-22T05:24:05Z")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("an unbound credential still gets its entries: %d %s", rec.Code, rec.Body)
	}
	if _, ok := cas.Since("mac-1"); ok {
		t.Fatal("an unbound credential's CA time was recorded")
	}
}
