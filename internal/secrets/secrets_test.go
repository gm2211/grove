package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, path
}

func TestSetListNeverReturnsValue(t *testing.T) {
	s, path := newStore(t)
	got, err := s.Set("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-value", []string{"tag:argos", " tag:argos ", "dev_1"}, "operator")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got.Fingerprint == "" || strings.Join(got.Allow, ",") != "dev_1,tag:argos" {
		t.Fatalf("Set returned %+v", got)
	}
	list := s.List()
	if len(list) != 1 || list[0].Name != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("List = %+v", list)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode = %v, want 0600", info.Mode().Perm())
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	values, err := reloaded.Resolve([]string{"CLAUDE_CODE_OAUTH_TOKEN"})
	if err != nil || values["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-value" {
		t.Fatalf("Resolve after reload = %v, %v", values, err)
	}
}

func TestSetKeepsMultiLineValues(t *testing.T) {
	s, _ := newStore(t)
	value := "{\n  \"tokens\": {\"access_token\": \"x\"}\n}"
	if _, err := s.Set("CODEX_AUTH_JSON", value, nil, "operator"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	values, _ := s.Resolve([]string{"CODEX_AUTH_JSON"})
	if values["CODEX_AUTH_JSON"] != value {
		t.Fatalf("value = %q", values["CODEX_AUTH_JSON"])
	}
}

func TestSetRejectsBadInput(t *testing.T) {
	s, _ := newStore(t)
	cases := []struct{ name, value string }{
		{"has-dash", "v"}, {"1LEADING", "v"}, {"PATH", "v"}, {"NOMAD_TOKEN", "v"}, {"GROVE_X", "v"},
		{"OK", ""}, {"OK", "a\x00b"}, {"OK", strings.Repeat("x", MaxValueLen+1)},
	}
	for _, c := range cases {
		if _, err := s.Set(c.name, c.value, nil, "op"); err == nil {
			t.Errorf("Set(%q, len %d) accepted", c.name, len(c.value))
		}
	}
	if _, err := s.Set("OK", "v", []string{"two words"}, "op"); err == nil {
		t.Error("accepted an allow entry with a space")
	}
	if len(s.List()) != 0 {
		t.Fatal("a rejected Set stored something")
	}
}

func TestAuthorize(t *testing.T) {
	s, _ := newStore(t)
	mustSet := func(name string, allow ...string) {
		if _, err := s.Set(name, "v", allow, "op"); err != nil {
			t.Fatal(err)
		}
	}
	mustSet("FOR_ARGOS", "tag:argos")
	mustSet("FOR_DEVICE", "dev_1")
	mustSet("OPERATOR_ONLY")

	argos := Caller{ID: "tailscale:nABC", Tags: []string{"tag:argos"}}
	device := Caller{ID: "dev_1"}
	operator := Caller{ID: "operator", Operator: true}

	if err := s.Authorize([]string{"FOR_ARGOS"}, argos); err != nil {
		t.Errorf("tag grant: %v", err)
	}
	if err := s.Authorize([]string{"FOR_DEVICE"}, device); err != nil {
		t.Errorf("id grant: %v", err)
	}
	if err := s.Authorize([]string{"FOR_ARGOS", "FOR_DEVICE", "OPERATOR_ONLY"}, operator); err != nil {
		t.Errorf("operator: %v", err)
	}
	for _, c := range []struct {
		names []string
		who   Caller
	}{
		{[]string{"FOR_DEVICE"}, argos},
		{[]string{"FOR_ARGOS"}, device},
		{[]string{"OPERATOR_ONLY"}, argos},
		{[]string{"FOR_ARGOS", "OPERATOR_ONLY"}, argos},
		{[]string{"FOR_ARGOS"}, Caller{ID: "tag:argos"}},
	} {
		if err := s.Authorize(c.names, c.who); err == nil {
			t.Errorf("Authorize(%v, %+v) allowed", c.names, c.who)
		}
	}
	// A non-operator can't tell a missing secret from one it isn't granted.
	errMissing := s.Authorize([]string{"NOPE"}, argos)
	errDenied := s.Authorize([]string{"FOR_DEVICE"}, argos)
	if errMissing == nil || errDenied == nil ||
		strings.Replace(errMissing.Error(), "NOPE", "X", 1) != strings.Replace(errDenied.Error(), "FOR_DEVICE", "X", 1) {
		t.Errorf("missing vs denied differ: %v / %v", errMissing, errDenied)
	}
	if err := s.Authorize([]string{"NOPE"}, operator); err == nil {
		t.Error("operator authorized a secret that doesn't exist")
	}
}

func TestDeleteWipesValue(t *testing.T) {
	s, path := newStore(t)
	if _, err := s.Set("TOKEN", "wipe-me", nil, "op"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("TOKEN"); err != ErrNotFound {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "wipe-me") {
		t.Fatal("deleted value still on disk")
	}
}

func TestNilStore(t *testing.T) {
	var s *Store
	if err := s.Authorize([]string{"X"}, Caller{Operator: true}); err == nil {
		t.Error("nil store authorized")
	}
	if _, err := s.Resolve([]string{"X"}); err == nil {
		t.Error("nil store resolved")
	}
	if len(s.List()) != 0 {
		t.Error("nil store listed something")
	}
}
