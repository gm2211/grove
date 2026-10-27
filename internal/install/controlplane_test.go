package install

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/nomad"
)

func init() {
	// FakeRunner answers `launchctl print` successfully forever; don't wait for an unload.
	launchdRelaunchWait = 0
}

func testControlPlaneOptions(home string, goos string) Options {
	return testControlPlaneOptionsWith(home, goos, &fakeNomadAdmin{})
}

func testControlPlaneOptionsWith(home string, goos string, nc *fakeNomadAdmin) Options {
	return Options{
		Role:        RoleControlPlane,
		Hostname:    "test-cp",
		Home:        home,
		GOOS:        goos,
		GOARCH:      "arm64",
		LookPath:    lookPathOnly("tailscale"),
		Exists:      alwaysFalseExists,
		NomadClient: func(config.Endpoint) (nomad.Client, error) { return nc, nil },
	}
}

// fakeNomadAdmin is the slice of a Nomad API client the control-plane plan uses: listing nodes and
// minting introduction tokens. Anything else panics through the nil embedded interface.
type fakeNomadAdmin struct {
	nomad.Client
	nodes  []nomad.Node
	intros []string
}

func (f *fakeNomadAdmin) ListNodes(context.Context) ([]nomad.Node, error) { return f.nodes, nil }

func (f *fakeNomadAdmin) CreateIntroToken(_ context.Context, name, pool string, ttl time.Duration) (string, error) {
	f.intros = append(f.intros, name)
	return "intro-for-" + name, nil
}

// nomadBootstrapCall is the `nomad acl bootstrap` command line the plan runs for host, with the
// TLS flags pointing at the CA and grove's own client certificate under home.
func nomadBootstrapCall(home, host string) string {
	tls := filepath.Join(home, ".config", "grove", "nomad", "tls")
	return "nomad acl bootstrap -address=https://" + host + ":4646" +
		" -ca-cert=" + filepath.Join(tls, "ca.pem") +
		" -client-cert=" + filepath.Join(tls, "cli.pem") +
		" -client-key=" + filepath.Join(tls, "cli-key.pem") + " -json"
}

func scriptWorkerEnrollmentToken(r *FakeRunner, home string) {
	orchard := filepath.Join(home, ".local", "bin", "orchard")
	r.Script(orchard+" get service-account grove/token", FakeResult{Stdout: "grove-account-secret\n"})
	r.Script(orchard+" get service-account grove-enroller/token", FakeResult{Stdout: "enroller-account-secret\n"})
	for _, host := range []string{"test-cp", "100.64.0.1"} {
		r.Script(nomadBootstrapCall(home, host), FakeResult{Stdout: `{"AccessorID":"acc","SecretID":"nomad-mgmt-secret"}`})
	}
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

	if !r.CalledWith("brew install hashicorp/tap/nomad") {
		t.Error("expected nomad to be installed via brew on darwin")
	}

	cfgDir := filepath.Join(home, ".config", "grove")
	for _, rel := range []string{
		"config.yaml",
		"fleet.yaml",
		filepath.Join("orchard-controller", "env"),
		filepath.Join("nomad", "server.hcl"),
	} {
		if _, err := os.Stat(filepath.Join(cfgDir, rel)); err != nil {
			t.Errorf("expected %s to be rendered: %v", rel, err)
		}
	}

	for _, label := range []string{"orchard-controller", "nomad", "server"} {
		p := filepath.Join(home, "Library", "LaunchAgents", "com.grove."+label+".plist")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected launch agent for %s: %v", label, err)
		}
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

// TestControlPlanePlan_TrustsHashicorpTapBeforeInstalling covers the fix for the observed defect on
// the control-plane role: recent Homebrew refuses `brew install hashicorp/tap/nomad` from an
// untrusted tap, so it must be tapped+trusted, in order, before the install runs.
func TestControlPlanePlan_TrustsHashicorpTapBeforeInstalling(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptWorkerEnrollmentToken(r, home)
	scriptTailscaleUp(r)
	scriptTapInfo(r, "hashicorp/tap", false, false)
	opts := testControlPlaneOptions(home, "darwin")
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	assertOrdered := func(tapCmd, trustCmd, installCmd string) {
		t.Helper()
		tapIdx, trustIdx, installIdx := -1, -1, -1
		for i, c := range r.Calls {
			switch c.String() {
			case tapCmd:
				tapIdx = i
			case trustCmd:
				trustIdx = i
			case installCmd:
				installIdx = i
			}
		}
		if tapIdx == -1 || trustIdx == -1 || installIdx == -1 {
			t.Fatalf("expected %q, %q and %q to all run, calls: %v", tapCmd, trustCmd, installCmd, r.Calls)
		}
		if !(tapIdx < trustIdx && trustIdx < installIdx) {
			t.Errorf("expected tap(%d) < trust(%d) < install(%d) ordering for %q", tapIdx, trustIdx, installIdx, installCmd)
		}
	}
	assertOrdered("brew tap hashicorp/tap", "brew trust hashicorp/tap", "brew install hashicorp/tap/nomad")
}

// TestControlPlanePlan_SkipsTrustWhenTapsAlreadyTrusted mirrors the worker-role idempotency test:
// once tap-info reports the tap installed and trusted, neither `brew tap` nor `brew trust` should
// run again, though the installs themselves still do (their own Check gates that).
func TestControlPlanePlan_SkipsTrustWhenTapsAlreadyTrusted(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptWorkerEnrollmentToken(r, home)
	scriptTailscaleUp(r)
	scriptTapInfo(r, "hashicorp/tap", true, true)
	opts := testControlPlaneOptions(home, "darwin")
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	for _, forbidden := range []string{
		"brew tap hashicorp/tap", "brew trust hashicorp/tap",
	} {
		if r.CalledWith(forbidden) {
			t.Errorf("%q should not run when the tap is already installed and trusted", forbidden)
		}
	}
	if !r.CalledWith("brew install hashicorp/tap/nomad") {
		t.Error("expected nomad to still be installed")
	}
}

// TestControlPlanePlan_LinuxDoesNotTrustTaps confirms the darwin-only trust steps aren't part of
// the Linux plan at all (Linux has no Homebrew, and installs nomad via direct download).
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
		if s.Name == "brew-trust:hashicorp/tap" {
			t.Errorf("did not expect a brew-trust step in the Linux plan, got %q", s.Name)
		}
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if r.CalledWith("brew tap-info --json=v1 hashicorp/tap") {
		t.Error("no brew command should run at all on Linux")
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

	for _, label := range []string{"orchard-controller", "nomad", "server"} {
		p := filepath.Join(home, ".config", "systemd", "user", "grove-"+label+".service")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected systemd unit for %s: %v", label, err)
		}
	}

	// Linux has no brew: nomad comes from a direct download instead.
	if r.CalledWith("brew install hashicorp/tap/nomad") {
		t.Error("should not use brew on linux")
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
	if r.CalledWith("brew install hashicorp/tap/nomad") {
		t.Error("no install command should run during --dry-run")
	}
}

// TestControlPlanePlan_RestartsNomadOnlyWhenItsConfigChanges: re-running the install on a control
// plane whose server.hcl predates the sandbox client must restart the running agent, or the new
// config never takes effect; with the config already current, the agent is left alone.
func TestControlPlanePlan_RestartsNomadOnlyWhenItsConfigChanges(t *testing.T) {
	home := t.TempDir()
	domain := "gui/" + strconv.Itoa(os.Getuid())
	run := func() *FakeRunner {
		t.Helper()
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
		return r
	}
	restart := "launchctl bootout " + domain + "/com.grove.nomad"

	run() // fresh install
	if r := run(); r.CalledWith(restart) {
		t.Error("a re-run with an unchanged Nomad config restarted Nomad")
	}

	conf := filepath.Join(home, ".config", "grove", "nomad", "server.hcl")
	if err := os.WriteFile(conf, []byte("server { enabled = true }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := run(); !r.CalledWith(restart) || !r.CalledWith("launchctl bootstrap "+domain+" "+filepath.Join(home, "Library", "LaunchAgents", "com.grove.nomad.plist")) {
		t.Error("a re-run that rewrote the Nomad config did not restart Nomad")
	}
	data, _ := os.ReadFile(conf)
	if !strings.Contains(string(data), `enforcement = "strict"`) {
		t.Errorf("server.hcl = %s, want strict client introduction", data)
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

// TestControlPlanePlan_NomadMutualTLS: a control plane comes up with grove's own CA, a server
// that insists on client certificates and introduction tokens, config.yaml pointing grove at its
// client certificate, and the sandbox-host client registered through an introduction token of
// its own. A re-run once that client is registered mints nothing.
func TestControlPlanePlan_NomadMutualTLS(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			home := t.TempDir()
			nc := &fakeNomadAdmin{}
			run := func() *FakeRunner {
				t.Helper()
				r := NewFakeRunner()
				scriptWorkerEnrollmentToken(r, home)
				scriptTailscaleUp(r)
				opts := testControlPlaneOptionsWith(home, goos, nc)
				scriptPinnedDownloads(r, opts)
				steps, err := BuildPlan(r, opts, io.Discard)
				if err != nil {
					t.Fatalf("BuildPlan: %v", err)
				}
				if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
					t.Fatalf("RunPlan: %v", err)
				}
				return r
			}
			r := run()

			cfgDir := filepath.Join(home, ".config", "grove")
			tlsDir := filepath.Join(cfgDir, "nomad", "tls")
			for _, key := range []string{"ca-key.pem", "server-key.pem", "cli-key.pem", "node-key.pem"} {
				info, err := os.Stat(filepath.Join(tlsDir, key))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf("%s: %v %v, want mode 0600", key, info, err)
				}
			}
			cfg, err := config.LoadFrom(filepath.Join(cfgDir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(cfg.Nomad.URL, "https://") || cfg.Nomad.CACert != filepath.Join(tlsDir, "ca.pem") ||
				cfg.Nomad.ClientCert != filepath.Join(tlsDir, "cli.pem") || cfg.Nomad.NodeCert != filepath.Join(tlsDir, "node.pem") {
				t.Errorf("config.yaml nomad = %+v, want https and the TLS file paths", cfg.Nomad)
			}
			sandboxConf, err := os.ReadFile(filepath.Join(cfgDir, "nomad", "sandbox-client.hcl"))
			if err != nil || !strings.Contains(string(sandboxConf), filepath.Join(tlsDir, "node.pem")) {
				t.Errorf("sandbox-client.hcl = %s, %v; want the node certificate", sandboxConf, err)
			}

			token := filepath.Join(cfgDir, "nomad", "sandbox-data", "client", "intro_token.jwt")
			data, err := os.ReadFile(token)
			if err != nil || string(data) != "intro-for-"+SandboxClientNodeName {
				t.Fatalf("intro token = %q, %v", data, err)
			}
			if info, _ := os.Stat(token); info.Mode().Perm() != 0o600 {
				t.Errorf("intro token mode = %v, want 0600", info.Mode().Perm())
			}
			restart := "systemctl --user restart grove-nomad-sandbox-client.service"
			if goos == "darwin" {
				restart = "launchctl kickstart -k gui/" + strconv.Itoa(os.Getuid()) + "/com.grove.nomad-sandbox-client"
			}
			if !r.CalledWith(restart) {
				t.Errorf("sandbox client not restarted onto its token (%s); calls: %v", restart, r.Calls)
			}
			for _, c := range r.Calls {
				if strings.Contains(c.String(), "intro-for-") {
					t.Errorf("introduction token reached argv: %s", c.String())
				}
			}

			nc.nodes = []nomad.Node{{Name: SandboxClientNodeName, Status: "ready"}}
			before := len(nc.intros)
			if r := run(); r.CalledWith(restart) || len(nc.intros) != before {
				t.Errorf("a re-run with the sandbox client registered minted %d tokens / restarted it", len(nc.intros)-before)
			}
		})
	}
}

func TestControlPlanePlan_RebuildsOrchardAtThePinAndRestartsTheController(t *testing.T) {
	home := t.TempDir()
	domain := "gui/" + strconv.Itoa(os.Getuid())
	destOrchard := filepath.Join(home, ".local", "bin", "orchard")
	run := func() *FakeRunner {
		t.Helper()
		r := NewFakeRunner()
		scriptWorkerEnrollmentToken(r, home)
		scriptTailscaleUp(r)
		r.Script(destOrchard+" create service-account --help", FakeResult{Stdout: "roles: service-account:issue-worker"})
		opts := testControlPlaneOptions(home, "darwin")
		scriptPinnedDownloads(r, opts)
		steps, err := BuildPlan(r, opts, io.Discard)
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
			t.Fatalf("RunPlan: %v", err)
		}
		return r
	}
	built := func(r *FakeRunner) bool {
		for _, c := range r.Calls {
			if c.Name == "/bin/sh" && len(c.Args) == 2 && strings.Contains(c.Args[1], "git fetch -q --depth 1 https://github.com/gm2211/orchard "+OrchardForkCommit) {
				return true
			}
		}
		return false
	}
	restart := "launchctl bootout " + domain + "/com.grove.orchard-controller"

	if r := run(); !built(r) {
		t.Fatal("a fresh install must build Orchard at the pinned commit")
	}
	// The fake build moved nothing into place.
	if err := os.WriteFile(destOrchard, []byte("orchard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := run(); built(r) || r.CalledWith(restart) {
		t.Error("an Orchard already at the pin was rebuilt or its controller restarted")
	}

	// Built from another commit (an older pin, or a binary from before pinning).
	if err := os.WriteFile(orchardCommitPath(destOrchard), []byte("0000000000000000000000000000000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := run(); !built(r) || !r.CalledWith(restart) {
		t.Error("an Orchard from another commit must be rebuilt and the controller restarted onto it")
	}
}

func TestControlPlanePlan_ServicesNameAbsolutePrograms(t *testing.T) {
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
				t.Fatal(err)
			}
			if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
				t.Fatal(err)
			}
			var files []string
			if goos == "darwin" {
				files, _ = filepath.Glob(filepath.Join(opts.LaunchAgentsDir(), "com.grove.*.plist"))
			} else {
				files, _ = filepath.Glob(filepath.Join(opts.SystemdUserDir(), "grove-*.service"))
			}
			if len(files) == 0 {
				t.Fatal("no service files rendered")
			}
			for _, f := range files {
				b, _ := os.ReadFile(f)
				body := string(b)
				for _, bare := range []string{"<string>nomad</string>", "<string>grove</string>", "ExecStart=nomad ", "ExecStart=grove "} {
					if strings.Contains(body, bare) {
						t.Errorf("%s names a bare program (%q); launchd/systemd can't find it on their PATH:\n%s", f, bare, body)
					}
				}
			}
		})
	}
}

func TestRelaunchAgent_WaitsForUnloadBeforeBootstrap(t *testing.T) {
	saved := launchdRelaunchWait
	launchdRelaunchWait, launchdPoll = time.Second, time.Millisecond
	defer func() { launchdRelaunchWait = saved }()

	r := &unloadingRunner{loadedFor: 3}
	if err := relaunchAgent(context.Background(), r, "gui/502", "com.grove.nomad", "/p.plist"); err != nil {
		t.Fatal(err)
	}
	if r.prints != 4 || !r.bootstrappedUnloaded {
		t.Fatalf("bootstrap must wait for launchd to unload the job: prints=%d bootstrappedUnloaded=%v", r.prints, r.bootstrappedUnloaded)
	}
}

// unloadingRunner models launchd's asynchronous bootout: the job stays loaded for loadedFor
// `launchctl print` calls after bootout.
type unloadingRunner struct {
	FakeRunner
	loadedFor            int
	prints               int
	bootstrappedUnloaded bool
}

func (u *unloadingRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	if name == "launchctl" && len(args) > 0 {
		switch args[0] {
		case "print":
			u.prints++
			if u.prints <= u.loadedFor {
				return "", "", nil
			}
			return "", "", errors.New("Could not find service")
		case "bootstrap":
			u.bootstrappedUnloaded = u.prints > u.loadedFor
		}
	}
	return "", "", nil
}
