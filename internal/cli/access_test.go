package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	cmd.SetArgs([]string{"--name", "argos", "--scope", "read", "--scope", "dispatch"})
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
	if !ok || principal.Name != "argos" || strings.Join(principal.Scopes, ",") != "dispatch,read" {
		t.Fatalf("issued principal = %+v ok=%v", principal, ok)
	}
}

func TestAccessIssue_RequiresNameAndScope(t *testing.T) {
	for _, args := range [][]string{{"--scope", "read"}, {"--name", "argos"}} {
		cmd := newAccessIssueCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("args %v accepted", args)
		}
	}
}
