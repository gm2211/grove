package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/dispatch"
)

func TestKeychainUnlock_OperatorQueuesOnlyThatWorkersBrokerCollectsOnce(t *testing.T) {
	access, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(nil, nil, &fakeDispatch{jobs: map[string]*dispatch.Job{}}, Options{Token: "operator", AccessStore: access})
	mac1, _, _ := access.Issue("mac-1", []string{ScopeBroker})
	mac2, _, _ := access.Issue("mac-2", []string{ScopeBroker})
	reader, _, _ := access.Issue("viewer", []string{ScopeRead})
	const pw = `s3cr"et pw`
	body := `{"password":"s3cr\"et pw"}`

	if rec := peerRequest(srv, http.MethodPost, "/api/v1/workers/mac-1/unlock-keychain", body, "127.0.0.1:1", reader); rec.Code != http.StatusForbidden {
		t.Fatalf("read scope queued an unlock: %d", rec.Code)
	}
	if rec := peerRequest(srv, http.MethodPost, "/api/v1/workers/mac-1/unlock-keychain", body, "127.0.0.1:1", mac1); rec.Code != http.StatusForbidden {
		t.Fatalf("a broker credential queued an unlock: %d", rec.Code)
	}
	rec := peerRequest(srv, http.MethodPost, "/api/v1/workers/mac-1/unlock-keychain", body, "127.0.0.1:1", "operator")
	if rec.Code != http.StatusAccepted || strings.Contains(rec.Body.String(), "s3cr") {
		t.Fatalf("operator queue: %d %s", rec.Code, rec.Body)
	}
	rec = peerRequest(srv, http.MethodGet, "/api/v1/workers/mac-1/unlock-keychain", "", "127.0.0.1:1", "operator")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pending":true`) || strings.Contains(rec.Body.String(), "s3cr") {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}

	// Another worker's broker gets nothing; mac-1's gets it once; then it's gone.
	if rec := peerRequest(srv, http.MethodGet, "/api/v1/broker/entries", "", "127.0.0.1:1", mac2); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "keychainUnlock") {
		t.Fatalf("mac-2 broker: %d %s", rec.Code, rec.Body)
	}
	for _, tok := range []string{"operator", reader} {
		if rec := peerRequest(srv, http.MethodGet, "/api/v1/broker/entries", "", "127.0.0.1:1", tok); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "s3cr") {
			t.Fatalf("a non-broker credential collected the password: %d %s", rec.Code, rec.Body)
		}
	}
	rec = peerRequest(srv, http.MethodGet, "/api/v1/broker/entries", "", "127.0.0.1:1", mac1)
	var got BrokerEntriesResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.KeychainUnlock != pw {
		t.Fatalf("mac-1 broker: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	if rec := peerRequest(srv, http.MethodGet, "/api/v1/broker/entries", "", "127.0.0.1:1", mac1); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "keychainUnlock") {
		t.Fatalf("collected twice: %d %s", rec.Code, rec.Body)
	}

	for name, b := range map[string]string{"empty": `{"password":""}`, "multiline": `{"password":"a\nb"}`, "junk": `nope`} {
		if rec := peerRequest(srv, http.MethodPost, "/api/v1/workers/mac-1/unlock-keychain", b, "127.0.0.1:1", "operator"); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
}
