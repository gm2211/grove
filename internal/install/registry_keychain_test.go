package install

import (
	"bytes"
	"context"
	"errors"
	"os"
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
