package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func testControlPlaneOptions(home string, goos string) Options {
	return Options{
		Role:     RoleControlPlane,
		Hostname: "test-cp",
		Home:     home,
		GOOS:     goos,
		GOARCH:   "arm64",
		LookPath: lookPathOnly("tailscale"),
		Exists:   alwaysFalseExists,
	}
}

func scriptWorkerEnrollmentToken(r *FakeRunner, home string) {
	orchard := filepath.Join(home, ".local", "bin", "orchard")
	r.Script(orchard+" get service-account grove/token", FakeResult{Stdout: "grove-account-secret\n"})
	r.Script(orchard+" get service-account grove-enroller/token", FakeResult{Stdout: "enroller-account-secret\n"})
}

func TestControlPlaneOrchardHomePreservesExistingLayouts(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "orchard-controller")
	if got := controlPlaneOrchardHome(base); got != root {
		t.Fatalf("fresh home=%q want %q", got, root)
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := controlPlaneOrchardHome(base); got != filepath.Join(root, "data") {
		t.Fatalf("nested home=%q", got)
	}
	if err := os.MkdirAll(filepath.Join(root, ".orchard"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".orchard", "orchard.yml"), []byte("contexts: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := controlPlaneOrchardHome(base); got != root {
		t.Fatalf("legacy home=%q want %q", got, root)
	}
}

func TestControlPlanePlan_DarwinRendersConfigAndLaunchAgents(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptWorkerEnrollmentToken(r, home)
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "darwin")
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	for _, c := range r.Calls {
		if strings.Contains(c.String(), "nomad") || strings.Contains(c.String(), "hashicorp") {
			t.Errorf("a fresh control plane must not touch Nomad: %s", c.String())
		}
	}

	cfgDir := filepath.Join(home, ".config", "grove")
	for _, rel := range []string{
		"config.yaml",
		"fleet.yaml",
		filepath.Join("orchard-controller", "env"),
	} {
		if _, err := os.Stat(filepath.Join(cfgDir, rel)); err != nil {
			t.Errorf("expected %s to be rendered: %v", rel, err)
		}
	}

	for _, label := range []string{"orchard-controller", "server"} {
		p := filepath.Join(home, "Library", "LaunchAgents", "com.grove."+label+".plist")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected launch agent for %s: %v", label, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "nomad")); !os.IsNotExist(err) {
		t.Errorf("a fresh control plane must not get a Nomad config dir (err=%v)", err)
	}

	cfg, err := config.LoadFrom(filepath.Join(cfgDir, "config.yaml"))
	if err != nil {
		t.Fatalf("loading rendered config.yaml: %v", err)
	}
	if cfg.Server.Token == "" {
		t.Error("expected a generated server token")
	}
	wantArtifacts := filepath.Join(home, ".local", "state", "grove", "artifacts")
	if cfg.Artifacts.Dir != wantArtifacts {
		t.Errorf("cfg.Artifacts.Dir = %q, want %q", cfg.Artifacts.Dir, wantArtifacts)
	}
	if info, err := os.Stat(wantArtifacts); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("expected artifact store dir %s with mode 0700: %v %v", wantArtifacts, info, err)
	}
	if cfg.Orchard.Token == "" {
		t.Error("expected a grove-generated orchard service-account token")
	}
	if cfg.Enrollment.IssuerToken != "grove-enroller:enroller-account-secret" {
		t.Error("expected control-plane enrollment issuer credential")
	}
	if cfg.Fleet != filepath.Join(cfgDir, "fleet.yaml") {
		t.Errorf("cfg.Fleet = %q, want %q", cfg.Fleet, filepath.Join(cfgDir, "fleet.yaml"))
	}

	// `orchard create service-account` must be called with --roles repeated once per role
	// (the fork's --roles is a StringArrayVar and does not split on commas) and with an
	// Credentials are retrieved after creation so no secret appears in argv.
	destOrchard := filepath.Join(home, ".local", "bin", "orchard")
	wantCreate := Call{
		Name: destOrchard,
		Args: []string{
			"create", "service-account", "grove",
			"--roles", "compute:read",
			"--roles", "compute:write",
			"--roles", "compute:connect",
		},
	}
	found := false
	for _, c := range r.Calls {
		if c.String() == wantCreate.String() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected call %q, calls were: %v", wantCreate.String(), r.Calls)
	}
	for _, call := range r.Calls {
		for _, arg := range call.Args {
			if strings.Contains(arg, "account-secret") {
				t.Fatalf("service-account secret leaked into argv: %s", call.String())
			}
		}
	}
	workerCreate := destOrchard + " create service-account grove-enroller --roles service-account:issue-worker"
	if !r.CalledWith(workerCreate) {
		t.Errorf("expected scoped worker account creation, calls were: %v", r.Calls)
	}
	if strings.Contains(workerCreate, "worker-account-secret") {
		t.Fatal("worker account secret leaked into argv")
	}

	// No sudo anywhere in the control-plane plan; it has no privileged steps.
	if r.CalledWith("sudo") {
		t.Error("control-plane plan should never invoke sudo")
	}
}

// TestControlPlanePlan_LinuxDoesNotTrustTaps confirms the darwin-only trust steps aren't part of
// the Linux plan at all (Linux has no Homebrew).
func TestControlPlanePlan_LinuxDoesNotTrustTaps(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptWorkerEnrollmentToken(r, home)
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "linux")
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	for _, s := range steps {
		if strings.HasPrefix(s.Name, "brew-trust:") {
			t.Errorf("did not expect a brew-trust step in the Linux plan, got %q", s.Name)
		}
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	for _, c := range r.Calls {
		if c.Name == "brew" {
			t.Errorf("no brew command should run at all on Linux: %s", c.String())
		}
	}
}

func TestControlPlanePlan_LinuxRendersSystemdUnits(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptWorkerEnrollmentToken(r, home)
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "linux")
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	for _, label := range []string{"orchard-controller", "server"} {
		p := filepath.Join(home, ".config", "systemd", "user", "grove-"+label+".service")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected systemd unit for %s: %v", label, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "grove-nomad.service")); !os.IsNotExist(err) {
		t.Errorf("did not expect a Nomad unit (err=%v)", err)
	}
}

func TestControlPlanePlan_DryRunAppliesNothing(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, "darwin")
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	results, err := RunPlan(context.Background(), steps, PlanOptions{DryRun: true, Out: io.Discard})
	if err != nil {
		t.Fatalf("RunPlan (dry-run): %v", err)
	}
	for _, res := range results {
		if res.Applied {
			t.Errorf("step %q was applied during --dry-run", res.Step)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "grove", "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected no config.yaml written during --dry-run, stat err = %v", err)
	}
	for _, c := range r.Calls {
		if c.Name == "brew" && len(c.Args) > 0 && c.Args[0] == "install" {
			t.Errorf("no install command should run during --dry-run: %s", c.String())
		}
	}
}

// TestControlPlanePlan_RetiresLegacyNomad covers re-running install over a control plane an older
// grove set up with Nomad: the Nomad service is stopped and its service file removed, Nomad's
// config and data are left for the operator, and config.yaml loses its nomad/tailscale keys
// (tokens included) without the rest of the config changing.
func TestControlPlanePlan_RetiresLegacyNomad(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			home := t.TempDir()
			cfgDir := filepath.Join(home, ".config", "grove")
			legacy := &config.Config{
				Nomad:     config.Endpoint{URL: "http://100.64.0.1:4646", Token: "nomad-mgmt-secret"},
				Tailscale: config.TailscaleConfig{AuthKey: "tskey-legacy-secret"},
				Server:    config.ServerConfig{Token: "keep-me"},
			}
			if err := config.Save(filepath.Join(cfgDir, "config.yaml"), legacy); err != nil {
				t.Fatal(err)
			}
			dataFile := filepath.Join(cfgDir, "nomad", "data", "server", "raft.db")
			serviceFile := filepath.Join(home, "Library", "LaunchAgents", "com.grove.nomad.plist")
			wantCall := "launchctl bootout gui/" + strconv.Itoa(os.Getuid()) + "/com.grove.nomad"
			if goos == "linux" {
				serviceFile = filepath.Join(home, ".config", "systemd", "user", "grove-nomad.service")
				wantCall = "systemctl --user disable --now grove-nomad.service"
			}
			for _, p := range []string{dataFile, serviceFile} {
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			r := NewFakeRunner()
			scriptWorkerEnrollmentToken(r, home)
			scriptTailscaleUp(r)
			opts := testControlPlaneOptions(home, goos)
			scriptPinnedDownloads(r, opts)
			steps, err := BuildPlan(r, opts, io.Discard)
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
				t.Fatalf("RunPlan: %v", err)
			}
			if !r.CalledWith(wantCall) {
				t.Errorf("expected %q, calls: %v", wantCall, r.Calls)
			}
			if _, err := os.Stat(serviceFile); !os.IsNotExist(err) {
				t.Errorf("expected %s removed, err=%v", serviceFile, err)
			}
			if _, err := os.Stat(dataFile); err != nil {
				t.Errorf("Nomad data must be left for the operator: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(cfgDir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			for _, leaked := range []string{"nomad", "tailscale", "nomad-mgmt-secret", "tskey-legacy-secret"} {
				if strings.Contains(string(raw), leaked) {
					t.Errorf("legacy %q left in config.yaml:\n%s", leaked, raw)
				}
			}
			cfg, err := config.LoadFrom(filepath.Join(cfgDir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server.Token != "keep-me" {
				t.Errorf("server.token = %q, want it kept", cfg.Server.Token)
			}

			// A second run finds nothing left to retire.
			r2 := NewFakeRunner()
			scriptWorkerEnrollmentToken(r2, home)
			scriptTailscaleUp(r2)
			steps, _ = BuildPlan(r2, opts, io.Discard)
			if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
				t.Fatalf("second RunPlan: %v", err)
			}
			if r2.CalledWith(wantCall) {
				t.Error("legacy-nomad:retire re-ran with no Nomad service left")
			}
		})
	}
}

// TestControlPlanePlan_ArtifactStoreIsLocalNotAService asserts the artifact store is a local directory: no plan step, unit
// or command installs, configures or starts MinIO (or its client) on either OS.
func TestControlPlanePlan_ArtifactStoreIsLocalNotAService(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			home := t.TempDir()
			r := NewFakeRunner()
			scriptWorkerEnrollmentToken(r, home)
			scriptTailscaleUp(r)
			opts := testControlPlaneOptions(home, goos)
			scriptPinnedDownloads(r, opts)

			steps, err := BuildPlan(r, opts, io.Discard)
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			for _, s := range steps {
				if strings.Contains(strings.ToLower(s.Name), "minio") && s.Name != "legacy-minio:retire" {
					t.Errorf("unexpected MinIO step %q", s.Name)
				}
			}
			if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
				t.Fatalf("RunPlan: %v", err)
			}
			for _, c := range r.Calls {
				if strings.Contains(strings.ToLower(c.String()), "minio") || strings.Contains(c.String(), "grove-mc") {
					t.Errorf("unexpected MinIO command: %s", c.String())
				}
			}
			for _, p := range []string{
				filepath.Join(home, "Library", "LaunchAgents", "com.grove.minio.plist"),
				filepath.Join(home, ".config", "systemd", "user", "grove-minio.service"),
				filepath.Join(home, ".config", "grove", "minio"),
			} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("did not expect %s to exist (err=%v)", p, err)
				}
			}
		})
	}
}

// TestControlPlanePlan_RetiresLegacyObjectStore covers re-running install over an older control plane:
// the MinIO service file is stopped and removed, MinIO's data is left alone, and config.yaml
// loses its S3 keys (secret included) and gains artifacts.dir.
func TestControlPlanePlan_RetiresLegacyObjectStore(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			home := t.TempDir()
			cfgDir := filepath.Join(home, ".config", "grove")
			legacy := &config.Config{Artifacts: config.ArtifactsConfig{
				Endpoint: "http://100.64.0.1:9000", Bucket: "grove",
				AccessKey: "grove-artifacts-abc", SecretKey: "legacy-secret",
			}}
			if err := config.Save(filepath.Join(cfgDir, "config.yaml"), legacy); err != nil {
				t.Fatal(err)
			}
			dataFile := filepath.Join(cfgDir, "minio", "data", "grove", "obj")
			serviceFile := filepath.Join(home, "Library", "LaunchAgents", "com.grove.minio.plist")
			wantCall := "launchctl bootout gui/" + strconv.Itoa(os.Getuid()) + "/com.grove.minio"
			if goos == "linux" {
				serviceFile = filepath.Join(home, ".config", "systemd", "user", "grove-minio.service")
				wantCall = "systemctl --user disable --now grove-minio.service"
			}
			for _, p := range []string{dataFile, serviceFile} {
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			r := NewFakeRunner()
			scriptWorkerEnrollmentToken(r, home)
			scriptTailscaleUp(r)
			opts := testControlPlaneOptions(home, goos)
			scriptPinnedDownloads(r, opts)
			steps, err := BuildPlan(r, opts, io.Discard)
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
				t.Fatalf("RunPlan: %v", err)
			}
			if !r.CalledWith(wantCall) {
				t.Errorf("expected %q, calls: %v", wantCall, r.Calls)
			}
			if _, err := os.Stat(serviceFile); !os.IsNotExist(err) {
				t.Errorf("expected %s removed, err=%v", serviceFile, err)
			}
			if _, err := os.Stat(dataFile); err != nil {
				t.Errorf("MinIO data must be left for the operator: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(cfgDir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "legacy-secret") || strings.Contains(string(raw), "endpoint") {
				t.Errorf("legacy S3 keys left in config.yaml:\n%s", raw)
			}
			cfg, err := config.LoadFrom(filepath.Join(cfgDir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Artifacts.Dir == "" {
				t.Error("expected artifacts.dir to be recorded")
			}
		})
	}
}
