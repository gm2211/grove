package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAccessStoreExpiringCredential(t *testing.T) {
	store, err := NewAccessStore(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.IssueExpiring("argos", []string{ScopeDispatch}, 0); err == nil {
		t.Fatal("a credential that is already expired was issued")
	}
	long, p, err := store.IssueExpiring("argos", []string{ScopeDispatch}, 180*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if p.ExpiresAt == nil || p.ExpiresAt.Sub(time.Now()) < 179*24*time.Hour {
		t.Fatalf("ExpiresAt = %v, want about 180 days out", p.ExpiresAt)
	}
	if got, ok := store.Authenticate(long); !ok || got.ExpiresAt == nil || !got.ExpiresAt.Equal(*p.ExpiresAt) {
		t.Fatalf("Authenticate = %+v, %v; want the expiry carried on the principal", got, ok)
	}
	short, _, err := store.IssueExpiring("argos", []string{ScopeDispatch}, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok := store.Authenticate(short); ok {
		t.Fatal("an expired credential authenticated")
	}
	plain, _, err := store.Issue("laptop", []string{ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := store.Authenticate(plain); !ok || got.ExpiresAt != nil {
		t.Fatalf("a credential issued without expiry: %+v, %v", got, ok)
	}

	reloaded, err := NewAccessStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Authenticate(short); ok {
		t.Fatal("expiry was lost on reload")
	}
	if _, ok := reloaded.Authenticate(long); !ok {
		t.Fatal("unexpired credential stopped working after reload")
	}
}

func TestIssueDeviceEndpoint_ExpiresIn(t *testing.T) {
	store, err := NewAccessStore(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(nil, nil, nil, Options{Token: "operator", AccessStore: store})
	do := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	for _, bad := range []string{`"soon"`, `"-1h"`, `"0s"`} {
		if rec := do(http.MethodPost, "/api/v1/access/devices", "operator", `{"name":"argos","scopes":["dispatch"],"expiresIn":`+bad+`}`); rec.Code != http.StatusBadRequest {
			t.Errorf("expiresIn %s: status %d, want 400", bad, rec.Code)
		}
	}
	rec := do(http.MethodPost, "/api/v1/access/devices", "operator", `{"name":"argos","scopes":["read","dispatch"],"expiresIn":"4320h"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body)
	}
	var out IssueDeviceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Device.ExpiresAt == nil || out.Device.ExpiresAt.Sub(time.Now()) < 4319*time.Hour {
		t.Fatalf("device = %+v", out.Device)
	}
	// Its holder sees the expiry, to ask for a new credential in time.
	who := do(http.MethodGet, "/api/v1/whoami", out.Token, "")
	var me Principal
	if err := json.Unmarshal(who.Body.Bytes(), &me); err != nil || me.ExpiresAt == nil || !me.ExpiresAt.Equal(*out.Device.ExpiresAt) {
		t.Fatalf("whoami = %d %s", who.Code, who.Body)
	}
}
