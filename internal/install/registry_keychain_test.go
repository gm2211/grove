package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

const tartLoginCmd = "tart login ghcr.io --username gm2211 --password-stdin"

func TestWorkerPlan_RegistryLoginSurvivesAnUnavailableKeychain(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	opts.RegistryUser, opts.RegistryToken = "gm2211", "ghp_tok"
	r := NewFakeRunner()
	r.Script(tartLoginCmd, FakeResult{
		Err: errors.New(`tart login: exit status 1: Error: Failed(message: "Keychain failed to add item: User interaction is not allowed.")`),
	})
	var out bytes.Buffer
	step := stepNamed(t, planWorkerOut(t, r, opts, &out), "registry-login:ghcr.io")
	if err := step.Apply(context.Background()); err != nil {
		t.Fatalf("a Keychain failure must not fail the step: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("expected a warning, got %q", out.String())
	}
	if b, err := os.ReadFile(credentialPath(opts, registryCredentialName)); err != nil || string(b) != "gm2211\nghp_tok\n" {
		t.Errorf("credential file = %q, %v", b, err)
	}
	if ok, _ := step.Check(context.Background()); !ok {
		t.Error("Check must be satisfied once the file and marker are written (no loop)")
	}
}

func TestWorkerPlan_RegistryLoginOtherTartErrorsStayFatal(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	opts.RegistryUser, opts.RegistryToken = "gm2211", "ghp_bad"
	r := NewFakeRunner()
	r.Script(tartLoginCmd, FakeResult{Err: errors.New("tart login: exit status 1: Error: unauthorized: bad credentials")})
	step := stepNamed(t, planWorker(t, r, opts), "registry-login:ghcr.io")
	if err := step.Apply(context.Background()); err == nil {
		t.Fatal("rejected credentials must fail")
	}
	if _, err := os.Stat(credentialPath(opts, registryCredentialName)); err == nil {
		t.Error("no credential file should be written for a rejected login")
	}
}

func TestRunDoctor_RegistryLoginOKWithOnlyTheCredentialFile(t *testing.T) {
	home := t.TempDir()
	fleetPath := writeFleetYAML(t, home, "ghcr.io/gm2211/grove-macos-worker:latest")
	opts := Options{Home: home, GOOS: "darwin", LookPath: alwaysFailLookPath, Exists: alwaysFalseExists}
	if err := writeSecretFile(credentialPath(opts, registryCredentialName), "gm2211\nghp_tok\n"); err != nil {
		t.Fatal(err)
	}
	results := RunDoctor(context.Background(), NewFakeRunner(), opts, &config.Config{Fleet: fleetPath})
	if r := findResult(t, results, "registry-login"); !r.OK {
		t.Errorf("the credential file alone should satisfy registry-login, got %+v", r)
	}
}

// Over ssh or `su - user`, PATH has no ~/.local/bin, which is where grove links its own tart:
// the registry login must still find it, by absolute path.
func TestTartCommand_FallsBackToGrovesOwnTart(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	if got := tartCommand(opts); got != "tart" {
		t.Fatalf("with no tart anywhere: %q", got)
	}
	link := groveManagedTartLink(opts)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := tartCommand(opts); got != link {
		t.Fatalf("tart off PATH: got %q, want %q", got, link)
	}
	opts.RegistryUser, opts.RegistryToken = "gm2211", "ghp_tok"
	r := NewFakeRunner()
	step := stepNamed(t, planWorkerOut(t, r, opts, &bytes.Buffer{}), "registry-login:ghcr.io")
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range r.Calls {
		if c.Name == link && len(c.Args) > 0 && c.Args[0] == "login" {
			found = true
		}
		if strings.Contains(strings.Join(c.Args, " "), "ghp_tok") {
			t.Fatalf("token in argv: %+v", c)
		}
	}
	if !found {
		t.Fatalf("registry login didn't run grove's own tart: %+v", r.Calls)
	}
	opts.LookPath = lookPathOnly("tailscale", "tart")
	if got := tartCommand(opts); got != "tart" {
		t.Fatalf("tart on PATH: %q", got)
	}
}
