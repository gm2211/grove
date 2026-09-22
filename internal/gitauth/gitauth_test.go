package gitauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, path
}

func TestNewStoreStartsDisarmed(t *testing.T) {
	store, path := newTestStore(t)
	if status := store.Status(); status.Enabled {
		t.Fatalf("a fresh store must be disarmed, got %+v", status)
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove"); ok {
		t.Fatal("a fresh store handed out a credential")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store mode = %o, want 600", perm)
	}
}

func TestEnableThenTokenFor(t *testing.T) {
	store, _ := newTestStore(t)
	status, err := store.Enable(EnableOptions{Token: "ghp_secret", By: "operator"})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !status.Enabled || status.EnabledBy != "operator" {
		t.Fatalf("unexpected status %+v", status)
	}
	if status.TokenFingerprint != Fingerprint("ghp_secret") {
		t.Fatalf("fingerprint = %q", status.TokenFingerprint)
	}
	if strings.Contains(mustJSON(t, status), "ghp_secret") {
		t.Fatal("Status JSON leaked the token")
	}

	for _, repo := range []string{
		"https://github.com/gm2211/grove",
		"https://github.com/gm2211/grove.git",
		"https://GitHub.com/GM2211/Grove.git",
		"https://x-access-token@github.com/gm2211/grove.git",
	} {
		token, ok := store.TokenFor(repo)
		if !ok || token != "ghp_secret" {
			t.Fatalf("TokenFor(%q) = %q, %v; want the armed token", repo, token, ok)
		}
	}
}

func TestTokenForRejectsOutOfScopeURLs(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "t"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	for _, repo := range []string{
		"git@github.com:gm2211/grove.git",       // ssh: a token does nothing here
		"ssh://git@github.com/gm2211/grove.git", // ditto
		"https://gitlab.com/gm2211/grove.git",   // host not armed
		"https://github.com/gm2211",             // not an owner/name path
		"https://github.com/",
		"",
	} {
		if token, ok := store.TokenFor(repo); ok {
			t.Fatalf("TokenFor(%q) handed out %q", repo, token)
		}
	}
}

func TestRepoScopeNarrowsTheCredential(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "t", Repos: []string{"gm2211/grove", "acme/*"}}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	allowed := []string{
		"https://github.com/gm2211/grove.git",
		"https://github.com/acme/anything",
	}
	for _, repo := range allowed {
		if _, ok := store.TokenFor(repo); !ok {
			t.Fatalf("TokenFor(%q) was refused but is in scope", repo)
		}
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/other"); ok {
		t.Fatal("an out-of-scope repo got the credential")
	}
}

func TestEnterpriseHostScope(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "t", Hosts: []string{"GitHub.example.com"}}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, ok := store.TokenFor("https://github.example.com/gm2211/grove.git"); !ok {
		t.Fatal("the armed enterprise host was refused")
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove.git"); ok {
		t.Fatal("github.com got a credential armed only for an enterprise host")
	}
}

func TestDisableWipesTheTokenFromDisk(t *testing.T) {
	store, path := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "ghp_secret"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, err := store.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if status := store.Status(); status.Enabled || status.TokenFingerprint != "" {
		t.Fatalf("store still armed after Disable: %+v", status)
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove"); ok {
		t.Fatal("a disabled store handed out a credential")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(data), "ghp_secret") {
		t.Fatalf("Disable left the token on disk: %s", data)
	}
}

func TestTTLLapseDisarmsAndWipes(t *testing.T) {
	store, path := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "ghp_secret", TTL: 30 * time.Millisecond}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove"); !ok {
		t.Fatal("credential refused while still inside its TTL")
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := store.TokenFor("https://github.com/gm2211/grove"); ok {
		t.Fatal("a lapsed credential was handed out")
	}
	if status := store.Status(); status.Enabled {
		t.Fatalf("a lapsed store reports enabled: %+v", status)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(data), "ghp_secret") {
		t.Fatalf("a lapsed TTL left the token on disk: %s", data)
	}
}

func TestEnableRejectsBadInput(t *testing.T) {
	store, _ := newTestStore(t)
	cases := map[string]EnableOptions{
		"empty token":        {Token: "   "},
		"token with spaces":  {Token: "gh p_secret"},
		"token with newline": {Token: "ghp_secret\n\nmore"},
		"oversized token":    {Token: strings.Repeat("x", maxTokenLen+1)},
		"host with scheme":   {Token: "t", Hosts: []string{"https://github.com"}},
		"repo without owner": {Token: "t", Repos: []string{"grove"}},
		"negative ttl":       {Token: "t", TTL: -time.Hour},
	}
	for name, opts := range cases {
		if _, err := store.Enable(opts); err == nil {
			t.Fatalf("%s: Enable accepted %+v", name, opts)
		}
	}
	if status := store.Status(); status.Enabled {
		t.Fatalf("a rejected Enable armed the store: %+v", status)
	}
}

func TestStateSurvivesReload(t *testing.T) {
	store, path := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "ghp_secret", Repos: []string{"gm2211/grove"}, By: "operator"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}
	token, ok := reloaded.TokenFor("https://github.com/gm2211/grove")
	if !ok || token != "ghp_secret" {
		t.Fatalf("reloaded store lost its credential: %q %v", token, ok)
	}
	if status := reloaded.Status(); !status.Enabled || len(status.Repos) != 1 {
		t.Fatalf("reloaded status = %+v", status)
	}
}

func TestNilStoreIsOff(t *testing.T) {
	var store *Store
	if status := store.Status(); status.Enabled {
		t.Fatal("a nil store reports enabled")
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove"); ok {
		t.Fatal("a nil store handed out a credential")
	}
	if _, err := store.Enable(EnableOptions{Token: "t"}); err == nil {
		t.Fatal("a nil store accepted Enable")
	}
	if _, err := store.Disable(); err == nil {
		t.Fatal("a nil store accepted Disable")
	}
}

func TestEnableRequestOptionsParsesTTL(t *testing.T) {
	opts, err := EnableRequest{Token: "t", TTL: "90m"}.Options("operator")
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if opts.TTL != 90*time.Minute || opts.By != "operator" {
		t.Fatalf("unexpected options %+v", opts)
	}
	if _, err := (EnableRequest{Token: "t", TTL: "soon"}).Options(""); err == nil {
		t.Fatal("Options accepted an unparseable ttl")
	}
	if _, err := (EnableRequest{Token: "t", TTL: "-1h"}).Options(""); err == nil {
		t.Fatal("Options accepted a negative ttl")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}
