package install

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// planWorker builds the worker plan for opts on r.
func planWorker(t *testing.T, r *FakeRunner, opts Options) []Step {
	t.Helper()
	return planWorkerOut(t, r, opts, io.Discard)
}

// planWorkerOut is planWorker with the plan's progress output sent to out.
func planWorkerOut(t *testing.T, r *FakeRunner, opts Options, out io.Writer) []Step {
	t.Helper()
	scriptTailscaleUp(r)
	scriptPinnedDownloads(r, opts)
	steps, err := BuildPlan(r, opts, out)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return steps
}

func stepNamed(t *testing.T, steps []Step, name string) Step {
	t.Helper()
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %q in the plan", name)
	return Step{}
}

func gui(label string) string { return "gui/" + strconv.Itoa(os.Getuid()) + "/" + label }

func TestRenderLaunchAgent_UserName(t *testing.T) {
	got, err := RenderLaunchAgent(LaunchAgentSpec{Label: "com.grove.x", UserName: "argos", Program: "/p", StdoutPath: "/o", StderrPath: "/e"})
	if err != nil {
		t.Fatal(err)
	}
	want := `	<key>UserName</key>
	<string>argos</string>
`
	if !strings.Contains(got, want) {
		t.Errorf("plist lacks UserName:\n%s", got)
	}
	if strings.Index(got, "<key>UserName</key>") < strings.Index(got, "</array>") {
		t.Errorf("UserName should follow ProgramArguments:\n%s", got)
	}
	plain, _ := RenderLaunchAgent(LaunchAgentSpec{Label: "com.grove.x", Program: "/p"})
	if strings.Contains(plain, "UserName") {
		t.Errorf("an agent has no UserName:\n%s", plain)
	}
}

func TestDaemonPlist_NeverRoot(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	for _, d := range workerDaemons(opts) {
		plist, err := d.Render()
		if err != nil {
			t.Fatalf("%s: %v", d.Label, err)
		}
		if !strings.Contains(plist, "<key>UserName</key>\n\t<string>worker-user</string>") {
			t.Errorf("%s runs as root: it has no UserName:\n%s", d.Label, plist)
		}
	}
	opts.User = "root"
	for _, d := range workerDaemons(opts) {
		if plist, err := d.Render(); err == nil {
			t.Errorf("%s rendered for root:\n%s", d.Label, plist)
		}
	}
	if _, err := BuildPlan(NewFakeRunner(), opts, io.Discard); err == nil || !strings.Contains(err.Error(), "not root") {
		t.Errorf("BuildPlan for user root: err = %v, want a refusal", err)
	}
}

func TestWorkerPlan_RootWithYesInstallsAndLoadsTheDaemons(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	opts := testWorkerOptions(home)
	steps := planWorker(t, r, opts)
	// The fake runner copies nothing: put what `sudo install` of an untouched staged plist leaves.
	for _, d := range workerDaemons(opts) {
		placeInstalled(t, opts, d, "")
	}

	if _, err := RunPlan(context.Background(), steps, PlanOptions{Yes: true, IsRoot: true, Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	for _, d := range workerDaemons(opts) {
		staged, installed := daemonPaths(opts, d.Label)
		for _, want := range []string{
			"sudo install -o root -g wheel -m 0644 " + staged + " " + installed,
			"sudo launchctl bootout " + gui(d.Label),
			"sudo launchctl bootout system/" + d.Label,
			"sudo launchctl bootstrap system " + installed,
		} {
			if !r.CalledWith(want) {
				t.Errorf("missing %q; calls: %v", want, r.Calls)
			}
		}
	}
	if !r.CalledWith("sudo pmset -a disablesleep 1") {
		t.Error("root with --yes applies the other privileged steps too")
	}
	// bootstrap comes after the install and the UserName check.
	var order []string
	for _, c := range r.Calls {
		if c.Name == "sudo" && len(c.Args) > 0 && (c.Args[0] == "install" || c.Args[0] == "launchctl") && strings.Contains(c.String(), "com.grove.broker") {
			order = append(order, c.Args[0]+" "+c.Args[1])
		}
	}
	if len(order) < 3 || order[0] != "install -o" || order[len(order)-1] != "launchctl bootstrap" {
		t.Errorf("broker daemon calls out of order: %v", order)
	}
}

func TestWorkerPlan_NoSudoWithoutRootAndYes(t *testing.T) {
	for name, po := range map[string]PlanOptions{
		"not root":     {Yes: true, IsRoot: false, Out: io.Discard},
		"root, no yes": {Yes: false, IsRoot: true, Out: io.Discard},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewFakeRunner()
			steps := planWorker(t, r, testWorkerOptions(t.TempDir()))
			if _, err := RunPlan(context.Background(), steps, po); err != nil {
				t.Fatalf("RunPlan: %v", err)
			}
			for _, c := range r.Calls {
				if c.Name == "sudo" {
					t.Errorf("sudo ran: %s", c.String())
				}
			}
		})
	}
}

// placeInstalled writes d's installed plist: what grove renders, plus extra.
func placeInstalled(t *testing.T, opts Options, d daemon, extra string) {
	t.Helper()
	plist, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	_, installed := daemonPaths(opts, d.Label)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte(plist+extra), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The staged plist is the worker user's file: extra keys (GroupName wheel, another Label,
// SessionCreate), or any other changed byte, must never get loaded from the root-owned copy.
func TestWorkerDaemons_RefuseAPlistThatIsNotWhatGroveRenders(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	d := workerDaemons(opts)[0]
	_, installed := daemonPaths(opts, d.Label)
	for name, mutate := range map[string]func(string) string{
		"GroupName": func(p string) string {
			return strings.Replace(p, "</dict>", "\t<key>GroupName</key>\n\t<string>wheel</string>\n</dict>", 1)
		},
		"another Label": func(p string) string { return strings.Replace(p, orchardWorkerLabel, "com.apple.other", 1) },
		"no UserName":   func(p string) string { return strings.Replace(p, "<key>UserName</key>", "<key>Nope</key>", 1) },
		"one byte":      func(p string) string { return p + " " },
	} {
		t.Run(name, func(t *testing.T) {
			r := NewFakeRunner()
			steps := planWorker(t, r, opts)
			plist, _ := d.Render()
			if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(installed, []byte(mutate(plist)), 0o644); err != nil {
				t.Fatal(err)
			}
			err := stepNamed(t, steps, "worker-daemons").Apply(context.Background())
			if err == nil || !strings.Contains(err.Error(), "not the plist grove renders") {
				t.Fatalf("Apply: err = %v, want a refusal", err)
			}
			if !r.CalledWith("sudo rm -f " + installed) {
				t.Error("the rejected copy must be removed")
			}
			for _, c := range r.Calls {
				if strings.Contains(c.String(), "launchctl bootstrap") {
					t.Errorf("a tampered daemon was loaded: %s", c.String())
				}
			}
		})
	}

	// A missing copy (the install did not happen) is refused too, and root is never a user.
	r := NewFakeRunner()
	steps := planWorker(t, r, opts)
	os.Remove(installed)
	if err := stepNamed(t, steps, "worker-daemons").Apply(context.Background()); err == nil {
		t.Error("no installed copy must be refused")
	}
}

func TestWorkerDaemonsInstallCommand(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	desc := stepNamed(t, planWorker(t, NewFakeRunner(), opts), "worker-daemons").Description
	for _, d := range workerDaemons(opts) {
		staged, installed := daemonPaths(opts, d.Label)
		for _, want := range []string{
			"sudo install -o root -g wheel -m 0644 \"" + staged + "\" \"" + installed + "\"",
			"shasum -a 256 < \"" + installed + "\" | cut -d' ' -f1)\" = \"" + plistSHA256(d) + "\"",
			"|| { sudo rm -f \"" + installed + "\"; false; }",
			"sudo launchctl bootout " + gui(d.Label),
			"sudo launchctl bootout system/" + d.Label,
			"sudo launchctl bootstrap system \"" + installed + "\"",
			"sudo rm -f \"" + legacyAgentPlist(opts, d.Label) + "\"",
		} {
			if !strings.Contains(desc, want) {
				t.Errorf("install command lacks %q:\n%s", want, desc)
			}
		}
	}
	// One command: the first daemon is installed and checked before anything is loaded.
	if strings.Index(desc, "launchctl bootstrap") < strings.LastIndex(desc, "shasum") {
		t.Errorf("a daemon is loaded before the hash check of the last one:\n%s", desc)
	}
	for _, d := range workerDaemons(opts) {
		if len(plistSHA256(d)) != 64 {
			t.Errorf("%s: no expected hash", d.Label)
		}
	}
}

func TestWorkerDaemonsCheck(t *testing.T) {
	r := NewFakeRunner()
	opts := testWorkerOptions(t.TempDir())
	step := stepNamed(t, planWorker(t, r, opts), "worker-daemons")
	ctx := context.Background()
	if ok, _ := step.Check(ctx); ok {
		t.Fatal("nothing is installed yet")
	}
	for _, d := range workerDaemons(opts) {
		plist, _ := d.Render()
		_, installed := daemonPaths(opts, d.Label)
		if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(installed, []byte(plist), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := step.Check(ctx); ok {
		t.Fatal("installed but not loaded")
	}
	r.Script("launchctl print system/com.grove.orchard-worker", FakeResult{Stdout: "program = " + workerWrapperPath(opts) + "\n"})
	r.Script("launchctl print system/com.grove.broker", FakeResult{Stdout: "program = " + groveExecutable(opts) + "\n"})
	if ok, err := step.Check(ctx); !ok || err != nil {
		t.Fatalf("installed and loaded: %v, %v", ok, err)
	}
	// An installed plist that is not what this grove renders is replaced.
	_, installed := daemonPaths(opts, brokerLabel)
	if err := os.WriteFile(installed, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := step.Check(ctx); ok {
		t.Fatal("a stale installed plist must fail the check")
	}
}

func TestRetireLaunchAgents_WaitsForTheDaemon(t *testing.T) {
	r := NewFakeRunner()
	opts := testWorkerOptions(t.TempDir())
	ds := workerDaemons(opts)
	step := retireLaunchAgentsStep(r, opts, ds)
	ctx := context.Background()
	if ok, _ := step.Check(ctx); !ok {
		t.Fatal("no LaunchAgents: nothing to retire")
	}
	if err := os.MkdirAll(opts.LaunchAgentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if err := os.WriteFile(legacyAgentPlist(opts, d.Label), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := step.Check(ctx); ok {
		t.Fatal("LaunchAgents remain")
	}

	// The system job isn't loaded: removing the agent would leave no worker.
	r.Script("launchctl print system/com.grove.orchard-worker", FakeResult{Err: errors.New("Could not find service")})
	r.Script("launchctl print system/com.grove.broker", FakeResult{Err: errors.New("Could not find service")})
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if _, err := os.Stat(legacyAgentPlist(opts, d.Label)); err != nil {
			t.Errorf("%s removed before its daemon was loaded", d.Label)
		}
	}
	for _, c := range r.Calls {
		if c.Name == "launchctl" && len(c.Args) > 0 && c.Args[0] == "bootout" {
			t.Errorf("an agent was booted out before its daemon was loaded: %s", c.String())
		}
	}

	// The worker's daemon is loaded; the broker's is not: only the worker's agent goes.
	r.Script("launchctl print system/com.grove.orchard-worker", FakeResult{Stdout: "program = " + workerWrapperPath(opts) + "\n"})
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyAgentPlist(opts, orchardWorkerLabel)); !os.IsNotExist(err) {
		t.Errorf("worker LaunchAgent should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(legacyAgentPlist(opts, brokerLabel)); err != nil {
		t.Errorf("broker LaunchAgent removed early: %v", err)
	}
	if !r.CalledWith("launchctl bootout " + gui(orchardWorkerLabel)) {
		t.Error("the worker's agent should be unloaded from the user's session")
	}
}

func TestRestartDaemon(t *testing.T) {
	ctx := context.Background()
	const print = "launchctl print system/com.grove.broker"

	r := NewFakeRunner()
	r.Script(print, FakeResult{Stdout: "com.grove.broker = {\n\tactive count = 1\n\tpid = 912\n\tstate = running\n}\n"})
	restartDaemon(ctx, r, brokerLabel)
	if !r.CalledWith("kill -TERM 912") {
		t.Errorf("a running daemon is sent SIGTERM; calls: %v", r.Calls)
	}

	r = NewFakeRunner()
	r.Script(print, FakeResult{Err: errors.New("Could not find service")})
	restartDaemon(ctx, r, brokerLabel)
	r2 := NewFakeRunner()
	r2.Script(print, FakeResult{Stdout: "com.grove.broker = {\n\tstate = waiting\n}\n"})
	restartDaemon(ctx, r2, brokerLabel)
	for _, rr := range []*FakeRunner{r, r2} {
		for _, c := range rr.Calls {
			if c.Name == "kill" || (c.Name == "launchctl" && c.Args[0] != "print") {
				t.Errorf("a daemon that is not running is left alone, ran: %s", c.String())
			}
		}
	}
}

func TestWorkerPlan_CredentialChangeRestartsTheRunningWorker(t *testing.T) {
	r := NewFakeRunner()
	r.Script("launchctl print system/com.grove.orchard-worker", FakeResult{Stdout: "pid = 77\n"})
	opts := testWorkerOptions(t.TempDir())
	steps := planWorker(t, r, opts)
	if err := stepNamed(t, steps, "worker-bootstrap-credential").Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.CalledWith("kill -TERM 77") {
		t.Error("a new bootstrap credential must restart the running worker")
	}
}

func TestWorkerPlan_MovesTheWorkerCredentialOutOfKeychain(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	opts.Token = ""
	r := NewFakeRunner()
	r.Script("/usr/bin/security find-generic-password -a test-worker -s "+orchardWorkerKeychainService+" -w", FakeResult{Stdout: "old-keychain-token\n"})
	if err := os.MkdirAll(filepath.Dir(workerWrapperPath(opts)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workerWrapperPath(opts), []byte(workerWrapper(opts, "/x/orchard")), 0o700); err != nil {
		t.Fatal(err)
	}
	steps := planWorker(t, r, opts)
	step := stepNamed(t, steps, "worker-bootstrap-credential")
	if ok, _ := step.Check(context.Background()); ok {
		t.Fatal("no credential file yet")
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(credentialPath(opts, workerBootstrapCredentialName)); err != nil || string(b) != "old-keychain-token\n" {
		t.Fatalf("credential file = %q, %v", b, err)
	}
	if ok, _ := step.Check(context.Background()); !ok {
		t.Error("with the file in place a re-run without --token is satisfied")
	}
	for _, c := range r.Calls {
		if strings.Contains(c.String(), "old-keychain-token") || strings.Contains(c.String(), "delete-generic-password") {
			t.Errorf("unexpected call: %s", c.String())
		}
	}

	// A Keychain that can't be read from this shell: the error says what to do.
	r = NewFakeRunner()
	r.Script("/usr/bin/security find-generic-password -a test-worker -s "+orchardWorkerKeychainService+" -w", FakeResult{Err: errors.New("User interaction is not allowed")})
	err := stepNamed(t, planWorker(t, r, opts), "worker-bootstrap-credential").Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "security unlock-keychain") || !strings.Contains(err.Error(), "grove setup") {
		t.Errorf("err = %v, want a hint to unlock the Keychain or run grove setup", err)
	}
}

func TestWorkerPlan_RegistryLoginRewritesAMissingCredentialFile(t *testing.T) {
	// An install from before the file existed has the marker but not the file the daemon needs.
	home := t.TempDir()
	opts := testWorkerOptions(home)
	opts.RegistryUser, opts.RegistryToken = "gm2211", "ghp_tok"
	if err := os.MkdirAll(opts.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryLoginMarkerPath(opts, "ghcr.io"), []byte("gm2211 ghcr.io\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	step := stepNamed(t, planWorker(t, NewFakeRunner(), opts), "registry-login:ghcr.io")
	if ok, _ := step.Check(context.Background()); ok {
		t.Fatal("the credential file is missing")
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := step.Check(context.Background()); !ok {
		t.Fatal("satisfied once both the marker and the file match")
	}
	if err := writeSecretFile(credentialPath(opts, registryCredentialName), "gm2211\nother\n"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := step.Check(context.Background()); ok {
		t.Fatal("a rotated token must log in again")
	}

	// A newline would turn one value into two lines the wrapper reads as two values.
	opts.RegistryToken = "a\nb"
	if err := stepNamed(t, planWorker(t, NewFakeRunner(), opts), "registry-login:ghcr.io").Apply(context.Background()); err == nil {
		t.Error("a multi-line token must be refused")
	}
}

// TestWorkerPlan_SecretsNeverLeak runs the whole plan with every kind of secret and looks for them
// everywhere a secret must not be: argv, stdin of anything but `tart login`, plists, the wrapper,
// and the plan's printed text.
func TestWorkerPlan_SecretsNeverLeak(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	opts.ServerURL = "http://100.64.0.9:6130"
	opts.BrokerToken = "grove_broker-secret-3c1d"
	opts.RegistryUser, opts.RegistryToken = "gm2211", "ghp_registry-secret-77"
	opts.LookPath = lookPathOnly("tailscale", "grove")
	secrets := []string{opts.Token, opts.BrokerToken, opts.RegistryToken}

	r := NewFakeRunner()
	steps := planWorker(t, r, opts)
	var printed strings.Builder
	for _, s := range steps {
		printed.WriteString(s.Description)
	}
	var out strings.Builder
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: &out}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	printed.WriteString(out.String())

	for _, c := range r.Calls {
		for _, s := range secrets {
			if strings.Contains(c.String(), s) {
				t.Errorf("secret reached argv: %s", c.String())
			}
			if strings.Contains(c.Stdin, s) && !(c.Name == "tart" && s == opts.RegistryToken) {
				t.Errorf("secret on stdin of %s", c.String())
			}
		}
	}
	files := []string{workerWrapperPath(opts)}
	for _, d := range workerDaemons(opts) {
		staged, _ := daemonPaths(opts, d.Label)
		files = append(files, staged)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				t.Errorf("secret in %s", f)
			}
		}
	}
	for _, s := range secrets {
		if strings.Contains(printed.String(), s) {
			t.Errorf("secret in the plan's printed text")
		}
	}
	// ...and they are where they belong.
	for name, want := range map[string]string{
		workerBootstrapCredentialName: opts.Token + "\n",
		brokerCredentialName:          opts.BrokerToken + "\n",
		registryCredentialName:        "gm2211\n" + opts.RegistryToken + "\n",
	} {
		p := credentialPath(opts, name)
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v, %v", p, info, err)
		}
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s = %q", p, b)
		}
	}
}

// TestWorkerWrapper_RunsOrchardWithTheCredentials executes the wrapper with a stand-in Orchard to
// prove the shell does what the template says: the bootstrap file arrives on stdin, and the
// registry file's two lines become Tart's environment variables.
func TestWorkerWrapper_RunsOrchardWithTheCredentials(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("no zsh")
	}
	home := t.TempDir()
	opts := testWorkerOptions(home)
	orchard := filepath.Join(home, "orchard")
	if err := os.WriteFile(orchard, []byte("#!/bin/sh\necho \"args: $*\"\necho \"tart: $TART_REGISTRY_USERNAME / $TART_REGISTRY_PASSWORD\"\necho \"stdin: $(cat)\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(credentialPath(opts, workerBootstrapCredentialName), "bootstrap-123\n"); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(home, "wrapper.zsh")
	if err := os.WriteFile(script, []byte(workerWrapper(opts, orchard)), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		cmd := exec.Command(zsh, script)
		cmd.Env = []string{"HOME=" + home}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("wrapper: %v\n%s", err, out)
		}
		return string(out)
	}

	out := run()
	if !strings.Contains(out, "stdin: bootstrap-123") || !strings.Contains(out, "tart:  / \n") {
		t.Errorf("without a registry login:\n%s", out)
	}
	if strings.Contains(out[:strings.Index(out, "tart:")], "bootstrap-123") {
		t.Errorf("the credential reached argv:\n%s", out)
	}
	if err := writeSecretFile(credentialPath(opts, registryCredentialName), "gm2211\nghp_tok en\n"); err != nil {
		t.Fatal(err)
	}
	if out := run(); !strings.Contains(out, "tart: gm2211 / ghp_tok en\n") {
		t.Errorf("with a registry login:\n%s", out)
	}
}
