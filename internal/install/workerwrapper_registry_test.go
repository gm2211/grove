package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

// wrapperRun runs the rendered worker wrapper with a stand-in Orchard that reports the registry
// variables it received, and a stand-in grove whose `broker pull-login` is fakeGrove.
func wrapperRun(t *testing.T, fakeGrove string, legacy string) (out string, source string, script string) {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("no zsh")
	}
	home := t.TempDir()
	opts := testWorkerOptions(home)
	grove := filepath.Join(home, "grove")
	if err := os.WriteFile(grove, []byte(fakeGrove), 0o755); err != nil {
		t.Fatal(err)
	}
	opts.GroveBin = grove
	orchard := filepath.Join(home, "orchard")
	if err := os.WriteFile(orchard, []byte("#!/bin/sh\necho \"tart: $TART_REGISTRY_USERNAME / $TART_REGISTRY_PASSWORD\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(credentialPath(opts, workerBootstrapCredentialName), "bootstrap-123\n"); err != nil {
		t.Fatal(err)
	}
	if legacy != "" {
		if err := writeSecretFile(credentialPath(opts, registryCredentialName), legacy); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(groveTartBinDir(opts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(groveTartShimPath(opts), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script = workerWrapper(opts, orchard)
	path := filepath.Join(home, "wrapper.zsh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(zsh, path)
	cmd.Env = []string{"HOME=" + home}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper: %v\n%s", err, b)
	}
	src, _ := os.ReadFile(registrySourcePath(opts))
	return string(b), strings.TrimSpace(string(src)), script
}

func TestWorkerWrapper_TakesTheRegistryLoginFromTheControlPlane(t *testing.T) {
	// The legacy file holds a different login: it must not be read when the control plane answers.
	out, source, script := wrapperRun(t,
		"#!/bin/sh\n[ \"$1 $2\" = \"broker pull-login\" ] || exit 2\nprintf 'cp-user\\ncp-token\\n'\n",
		"file-user\nfile-token\n")
	if !strings.Contains(out, "tart: cp-user / cp-token\n") || source != "control-plane" {
		t.Errorf("source %q, output:\n%s", source, out)
	}
	// Nothing in the wrapper's text is a secret, and it never echoes TART_REGISTRY_*.
	for _, banned := range []string{"cp-token", "file-token", "echo \"$TART_REGISTRY", "echo $TART_REGISTRY", "print -r -- \"$TART_REGISTRY", "set -x"} {
		if strings.Contains(script, banned) {
			t.Errorf("wrapper text contains %q:\n%s", banned, script)
		}
	}
	if !strings.Contains(script, "broker pull-login") {
		t.Errorf("wrapper does not call broker pull-login:\n%s", script)
	}
}

func TestWorkerWrapper_FallsBackToTheLegacyFileWhenTheControlPlaneHasNothing(t *testing.T) {
	out, source, _ := wrapperRun(t, "#!/bin/sh\necho 'no login for this worker' >&2\nexit 1\n", "file-user\nfile-token\n")
	if !strings.Contains(out, "tart: file-user / file-token\n") || source != "legacy-file" {
		t.Errorf("source %q, output:\n%s", source, out)
	}
}

func TestWorkerWrapper_StartsWithNoRegistryLoginAtAll(t *testing.T) {
	out, source, _ := wrapperRun(t, "#!/bin/sh\nexit 1\n", "")
	if !strings.Contains(out, "tart:  / \n") || source != "none" {
		t.Errorf("source %q, output:\n%s", source, out)
	}
	// A half answer (user without token) is not a login.
	out, source, _ = wrapperRun(t, "#!/bin/sh\nprintf 'only-user\\n'\n", "")
	if strings.Contains(out, "only-user") || source != "none" {
		t.Errorf("half answer used: source %q, output:\n%s", source, out)
	}
}

func TestCheckRegistryLogin_NamesTheLoginSource(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	fleetPath := filepath.Join(home, "fleet.yaml")
	pool := "pools:\n  - name: macos\n    image: ghcr.io/gm2211/grove-macos-worker@sha256:" + strings.Repeat("a", 64) + "\n    perWorker: 1\n    cpu: 4\n    memory: 8192\n"
	if err := os.WriteFile(fleetPath, []byte(pool), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Fleet: fleetPath}
	for source, want := range map[string]string{"control-plane": "control plane", "legacy-file": "legacy file"} {
		if err := os.MkdirAll(filepath.Dir(registrySourcePath(opts)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(registrySourcePath(opts), []byte(source+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := checkRegistryLogin(opts, cfg)
		if res == nil || !res.OK || !strings.Contains(res.Detail, want) {
			t.Errorf("%s: %+v", source, res)
		}
	}
}
