package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/server"
)

func writeTokenTestConfig(t *testing.T, cfg *config.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROVE_CONFIG", path)
	return path
}

func runTokenRotateForTest(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	tokenRotateCmd.SetOut(&out)
	tokenRotateCmd.SetErr(&errOut)
	defer tokenRotateCmd.SetOut(nil)
	defer tokenRotateCmd.SetErr(nil)
	err = runTokenRotate(tokenRotateCmd, nil)
	return out.String(), errOut.String(), err
}

func TestTokenRotate(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Listen, cfg.Server.URL, cfg.Server.Token = "100.64.0.1:6120", "http://100.64.0.1:6120", "old-operator-token"
	cfg.Orchard.URL, cfg.Nomad.Token = "http://100.64.0.1:6130", "nomad-management"
	path := writeTokenTestConfig(t, cfg)

	stdout, stderr, err := runTokenRotateForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(stdout)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(token) {
		t.Fatalf("stdout = %q, want exactly one 256-bit hex token", stdout)
	}
	if strings.Contains(stderr, token) || strings.Contains(stderr, "old-operator-token") {
		t.Fatalf("a token was printed to stderr: %q", stderr)
	}
	for _, want := range []string{"HUP", "restart", "grove config set server.token -"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}

	got, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.Token != token {
		t.Fatalf("config.yaml holds %q, want the printed token", got.Server.Token)
	}
	if got.Server.Listen != cfg.Server.Listen || got.Orchard.URL != cfg.Orchard.URL || got.Nomad.Token != cfg.Nomad.Token {
		t.Fatalf("rotation changed other settings: %+v", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.yaml mode = %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left beside config.yaml: %v", entries)
	}

	// A second rotation gives a different token.
	again, _, err := runTokenRotateForTest(t)
	if err != nil || strings.TrimSpace(again) == token {
		t.Fatalf("second rotation: %q, %v", again, err)
	}
}

func TestTokenRotateOnlyOnTheControlPlane(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.URL, cfg.Server.Token = "http://100.64.0.1:6120", "copied-operator-token"
	path := writeTokenTestConfig(t, cfg)
	stdout, _, err := runTokenRotateForTest(t)
	if err == nil || !strings.Contains(err.Error(), "not the control plane") {
		t.Fatalf("err = %v, want a refusal off the control plane", err)
	}
	if stdout != "" {
		t.Fatalf("printed %q", stdout)
	}
	got, _ := config.LoadFrom(path)
	if got.Server.Token != "copied-operator-token" {
		t.Fatalf("the config was changed: %q", got.Server.Token)
	}
}

// The running server takes the rotated token up on a reload: the old one stops working, the new
// one works, and a config that can't be read or has no token leaves the old one in force.
func TestRotatedTokenIsPickedUpByReload(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Listen, cfg.Server.Token = "127.0.0.1:6120", "old-operator-token"
	path := writeTokenTestConfig(t, cfg)
	srv := server.New(nil, nil, nil, nil, server.Options{Token: "old-operator-token"})
	status := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	load := func() (*config.Config, error) { return config.LoadFrom(path) }

	stdout, _, err := runTokenRotateForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	newToken := strings.TrimSpace(stdout)
	if status("old-operator-token") != http.StatusOK || status(newToken) != http.StatusUnauthorized {
		t.Fatal("before the reload the running server must still be on the old token")
	}

	current, changed, err := reloadOperatorToken(srv, "old-operator-token", load)
	if err != nil || !changed || current != newToken {
		t.Fatalf("reload = %q, %v, %v", current, changed, err)
	}
	if status("old-operator-token") != http.StatusUnauthorized {
		t.Error("the old token still works after the reload")
	}
	if status(newToken) != http.StatusOK {
		t.Error("the new token does not work after the reload")
	}
	if _, changed, err := reloadOperatorToken(srv, current, load); err != nil || changed {
		t.Errorf("reload with nothing new: changed=%v err=%v", changed, err)
	}

	// A config with no token, or none at all, never clears the one in force.
	empty := &config.Config{}
	empty.Server.Listen = "127.0.0.1:6120"
	if err := config.Save(path, empty); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := reloadOperatorToken(srv, current, load); err == nil || changed {
		t.Errorf("reload of an empty token: changed=%v err=%v, want refused", changed, err)
	}
	if _, changed, err := reloadOperatorToken(srv, current, func() (*config.Config, error) { return nil, errors.New("unreadable") }); err == nil || changed {
		t.Errorf("reload of an unreadable config: changed=%v err=%v, want refused", changed, err)
	}
	if status(newToken) != http.StatusOK {
		t.Error("a failed reload dropped the token in force")
	}
}
