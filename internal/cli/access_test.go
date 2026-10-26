package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/server"
)

// `grove access issue` mints a credential through the operator API and prints ONLY the raw token
// on stdout, so `TOKEN=$(grove access issue ...)` captures it; the token authenticates with the
// requested scopes.
func TestAccessIssue_PrintsTokenOnceOnStdout(t *testing.T) {
	store, err := server.NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(nil, nil, nil, nil, server.Options{Token: "op-token", AccessStore: store}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  url: "+srv.URL+"\n  token: op-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROVE_CONFIG", cfgPath)

	cmd := newAccessIssueCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--name", "ci-bot", "--scope", "read", "--scope", "dispatch"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("issue: %v (stderr=%s)", err, stderr.String())
	}
	token := strings.TrimSpace(stdout.String())
	if strings.Contains(token, "\n") || len(token) != 64 {
		t.Fatalf("stdout = %q, want exactly one 64-hex token", stdout.String())
	}
	if strings.Contains(stderr.String(), token) {
		t.Errorf("token echoed on stderr too: %s", stderr.String())
	}
	principal, ok := store.Authenticate(token)
	if !ok || principal.Name != "ci-bot" || strings.Join(principal.Scopes, ",") != "dispatch,read" {
		t.Fatalf("issued principal = %+v ok=%v", principal, ok)
	}
}

func TestAccessIssue_RequiresNameAndScope(t *testing.T) {
	for _, args := range [][]string{{"--scope", "read"}, {"--name", "ci-bot"}} {
		cmd := newAccessIssueCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("args %v accepted", args)
		}
	}
}

func TestAccessIssue_ExpiresIn(t *testing.T) {
	store, err := server.NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(nil, nil, nil, nil, server.Options{Token: "op-token", AccessStore: store}))
	defer srv.Close()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  url: "+srv.URL+"\n  token: op-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROVE_CONFIG", cfgPath)

	cmd := newAccessIssueCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--name", "argos", "--scope", "read", "--scope", "dispatch", "--expires-in", "180d"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("issue: %v (stderr=%s)", err, stderr.String())
	}
	p, ok := store.Authenticate(strings.TrimSpace(stdout.String()))
	if !ok || p.ExpiresAt == nil || p.ExpiresAt.Sub(time.Now()) < 179*24*time.Hour || p.ExpiresAt.Sub(time.Now()) > 180*24*time.Hour {
		t.Fatalf("principal = %+v, %v; want expiring in 180 days", p, ok)
	}
	if !strings.Contains(stderr.String(), "expiring "+p.ExpiresAt.Format(time.DateOnly)) {
		t.Errorf("stderr doesn't say when it expires: %s", stderr.String())
	}
}

func TestParseTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{"180d": 180 * 24 * time.Hour, "720h": 720 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := parseTTL(in); err != nil || got != want {
			t.Errorf("parseTTL(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "1.5d", "-1d", "0h", "soon"} {
		if _, err := parseTTL(in); err == nil {
			t.Errorf("parseTTL(%q) accepted", in)
		}
	}
}
