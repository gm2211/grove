package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccessStoreIssueAuthenticateAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "devices.json")
	store, err := NewAccessStore(path)
	if err != nil {
		t.Fatalf("NewAccessStore: %v", err)
	}
	token, principal, err := store.Issue("main laptop", []string{ScopeDispatch, ScopeRead, ScopeRead})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64 hex characters", len(token))
	}
	if got, want := principal.Scopes, []string{ScopeDispatch, ScopeRead}; !sameStrings(got, want) {
		t.Fatalf("principal scopes = %#v, want %#v", got, want)
	}
	if got, ok := store.Authenticate("wrong-token"); ok || got.ID != "" {
		t.Fatalf("wrong token authenticated: %#v, %v", got, ok)
	}
	got, ok := store.Authenticate(token)
	if !ok || got.ID != principal.ID || got.Name != principal.Name {
		t.Fatalf("Authenticate = %#v, %v; want %#v, true", got, ok, principal)
	}
	list := store.List()
	if len(list) != 1 || list[0].ID != principal.ID || list[0].LastUsedAt == nil {
		t.Fatalf("List = %#v, want one used credential", list)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("raw token persisted to access store")
	}
	var persisted accessFile
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("persisted JSON: %v", err)
	}
	if len(persisted.Credentials) != 1 || persisted.Credentials[0].TokenHash == "" {
		t.Fatalf("persisted credential missing token hash: %#v", persisted)
	}

	reloaded, err := NewAccessStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reloaded.Authenticate(token); !ok {
		t.Fatal("reloaded store rejected valid token")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("store mode = %o, want 600", got)
	}
}

func TestAccessStoreRevokePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	store, err := NewAccessStore(path)
	if err != nil {
		t.Fatalf("NewAccessStore: %v", err)
	}
	token, principal, err := store.Issue("studio", []string{ScopeRead})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := store.Revoke(principal.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := store.Authenticate(token); ok {
		t.Fatal("revoked token authenticated")
	}
	if err := store.Revoke(principal.ID); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
	reloaded, err := NewAccessStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reloaded.Authenticate(token); ok {
		t.Fatal("revoked token authenticated after reload")
	}
	if got := reloaded.List()[0].RevokedAt; got == nil {
		t.Fatal("reloaded credential missing revoked timestamp")
	}
	if err := store.Revoke("missing"); err == nil {
		t.Fatal("Revoke missing ID succeeded")
	}
}

func TestAccessStoreRejectsInvalidIssueInput(t *testing.T) {
	store, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewAccessStore: %v", err)
	}
	for _, test := range []struct {
		name   string
		scopes []string
	}{
		{"", []string{ScopeRead}},
		{"device", nil},
		{"device", []string{"admin"}},
	} {
		if _, _, err := store.Issue(test.name, test.scopes); err == nil {
			t.Fatalf("Issue(%q, %#v) succeeded", test.name, test.scopes)
		}
	}
}

func TestAccessStoreListDoesNotExposeMutableScopes(t *testing.T) {
	store, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewAccessStore: %v", err)
	}
	_, principal, err := store.Issue("laptop", []string{ScopeRead})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	principal.Scopes[0] = "tampered"
	listed := store.List()
	if listed[0].Scopes[0] != ScopeRead {
		t.Fatalf("store scope changed through returned Principal: %#v", listed[0].Scopes)
	}
	listed[0].Scopes[0] = "tampered"
	if store.List()[0].Scopes[0] != ScopeRead {
		t.Fatal("store scope changed through returned List record")
	}
}

func TestAccessStoreLastUsedAtIsRecent(t *testing.T) {
	store, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("NewAccessStore: %v", err)
	}
	token, _, err := store.Issue("device", []string{ScopeRead})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	before := time.Now().UTC()
	if _, ok := store.Authenticate(token); !ok {
		t.Fatal("Authenticate rejected valid token")
	}
	after := time.Now().UTC()
	used := store.List()[0].LastUsedAt
	if used == nil || used.Before(before) || used.After(after) {
		t.Fatalf("LastUsedAt = %v, want between %v and %v", used, before, after)
	}
}

func TestAccessStoreThrottlesLastUsedPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	store, err := NewAccessStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Issue("device", []string{ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Authenticate(token); !ok {
		t.Fatal("first Authenticate rejected valid token")
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Authenticate(token); !ok {
		t.Fatal("second Authenticate rejected valid token")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("credential store was persisted again inside throttle window: %v", info.ModTime())
	}
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
