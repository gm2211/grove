package registryauth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "ghp_registrySecretToken0123456789"

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry-pull.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, path
}

func armed(t *testing.T, repos ...string) *Store {
	t.Helper()
	store, _ := newTestStore(t)
	if _, err := store.Enable(EnableOptions{User: "gm2211", Token: testToken, Repos: repos, By: "operator"}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestNewStoreStartsDisarmedAt0600(t *testing.T) {
	store, path := newTestStore(t)
	if status := store.Status(); status.Enabled {
		t.Fatalf("a fresh store must be disarmed: %+v", status)
	}
	if _, ok := store.CredentialFor("ghcr.io/gm2211/argos-ci"); ok {
		t.Fatal("a fresh store handed out a credential")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("store file = %v, %v; want mode 0600", info, err)
	}
}

func TestStoreFileIsReplacedAtomicallyWith0600(t *testing.T) {
	store, path := newTestStore(t)
	if _, err := store.Enable(EnableOptions{User: "gm2211", Token: testToken, Repos: []string{"gm2211/argos-ci"}}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("directory holds %v after an atomic write, want only the store", entries)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Status(); !got.Enabled || got.User != "gm2211" || got.TokenFingerprint != Fingerprint(testToken) {
		t.Fatalf("reopened status = %+v", got)
	}
	if _, ok := reopened.CredentialFor("ghcr.io/gm2211/argos-ci"); !ok {
		t.Fatal("the credential did not survive a reopen")
	}
}

func TestStoreRefusesASymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "store.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if _, err := NewStore(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
}

func TestStoreRefusesANonRegularFileAndTightensALooseMode(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewStore(dir); err == nil {
		t.Fatal("a directory was accepted as the store")
	}
	p := filepath.Join(dir, "store.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(p); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
}

// Foreign-owner refusal is internal/secfile's (NewStore reads through secfile.ReadFile); its
// TestOpenRefusesForeignOwner covers it. This pins that NewStore does go through it.
func TestStoreReadsThroughSecfile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"enabled":true,"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(p); err == nil || !strings.Contains(err.Error(), "no complete credential") {
		t.Fatalf("err = %v, want an incomplete-credential refusal", err)
	}
}

func TestStatusHidesTheToken(t *testing.T) {
	store := armed(t, "gm2211/argos-ci")
	status := store.Status()
	data, _ := json.Marshal(status)
	if strings.Contains(string(data), testToken) {
		t.Fatalf("Status JSON leaked the token: %s", data)
	}
	if status.TokenFingerprint != Fingerprint(testToken) || status.Registry != "ghcr.io" || status.User != "gm2211" {
		t.Fatalf("status = %+v", status)
	}
	cred, _ := store.CredentialFor("ghcr.io/gm2211/argos-ci")
	for _, text := range []string{cred.String(), cred.GoString()} {
		if strings.Contains(text, testToken) {
			t.Fatalf("Credential printed its token: %s", text)
		}
	}
}

func TestEnableValidates(t *testing.T) {
	store, _ := newTestStore(t)
	for name, opts := range map[string]EnableOptions{
		"no token":      {User: "u", Repos: []string{"a/b"}},
		"no user":       {Token: testToken, Repos: []string{"a/b"}},
		"no repos":      {User: "u", Token: testToken},
		"bad repo":      {User: "u", Token: testToken, Repos: []string{"justaname"}},
		"tag in repo":   {User: "u", Token: testToken, Repos: []string{"a/b:tag"}},
		"spaced token":  {User: "u", Token: "a b", Repos: []string{"a/b"}},
		"colon in user": {User: "u:v", Token: testToken, Repos: []string{"a/b"}},
		"url as host":   {User: "u", Token: testToken, Repos: []string{"a/b"}, Registry: "https://ghcr.io"},
		"huge token":    {User: "u", Token: strings.Repeat("x", 600), Repos: []string{"a/b"}},
		"nested repo":   {User: "u", Token: testToken, Repos: []string{"a/b/c"}},
	} {
		if _, err := store.Enable(opts); err == nil {
			t.Errorf("%s: Enable accepted %+v", name, opts)
		}
	}
	if store.Status().Enabled {
		t.Fatal("a refused Enable armed the store")
	}
}

func TestDisableWipesTheTokenFromDisk(t *testing.T) {
	store, path := newTestStore(t)
	if _, err := store.Enable(EnableOptions{User: "gm2211", Token: testToken, Repos: []string{"gm2211/*"}}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), testToken) {
		t.Fatal("test setup: token not on disk")
	}
	if _, err := store.Disable(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), testToken) {
		t.Fatalf("disarmed store still holds the token: %s", data)
	}
	if _, ok := store.CredentialFor("ghcr.io/gm2211/argos-ci"); ok {
		t.Fatal("a disarmed store handed out a credential")
	}
}

func TestCredentialForScope(t *testing.T) {
	store := armed(t, "gm2211/argos-ci", "acme/*")
	for image, want := range map[string]bool{
		"ghcr.io/gm2211/argos-ci":                                   true,
		"ghcr.io/gm2211/argos-ci:v1":                                true,
		"ghcr.io/GM2211/Argos-CI@sha256:" + strings.Repeat("a", 64): true,
		"ghcr.io/acme/anything":                                     true,
		"ghcr.io/gm2211/other":                                      false,
		"ghcr.io/acme-evil/x":                                       false,
		"docker.io/gm2211/argos-ci":                                 false,
		"gm2211/argos-ci":                                           false,
		"":                                                          false,
	} {
		if _, got := store.CredentialFor(image); got != want {
			t.Errorf("CredentialFor(%q) = %v, want %v", image, got, want)
		}
	}
}

func TestPullConfig(t *testing.T) {
	store := armed(t, "gm2211/argos-ci")
	cfg, fp := store.PullConfig([]string{"ghcr.io/gm2211/grove-runner:latest", "ghcr.io/gm2211/argos-ci"})
	var parsed struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatal(err)
	}
	if raw, _ := base64.StdEncoding.DecodeString(parsed.Auths["ghcr.io"].Auth); string(raw) != "gm2211:"+testToken {
		t.Fatalf("auth = %q", raw)
	}

	empty, emptyFP := store.PullConfig([]string{"ghcr.io/gm2211/grove-runner:latest"})
	if empty != EmptyDockerConfig || emptyFP == fp {
		t.Fatalf("an out-of-scope pool must get the empty config with its own fingerprint: %q %q", empty, emptyFP)
	}
	// Prefix entries (environments.allow ending in "/") count when they overlap the scope.
	if got, _ := store.PullConfig([]string{"ghcr.io/gm2211/"}); got == EmptyDockerConfig {
		t.Error("an allow prefix over the credential's repository got no login")
	}
	if got, _ := store.PullConfig([]string{"ghcr.io/other/"}); got != EmptyDockerConfig {
		t.Error("an allow prefix outside the credential's scope got a login")
	}
	if got, _ := store.PullConfig([]string{"docker.io/gm2211/argos-ci"}); got != EmptyDockerConfig {
		t.Error("another registry's image got a login")
	}

	// A rotated token changes the fingerprint; the same one does not.
	_, same := store.PullConfig([]string{"ghcr.io/gm2211/argos-ci"})
	if same != fp {
		t.Error("the fingerprint changed with nothing")
	}
	if _, err := store.Enable(EnableOptions{User: "gm2211", Token: testToken + "2", Repos: []string{"gm2211/argos-ci"}}); err != nil {
		t.Fatal(err)
	}
	if _, rotated := store.PullConfig([]string{"ghcr.io/gm2211/argos-ci"}); rotated == fp {
		t.Error("a rotated token kept the fingerprint")
	}

	if _, err := store.Disable(); err != nil {
		t.Fatal(err)
	}
	if got, gotFP := store.PullConfig([]string{"ghcr.io/gm2211/argos-ci"}); got != EmptyDockerConfig || gotFP != emptyFP {
		t.Errorf("disarmed config = %q", got)
	}

	var nilStore *Store
	if got, _ := nilStore.PullConfig([]string{"ghcr.io/x/y"}); got != EmptyDockerConfig {
		t.Error("a nil store must read as off")
	}
}

func TestSplitImage(t *testing.T) {
	for in, want := range map[string][3]any{
		"ghcr.io/gm2211/argos-ci:v1": {"ghcr.io", "gm2211/argos-ci", true},
		"ghcr.io/gm2211/argos-ci":    {"ghcr.io", "gm2211/argos-ci", true},
		"127.0.0.1:5000/a/b:tag":     {"127.0.0.1:5000", "a/b", true},
		"127.0.0.1:5000/a/b":         {"127.0.0.1:5000", "a/b", true},
		"ghcr.io/a/b@sha256:abc":     {"ghcr.io", "a/b", true},
		"alpine:3":                   {"", "", false},
		"gm2211/argos-ci":            {"", "", false},
		"ghcr.io/":                   {"", "", false},
	} {
		host, path, ok := SplitImage(in)
		if host != want[0] || path != want[1] || ok != want[2] {
			t.Errorf("SplitImage(%q) = %q, %q, %v; want %v", in, host, path, ok, want)
		}
	}
}

func TestUpdateScopeChangesOnlyTheRepos(t *testing.T) {
	store := armed(t, "gm2211/argos-ci")
	before := store.Status()
	st, err := store.UpdateScope(ScopeRequest{Add: []string{"GM2211/grove-macos-worker", "gm2211/argos-ci"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Repos) != 2 || st.TokenFingerprint != before.TokenFingerprint || st.User != before.User || st.EnabledAt == nil || !st.EnabledAt.Equal(*before.EnabledAt) {
		t.Fatalf("after add: %+v", st)
	}
	if _, ok := store.CredentialFor("ghcr.io/gm2211/grove-macos-worker"); !ok {
		t.Fatal("added repo not covered")
	}
	if st, err = store.UpdateScope(ScopeRequest{Remove: []string{"gm2211/argos-ci"}}); err != nil || len(st.Repos) != 1 {
		t.Fatalf("remove: %+v %v", st, err)
	}
	if _, err := store.UpdateScope(ScopeRequest{Remove: []string{"gm2211/grove-macos-worker"}}); err == nil {
		t.Fatal("emptying the scope must be refused")
	}
	if _, err := store.UpdateScope(ScopeRequest{}); err == nil {
		t.Fatal("an empty edit must be refused")
	}
	if _, err := store.UpdateScope(ScopeRequest{Add: []string{"bad"}}); err == nil {
		t.Fatal("a malformed repo must be refused")
	}
	// It survives a reload, token intact.
	reloaded, err := NewStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.CredentialFor("ghcr.io/gm2211/grove-macos-worker"); !ok {
		t.Fatal("scope edit not persisted")
	}
	disarmed, _ := newTestStore(t)
	if _, err := disarmed.UpdateScope(ScopeRequest{Add: []string{"a/b"}}); err == nil {
		t.Fatal("a disarmed store has no scope to edit")
	}
}

func TestLoginForVMImages(t *testing.T) {
	store := armed(t, "gm2211/grove-macos-worker")
	in := "ghcr.io/gm2211/grove-macos-worker@sha256:" + strings.Repeat("a", 64)
	if c, ok := store.LoginForVMImages([]string{in}); !ok || c.Token != testToken {
		t.Fatalf("in scope: %v %v", c, ok)
	}
	for name, images := range map[string][]string{
		"none":                  nil,
		"repo outside scope":    {"ghcr.io/gm2211/other"},
		"another host mixed in": {in, "registry.example.com/x/y"},
		// SplitImage rejects a dotless, colonless host, but Tart still treats "internalreg/foo" as
		// remote and would present the login to "internalreg".
		"unparsable remote name mixed in": {in, "internalreg/foo"},
	} {
		if c, ok := store.LoginForVMImages(images); ok || c.Token != "" {
			t.Errorf("%s: handed out %v", name, c)
		}
	}
	// Local VM names (no registry host) are ignored, whether alone or beside an in-scope image...
	if c, ok := store.LoginForVMImages([]string{"grove-studio-linux-v2", in}); !ok || c.Token != testToken {
		t.Errorf("local VM name beside an in-scope image: %v %v", c, ok)
	}
	// ...but cover nothing on their own.
	if _, ok := store.LoginForVMImages([]string{"grove-studio-linux-v2"}); ok {
		t.Error("a local VM name alone handed out a login")
	}
	var nilStore *Store
	if _, ok := nilStore.LoginForVMImages([]string{in}); ok {
		t.Error("a nil store handed out a login")
	}
}
