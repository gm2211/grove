package gitauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/grant"
)

// operator is the caller every scope test asks as: an operator may always clone, so what those
// tests see is the host/repo scope alone.
var operator = grant.Caller{ID: "operator", Operator: true}

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
	if _, ok := store.TokenFor("https://github.com/gm2211/grove", operator); ok {
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
	status, err := store.Enable(EnableOptions{Token: "ghp_secret", Repos: []string{"gm2211/*"}, By: "operator"})
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
		token, ok := store.TokenFor(repo, operator)
		if !ok || token != "ghp_secret" {
			t.Fatalf("TokenFor(%q, operator) = %q, %v; want the armed token", repo, token, ok)
		}
	}
}

func TestTokenForRejectsOutOfScopeURLs(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "t", Repos: []string{"gm2211/*"}}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	for _, repo := range []string{
		"git@github.com:gm2211/grove.git",       // ssh: a token does nothing here
		"ssh://git@github.com/gm2211/grove.git", // ditto
		"https://gitlab.com/gm2211/grove.git",   // host not armed
		"http://github.com/gm2211/grove.git",    // plain http would send the token in the clear
		"https://github.com/gm2211",             // not an owner/name path
		"https://github.com/",
		"",
	} {
		if token, ok := store.TokenFor(repo, operator); ok {
			t.Fatalf("TokenFor(%q, operator) handed out %q", repo, token)
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
		if _, ok := store.TokenFor(repo, operator); !ok {
			t.Fatalf("TokenFor(%q, operator) was refused but is in scope", repo)
		}
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/other", operator); ok {
		t.Fatal("an out-of-scope repo got the credential")
	}
}

func TestEnterpriseHostScope(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "t", Hosts: []string{"GitHub.example.com"}, Repos: []string{"gm2211/grove"}}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, ok := store.TokenFor("https://github.example.com/gm2211/grove.git", operator); !ok {
		t.Fatal("the armed enterprise host was refused")
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove.git", operator); ok {
		t.Fatal("github.com got a credential armed only for an enterprise host")
	}
}

func TestDisableWipesTheTokenFromDisk(t *testing.T) {
	store, path := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "ghp_secret", Repos: []string{"gm2211/grove"}}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, err := store.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if status := store.Status(); status.Enabled || status.TokenFingerprint != "" {
		t.Fatalf("store still armed after Disable: %+v", status)
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove", operator); ok {
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
	if _, err := store.Enable(EnableOptions{Token: "ghp_secret", Repos: []string{"gm2211/grove"}, TTL: 30 * time.Millisecond}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove", operator); !ok {
		t.Fatal("credential refused while still inside its TTL")
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := store.TokenFor("https://github.com/gm2211/grove", operator); ok {
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
		"negative ttl":       {Token: "t", Repos: []string{"gm2211/grove"}, TTL: -time.Hour},
		"ttl over the max":   {Token: "t", Repos: []string{"gm2211/grove"}, TTL: MaxTTL + time.Second},
		"allow with a comma": {Token: "t", Repos: []string{"gm2211/grove"}, Allow: []string{"a,b"}},
		"allow empty tag":    {Token: "t", Repos: []string{"gm2211/grove"}, Allow: []string{"tag:"}},
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
	token, ok := reloaded.TokenFor("https://github.com/gm2211/grove", operator)
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
	if _, ok := store.TokenFor("https://github.com/gm2211/grove", operator); ok {
		t.Fatal("a nil store handed out a credential")
	}
	if _, err := store.Enable(EnableOptions{Token: "t", Repos: []string{"gm2211/*"}}); err == nil {
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

// Arming must name its repositories: a credential for everything the token can see would hand
// every job a key to all of them.
func TestEnableRequiresRepos(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "ghp_secret"}); err == nil {
		t.Fatal("Enable armed a credential with no repositories named")
	}
	if status := store.Status(); status.Enabled {
		t.Fatalf("a refused Enable left the store armed: %+v", status)
	}
}

// A credential an older grove armed with no repositories named covers nothing now.
func TestLegacyUnscopedCredentialIsInert(t *testing.T) {
	if repoAllowed("gm2211/grove", nil) {
		t.Fatal("an empty repo scope still covers every repository")
	}
}

// M3: a dispatch credential alone is not enough. Only an operator, or a principal the armed
// allow list names (by id or by Tailscale tag), gets the token.
func TestTokenForChecksTheAllowList(t *testing.T) {
	const repo = "https://github.com/gm2211/grove"
	store, _ := newTestStore(t)
	status, err := store.Enable(EnableOptions{
		Token: "ghp_secret", Repos: []string{"gm2211/grove"},
		Allow: []string{" tag:argos ", "dev_laptop", "dev_laptop"},
	})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if got := strings.Join(status.Allow, ","); got != "dev_laptop,tag:argos" {
		t.Fatalf("status.Allow = %q", got)
	}
	if status.Legacy || status.Warning != "" {
		t.Fatalf("a freshly armed credential is marked legacy: %+v", status)
	}
	for name, tc := range map[string]struct {
		caller grant.Caller
		want   bool
	}{
		"operator":                {grant.Caller{ID: "operator", Operator: true}, true},
		"granted by id":           {grant.Caller{ID: "dev_laptop"}, true},
		"granted by tag":          {grant.Caller{ID: "tailscale:nARGOS", Tags: []string{"tag:argos"}}, true},
		"worker Mac's credential": {grant.Caller{ID: "dev_worker"}, false},
		"another tag":             {grant.Caller{ID: "tailscale:nX", Tags: []string{"tag:ci"}}, false},
		"an id that reads as tag": {grant.Caller{ID: "tag:argos"}, false},
		"nobody":                  {grant.Caller{}, false},
	} {
		token, ok := store.TokenFor(repo, tc.caller)
		if ok != tc.want || (ok && token != "ghp_secret") || (!ok && token != "") {
			t.Fatalf("%s: TokenFor = %q, %v; want ok=%v", name, token, ok, tc.want)
		}
	}
}

// No allow list means operators only, which is what a newly armed credential gets by default.
func TestEmptyAllowListIsOperatorOnly(t *testing.T) {
	const repo = "https://github.com/gm2211/grove"
	store, path := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "ghp_secret", Repos: []string{"gm2211/grove"}}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, ok := store.TokenFor(repo, grant.Caller{ID: "dev_worker"}); ok {
		t.Fatal("a dispatcher no allow list names got the token")
	}
	if _, ok := store.TokenFor(repo, operator); !ok {
		t.Fatal("an operator was refused")
	}
	// And it stays that way across a restart: this is a version 2 file, not a legacy one.
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}
	if _, ok := reloaded.TokenFor(repo, grant.Caller{ID: "dev_worker"}); ok {
		t.Fatal("a reload turned an operator-only credential into one for everybody")
	}
	if status := reloaded.Status(); status.Legacy || status.Warning != "" {
		t.Fatalf("reloaded status = %+v", status)
	}
}

// A refused submitter's use doesn't count as a use.
func TestRefusedCallerLeavesNoTrace(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Enable(EnableOptions{Token: "ghp_secret", Repos: []string{"gm2211/grove"}}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	store.TokenFor("https://github.com/gm2211/grove", grant.Caller{ID: "dev_worker"})
	if status := store.Status(); status.LastUsedAt != nil {
		t.Fatalf("a refused request set LastUsedAt: %+v", status)
	}
}

// Nothing is armed forever: no TTL means DefaultTTL, an explicit one may be longer, up to MaxTTL.
func TestEnableAlwaysSetsAnExpiry(t *testing.T) {
	store, _ := newTestStore(t)
	before := time.Now()
	status, err := store.Enable(EnableOptions{Token: "t", Repos: []string{"gm2211/grove"}})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if status.ExpiresAt == nil {
		t.Fatal("a credential was armed with no expiry")
	}
	if d := status.ExpiresAt.Sub(before); d < DefaultTTL-time.Minute || d > DefaultTTL+time.Minute {
		t.Fatalf("default expiry is %s away, want %s", d, DefaultTTL)
	}
	status, err = store.Enable(EnableOptions{Token: "t", Repos: []string{"gm2211/grove"}, TTL: 72 * time.Hour})
	if err != nil {
		t.Fatalf("Enable with an explicit longer ttl: %v", err)
	}
	if d := status.ExpiresAt.Sub(before); d < 71*time.Hour {
		t.Fatalf("explicit expiry is %s away, want 72h", d)
	}
}

func writeLegacyStore(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The one transitional rule: a credential an older grove armed (a version 1 file: no allow list,
// maybe no expiry) keeps working for any submitter, with a warning, for at most LegacyGrace after
// this grove first loads it. The deadline is persisted, so a restart doesn't extend it.
func TestLegacyCredentialGetsABoundedGracePeriod(t *testing.T) {
	const repo = "https://github.com/gm2211/grove"
	path := writeLegacyStore(t, `{"version":1,"enabled":true,"token":"ghp_old","hosts":["github.com"],"repos":["gm2211/grove"]}`)
	before := time.Now()
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	status := store.Status()
	if !status.Enabled || !status.Legacy || status.Warning == "" {
		t.Fatalf("legacy status = %+v", status)
	}
	if status.ExpiresAt == nil {
		t.Fatal("an armed-forever credential still has no expiry")
	}
	if d := status.ExpiresAt.Sub(before); d < LegacyGrace-time.Minute || d > LegacyGrace+time.Minute {
		t.Fatalf("legacy deadline is %s away, want %s", d, LegacyGrace)
	}
	if strings.Contains(mustJSON(t, status), "ghp_old") {
		t.Fatal("Status JSON leaked the token")
	}
	if token, ok := store.TokenFor(repo, grant.Caller{ID: "dev_worker"}); !ok || token != "ghp_old" {
		t.Fatalf("legacy credential refused during its grace period: %q %v", token, ok)
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/other", grant.Caller{ID: "dev_worker"}); ok {
		t.Fatal("the legacy rule widened the repo scope")
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}
	again := reloaded.Status()
	if !again.Legacy || again.ExpiresAt == nil || !again.ExpiresAt.Equal(*status.ExpiresAt) {
		t.Fatalf("a restart moved the legacy deadline: %v, was %v", again.ExpiresAt, status.ExpiresAt)
	}

	// Re-arming ends the legacy rule at once.
	if _, err := reloaded.Enable(EnableOptions{Token: "ghp_new", Repos: []string{"gm2211/grove"}}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, ok := reloaded.TokenFor(repo, grant.Caller{ID: "dev_worker"}); ok {
		t.Fatal("a re-armed credential kept the legacy rule")
	}
	if status := reloaded.Status(); status.Legacy || status.Warning != "" {
		t.Fatalf("status after re-arming = %+v", status)
	}
}

// A legacy credential's own, sooner expiry is kept; a later one is cut to the grace period.
func TestLegacyCredentialKeepsASoonerExpiry(t *testing.T) {
	soon := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	path := writeLegacyStore(t, `{"version":1,"enabled":true,"token":"ghp_old","hosts":["github.com"],"repos":["gm2211/grove"],"expiresAt":"`+soon+`"}`)
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := store.Status().ExpiresAt; got == nil || time.Until(*got) > time.Hour+time.Minute {
		t.Fatalf("expiry = %v, want the credential's own (in 1h)", got)
	}

	late := time.Now().Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	path = writeLegacyStore(t, `{"version":1,"enabled":true,"token":"ghp_old","hosts":["github.com"],"repos":["gm2211/grove"],"expiresAt":"`+late+`"}`)
	if store, err = NewStore(path); err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := store.Status().ExpiresAt; got == nil || time.Until(*got) > LegacyGrace+time.Minute {
		t.Fatalf("expiry = %v, want it cut to the grace period", got)
	}
}

// After the grace period a legacy credential is gone, token and all.
func TestLegacyCredentialIsWipedAfterItsDeadline(t *testing.T) {
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	path := writeLegacyStore(t, `{"version":2,"enabled":true,"legacy":true,"token":"ghp_old","hosts":["github.com"],"repos":["gm2211/grove"],"expiresAt":"`+past+`"}`)
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, ok := store.TokenFor("https://github.com/gm2211/grove", operator); ok {
		t.Fatal("a legacy credential outlived its deadline")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghp_old") {
		t.Fatalf("the lapsed legacy token is still on disk: %s", data)
	}
}

// A disarmed version 1 file is just upgraded: nothing to carry over.
func TestDisarmedLegacyFileUpgradesQuietly(t *testing.T) {
	path := writeLegacyStore(t, `{"version":1,"enabled":false}`)
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if status := store.Status(); status.Enabled || status.Legacy || status.Warning != "" {
		t.Fatalf("status = %+v", status)
	}
}

func TestEnableRequestCarriesAllow(t *testing.T) {
	opts, err := EnableRequest{Token: "t", Allow: []string{"tag:argos"}}.Options("operator")
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if len(opts.Allow) != 1 || opts.Allow[0] != "tag:argos" || opts.TTL != 0 {
		t.Fatalf("unexpected options %+v", opts)
	}
}
