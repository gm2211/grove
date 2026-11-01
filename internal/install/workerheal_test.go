package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/orchard"
)

// healEnv is a scratch Mac: a home directory, a fake launchctl and a fake Orchard controller.
type healEnv struct {
	t       *testing.T
	home    string
	r       *FakeRunner
	retired []string // Orchard worker records deleted
}

func newHealEnv(t *testing.T) *healEnv {
	t.Helper()
	return &healEnv{t: t, home: t.TempDir(), r: NewFakeRunner()}
}

// opts is the options of `grove install --role worker` with only what the operator would type.
func (e *healEnv) opts() Options {
	o := testWorkerOptions(e.home)
	o.Controller, o.Token, o.Hostname = "", "", ""
	o.UsersDir = filepath.Join(e.home, "Users")
	o.LookupUID = func(name string) (int, bool) { return map[string]int{"gmecocci": 501, "other": 502}[name], true }
	o.Now = func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
	o.RetireWorker = func(_ context.Context, _ config.Endpoint, name string) error {
		e.retired = append(e.retired, name)
		return nil
	}
	return o
}

func (e *healEnv) write(path, content string, mode os.FileMode) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		e.t.Fatal(err)
	}
}

func (e *healEnv) read(path string) string {
	e.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func (e *healEnv) gone(path string) bool { _, err := os.Lstat(path); return os.IsNotExist(err) }

// install runs the unprivileged part of the plan, as `grove install` does before its root script.
func (e *healEnv) install(opts Options) ([]StepResult, string, error) {
	e.t.Helper()
	scriptTailscaleUp(e.r)
	scriptPinnedDownloads(e.r, opts)
	var out bytes.Buffer
	steps, err := BuildPlan(e.r, opts, &out)
	if err != nil {
		return nil, out.String(), err
	}
	results, err := RunPlan(context.Background(), steps, PlanOptions{Out: &out})
	return results, out.String(), err
}

// runRoot stands in for the root script: the staged plists become the installed ones and launchd
// has them loaded.
func (e *healEnv) runRoot(opts Options) {
	e.t.Helper()
	for _, d := range workerDaemons(opts) {
		staged, installed := daemonPaths(opts, d.Label)
		e.write(installed, e.read(staged), 0o644)
		e.r.Script("launchctl print system/"+d.Label, FakeResult{Stdout: "program = " + d.Program + "\n"})
	}
}

func (e *healEnv) finish(opts Options) (string, error) {
	var out bytes.Buffer
	err := FinishWorkerInstall(context.Background(), e.r, opts, &out)
	return out.String(), err
}

func needed(results []StepResult, names ...string) []string {
	var out []string
	for _, res := range results {
		for _, n := range names {
			if res.Step == n || strings.HasPrefix(res.Step, n) {
				if res.Needed {
					out = append(out, res.Step)
				}
			}
		}
	}
	return out
}

// studioLegacy lays down the Mac Studio's shape: a LaunchAgent running a hand-written
// run-worker.sh (no wrapper, no guest-agent transport) fed from worker-bootstrap.
func (e *healEnv) studioLegacy() {
	opts := e.opts()
	script := legacyWorkerScriptPath(opts)
	e.write(script, "#!/bin/zsh\nexec /Users/argos/.local/bin/orchard worker run --name studio --labels host=studio --bootstrap-token-stdin http://100.101.229.60:6120 < "+
		legacyWorkerBootstrapPath(opts)+"\n", 0o755)
	e.write(legacyWorkerBootstrapPath(opts), "legacy-token\n", 0o600)
	e.write(legacyAgentPlist(opts, orchardWorkerLabel),
		`<plist version="1.0"><dict><key>Label</key><string>com.grove.orchard-worker</string><key>ProgramArguments</key><array><string>`+script+`</string></array></dict></plist>`, 0o644)
}

const fleetYAML = `# pools
pools:
    - name: linux
      image: ghcr.io/gm2211/grove-linux-worker:latest
      perWorker: 2
      workerSelector:
        host: studio
    - name: macos # the MacBook's
      image: ghcr.io/gm2211/grove-macos-worker:latest
      perWorker: 1
      workerSelector:
        host: macbook
`

func (e *healEnv) controlPlane() string {
	opts := e.opts()
	fleetPath := filepath.Join(opts.ConfigDir(), "fleet.yaml")
	e.write(fleetPath, fleetYAML, 0o644)
	e.write(filepath.Join(opts.ConfigDir(), "config.yaml"),
		"orchard:\n  url: https://cp:6120\n  token: admin\nfleet: fleet.yaml\n", 0o600)
	return fleetPath
}

// (a) The Studio's legacy shape keeps its identity, gets the current launch shape, and the old
// pieces go away, backed up.
func TestHeal_StudioLegacyShapeKeepsIdentityAndMigrates(t *testing.T) {
	e := newHealEnv(t)
	e.studioLegacy()
	opts := e.opts()
	fleetPath := e.controlPlane()
	before := e.read(fleetPath)

	results, out, err := e.install(opts)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	wrapper := e.read(workerWrapperPath(opts))
	for _, want := range []string{`--name "studio"`, `--labels "host=studio"`, "--guest-agent-transport", `"http://100.101.229.60:6120"`} {
		if !strings.Contains(wrapper, want) {
			t.Errorf("wrapper lacks %s:\n%s", want, wrapper)
		}
	}
	if got := e.read(credentialPath(opts, workerBootstrapCredentialName)); strings.TrimSpace(got) != "legacy-token" {
		t.Errorf("the bootstrap token was not migrated into the credential file: %q", got)
	}
	rec, ok := readWorkerIdentityRecord(opts)
	if !ok || rec.Name != "studio" || rec.Labels != "host=studio" || len(rec.Pending) != 0 {
		t.Errorf("identity record = %+v, %v; want studio / host=studio, nothing pending", rec, ok)
	}
	bdir := backupDir(opts)
	for _, name := range []string{"grove__run-worker.sh", "grove__worker-bootstrap", "LaunchAgents__" + orchardWorkerLabel + ".plist"} {
		if _, err := os.Stat(filepath.Join(bdir, name)); err != nil {
			t.Errorf("not backed up (%s): %v", name, err)
		}
	}
	// The worker daemon is not loaded yet: nothing is removed before its replacement runs.
	if e.gone(legacyWorkerScriptPath(opts)) || e.gone(legacyAgentPlist(opts, orchardWorkerLabel)) {
		t.Error("the legacy launch shape was removed before the LaunchDaemon was loaded")
	}
	if len(needed(results, "worker-rename")) != 0 {
		t.Error("a same-identity heal must not plan a rename")
	}

	e.runRoot(opts)
	if out, err := e.finish(opts); err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	for _, p := range []string{legacyWorkerScriptPath(opts), legacyWorkerBootstrapPath(opts), legacyAgentPlist(opts, orchardWorkerLabel)} {
		if !e.gone(p) {
			t.Errorf("%s should be removed once the LaunchDaemon is loaded", p)
		}
	}
	if !e.r.CalledWith("launchctl bootout " + gui(orchardWorkerLabel)) {
		t.Error("the legacy LaunchAgent should be booted out of the gui domain")
	}
	if len(e.retired) != 0 || e.read(fleetPath) != before {
		t.Errorf("no fleet change expected; retired=%v", e.retired)
	}
}

// (b) A second run with nothing to change changes nothing and restarts nothing.
func TestHeal_SecondRunIsANoOp(t *testing.T) {
	e := newHealEnv(t)
	e.studioLegacy()
	opts := e.opts()
	if _, out, err := e.install(opts); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	e.runRoot(opts)
	if out, err := e.finish(opts); err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}

	wrapperBefore := e.read(workerWrapperPath(opts))
	identityBefore := e.read(workerIdentityPath(opts))
	calls := len(e.r.Calls)
	results, out, err := e.install(e.opts())
	if err != nil {
		t.Fatalf("second install: %v\n%s", err, out)
	}
	if n := needed(results, "worker-identity", "backup-legacy-worker", "worker-bootstrap-credential", "orchard-worker-wrapper",
		"daemon-stage:", "worker-daemons", "retire-launchagents", "retire-other-user-stacks", "worker-rename"); len(n) != 0 {
		t.Errorf("the second run still has work to do: %v", n)
	}
	if e.read(workerWrapperPath(opts)) != wrapperBefore || e.read(workerIdentityPath(opts)) != identityBefore {
		t.Error("the second run rewrote the wrapper or identity")
	}
	for _, c := range e.r.Calls[calls:] {
		if c.Name == "kill" || (c.Name == "launchctl" && len(c.Args) > 0 && c.Args[0] != "print") {
			t.Errorf("the second run touched launchd: %s", c)
		}
	}
	if out, err := e.finish(e.opts()); err != nil || strings.Contains(out, "removed") || strings.Contains(out, "deleted") {
		t.Errorf("finish on a healed worker should do nothing: %v\n%s", err, out)
	}
	if len(e.retired) != 0 {
		t.Errorf("records deleted on a no-op: %v", e.retired)
	}
}

// (c) An explicit --name on the control plane retires the old record and rewrites the selectors.
func TestHeal_RenameOnControlPlaneRewritesFleetAndDeletesRecord(t *testing.T) {
	e := newHealEnv(t)
	e.studioLegacy()
	fleetPath := e.controlPlane()
	rename := e.opts()
	rename.Hostname, rename.Token = "studio-2", "token-for-studio-2"
	if _, out, err := e.install(rename); err != nil {
		t.Fatalf("rename install: %v\n%s", err, out)
	}
	wrapper := e.read(workerWrapperPath(rename))
	if !strings.Contains(wrapper, `--name "studio-2"`) || !strings.Contains(wrapper, `--labels "host=studio-2"`) {
		t.Errorf("wrapper does not carry the new identity:\n%s", wrapper)
	}
	rec, _ := readWorkerIdentityRecord(rename)
	if len(rec.Pending) != 1 || rec.Pending[0].Name != "studio" {
		t.Fatalf("the old identity must stay pending until the rename is finished: %+v", rec)
	}
	if len(e.retired) != 0 || !strings.Contains(e.read(fleetPath), "host: studio\n") {
		t.Fatal("fleet and the old record must not change before the new worker is loaded")
	}

	e.runRoot(rename)
	out, err := e.finish(rename)
	if err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	fleetNow := e.read(fleetPath)
	if !strings.Contains(fleetNow, "host: studio-2") || strings.Contains(fleetNow, "host: studio\n") {
		t.Errorf("linux pool selector not rewritten:\n%s", fleetNow)
	}
	if !strings.Contains(fleetNow, "host: macbook") || !strings.Contains(fleetNow, "# pools") || !strings.Contains(fleetNow, "# the MacBook's") {
		t.Errorf("another worker's pool or the comments were disturbed:\n%s", fleetNow)
	}
	if len(e.retired) != 1 || e.retired[0] != "studio" {
		t.Errorf("old worker record not deleted: %v", e.retired)
	}
	if _, err := os.Stat(filepath.Join(backupDir(rename), "grove__fleet.yaml")); err != nil {
		t.Errorf("fleet.yaml not backed up: %v", err)
	}
	if rec, _ := readWorkerIdentityRecord(rename); len(rec.Pending) != 0 || rec.Name != "studio-2" {
		t.Errorf("identity after the rename = %+v", rec)
	}
	// A later run neither re-deletes nor re-edits.
	if _, err := e.finish(e.opts()); err != nil || len(e.retired) != 1 {
		t.Errorf("rename repeated: %v %v", err, e.retired)
	}
}

// (d) A rename on a host that is not the control plane refuses and prints the exact change.
func TestHeal_RenameOffControlPlaneRefusesUnlessForced(t *testing.T) {
	setup := func(t *testing.T) (*healEnv, Options) {
		e := newHealEnv(t)
		e.studioLegacy()
		opts := e.opts()
		if _, out, err := e.install(opts); err != nil {
			t.Fatalf("install: %v\n%s", err, out)
		}
		rename := e.opts()
		rename.Hostname, rename.Token = "studio-2", "tok"
		return e, rename
	}

	t.Run("refuses with the exact fleet change", func(t *testing.T) {
		e, rename := setup(t)
		_, _, err := e.install(rename)
		if err == nil {
			t.Fatal("expected a refusal")
		}
		for _, want := range []string{"change `host: studio` to `host: studio-2`", `delete the old worker record "studio"`, "--force"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal lacks %q:\n%v", want, err)
			}
		}
		if rec, _ := readWorkerIdentityRecord(rename); rec.Name != "studio" {
			t.Errorf("a refused rename changed the recorded identity: %+v", rec)
		}
	})
	t.Run("--force proceeds and prints the change", func(t *testing.T) {
		e, rename := setup(t)
		rename.Force = true
		_, out, err := e.install(rename)
		if err != nil {
			t.Fatalf("forced rename: %v\n%s", err, out)
		}
		if !strings.Contains(out, "change `host: studio` to `host: studio-2`") {
			t.Errorf("forced rename must still print the change:\n%s", out)
		}
		if rec, _ := readWorkerIdentityRecord(rename); rec.Name != "studio-2" || len(rec.Pending) != 0 {
			t.Errorf("identity = %+v", rec)
		}
		e.runRoot(rename)
		if _, err := e.finish(rename); err != nil || len(e.retired) != 0 {
			t.Errorf("a host without the control plane must not delete records: %v %v", err, e.retired)
		}
	})
	t.Run("a rename needs a credential minted for the new name", func(t *testing.T) {
		e, rename := setup(t)
		rename.Token = ""
		if _, _, err := e.install(rename); err == nil || !strings.Contains(err.Error(), "--token") {
			t.Errorf("err = %v, want one asking for --token", err)
		}
	})
}

// (e) An older per-user stack for the same worker is removed; another worker's is not.
func TestHeal_SupersededPerUserStackIsRemoved(t *testing.T) {
	e := newHealEnv(t)
	opts := e.opts()
	agent := func(user, name string) string {
		return filepath.Join(opts.UsersDir, user, "Library", "LaunchAgents", orchardWorkerLabel+".plist")
	}
	plist := func(name, script string) string {
		return `<plist><dict><key>ProgramArguments</key><array><string>` + script + `</string><string>worker</string><string>run</string><string>--name</string><string>` + name + `</string></array></dict></plist>`
	}
	mine := filepath.Join(opts.UsersDir, "gmecocci", ".config", "grove", "run-worker.sh")
	e.write(mine, "#!/bin/sh\n", 0o755)
	e.write(agent("gmecocci", "studio"), plist("studio", mine), 0o644)
	broker := filepath.Join(opts.UsersDir, "gmecocci", "Library", "LaunchAgents", brokerLabel+".plist")
	e.write(broker, "<plist><dict><key>ProgramArguments</key><array><string>/opt/homebrew/bin/grove</string></array></dict></plist>", 0o644)
	e.write(agent("other", "elsewhere"), plist("elsewhere", "/usr/local/bin/orchard"), 0o644)
	// The worker being installed keeps its name, from its own wrapper.
	e.write(workerWrapperPath(opts), workerWrapper(Options{Hostname: "studio", Controller: "https://cp:6120", Home: e.home}, "/x/orchard"), 0o700)

	opts.Controller, opts.Token = "https://cp:6120", "tok"
	results, out, err := e.install(opts)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if n := needed(results, "retire-other-user-stacks"); len(n) != 1 {
		t.Fatalf("the superseded stack should be planned for removal: %v", n)
	}
	scriptText, err := workerStack(t, e, opts).Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/bin/launchctl bootout gui/501/" + orchardWorkerLabel,
		"/bin/launchctl bootout gui/501/" + brokerLabel,
		"/bin/rm -f " + shQuote(agent("gmecocci", "")),
		"/bin/rm -f " + shQuote(mine),
		"/bin/cp -p " + shQuote(agent("gmecocci", "")),
	} {
		if !strings.Contains(scriptText, want) {
			t.Errorf("root script lacks %q:\n%s", want, scriptText)
		}
	}
	if strings.Contains(scriptText, "/other/") || strings.Contains(scriptText, "gui/502") {
		t.Errorf("another worker's stack must be left alone:\n%s", scriptText)
	}
	// Once root has run it, the check is satisfied.
	for _, p := range []string{agent("gmecocci", ""), broker, mine} {
		_ = os.Remove(p)
	}
	if ok, _ := workerStack(t, e, opts).Check(context.Background()); !ok {
		t.Error("the step should be satisfied once the stack is gone")
	}
}

func workerStack(t *testing.T, e *healEnv, opts Options) Step {
	t.Helper()
	scriptTailscaleUp(e.r)
	scriptPinnedDownloads(e.r, opts)
	steps, err := BuildPlan(e.r, opts, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return stepNamed(t, steps, "retire-other-user-stacks")
}

// (f) Every plist grove renders runs an absolute program.
func TestHeal_RenderedPlistsUseAbsolutePrograms(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	opts.LookPath = func(name string) (string, error) { return "bin/" + name, nil } // relative on purpose
	for _, d := range workerDaemons(opts) {
		plist, err := d.Render()
		if err != nil {
			t.Fatalf("%s: %v", d.Label, err)
		}
		path := filepath.Join(t.TempDir(), d.Label+".plist")
		if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
			t.Fatal(err)
		}
		if prog := plistProgram(path); !filepath.IsAbs(prog) {
			t.Errorf("%s runs %q, not an absolute path", d.Label, prog)
		}
	}
	if _, err := RenderLaunchAgent(LaunchAgentSpec{Label: "com.grove.x", Program: "nomad"}); err == nil {
		t.Error("a bare program name must not render: launchd cannot find it (exit 78)")
	}
	if p := supervisorProgram(opts, "nomad"); !filepath.IsAbs(p) {
		t.Errorf("supervisorProgram = %q", p)
	}
}

func TestDetectWorkerIdentity(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(e *healEnv, opts Options)
		wantName   string
		wantLabels string
		wantCtl    string
	}{
		{"nothing installed", func(*healEnv, Options) {}, "", "", ""},
		{"recorded identity wins", func(e *healEnv, o Options) {
			e.write(workerIdentityPath(o), renderWorkerIdentityRecord(workerIdentity{Name: "rec", Labels: "host=rec"}), 0o600)
			e.write(legacyWorkerScriptPath(o), "orchard worker run --name other http://c:6120", 0o755)
		}, "rec", "host=rec", "http://c:6120"},
		{"grove wrapper", func(e *healEnv, o Options) {
			o.Hostname, o.Labels, o.Controller = "w", "host=w,arch=arm64", "https://cp:6120"
			e.write(workerWrapperPath(o), workerWrapper(o, "/x/orchard"), 0o700)
		}, "w", "host=w,arch=arm64", "https://cp:6120"},
		{"run-worker.sh, positional controller, quoted", func(e *healEnv, o Options) {
			e.write(legacyWorkerScriptPath(o), "#!/bin/sh\n# comment --name nope\nexec orchard worker run \\\n  --name 'a b' --labels \"host=a\" --labels arch=arm64 http://c:6120 < f\n", 0o755)
		}, "a b", "host=a,arch=arm64", "http://c:6120"},
		{"LaunchAgent with inline arguments", func(e *healEnv, o Options) {
			e.write(legacyAgentPlist(o, orchardWorkerLabel), `<array><string>/x/orchard</string><string>worker</string><string>run</string><string>--name=s</string><string>--labels=host=s</string><string>http://c:6120</string></array>`, 0o644)
		}, "s", "host=s", "http://c:6120"},
		{"LaunchAgent that runs a script", func(e *healEnv, o Options) {
			script := filepath.Join(e.home, "launch.sh")
			e.write(script, "orchard worker run --name viaScript --labels host=viaScript http://c:6120\n", 0o755)
			e.write(legacyAgentPlist(o, orchardWorkerLabel), `<key>ProgramArguments</key><array><string>`+script+`</string></array>`, 0o644)
		}, "viaScript", "host=viaScript", "http://c:6120"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newHealEnv(t)
			opts := e.opts()
			tt.setup(e, opts)
			got := detectWorkerIdentity(opts)
			if got.Name != tt.wantName || got.Labels != tt.wantLabels || got.Controller != tt.wantCtl {
				t.Errorf("detectWorkerIdentity = %+v, want %q / %q / %q", got, tt.wantName, tt.wantLabels, tt.wantCtl)
			}
		})
	}
}

func TestChooseWorkerIdentity(t *testing.T) {
	prev := workerIdentity{Name: "studio", Labels: "host=studio,arch=arm64,gpu=yes"}
	tests := []struct {
		name       string
		opts       Options
		prev       workerIdentity
		wantName   string
		wantLabels string
	}{
		{"new install: hostname", Options{Hostname: ""}, workerIdentity{}, "", ""}, // filled below from os.Hostname
		{"reuse installed", Options{}, prev, "studio", "host=studio,arch=arm64,gpu=yes"},
		{"rename carries labels over", Options{Hostname: "s2"}, prev, "s2", "host=s2,arch=arm64,gpu=yes"},
		{"explicit labels win", Options{Labels: "host=x"}, prev, "studio", "host=x"},
		{"legacy without labels gets the default", Options{}, workerIdentity{Name: "studio"}, "studio", "host=studio,arch=arm64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chooseWorkerIdentity(tt.opts, tt.prev)
			if tt.wantName == "" {
				host, _ := os.Hostname()
				if got.Name != host || got.Labels != defaultWorkerLabels(host) {
					t.Errorf("new install = %+v, want the hostname's", got)
				}
				return
			}
			if got.Name != tt.wantName || got.Labels != tt.wantLabels {
				t.Errorf("= %q %q, want %q %q", got.Name, got.Labels, tt.wantName, tt.wantLabels)
			}
		})
	}
}

type fakeOrchard struct {
	orchard.Client
	workers []orchard.Worker
}

func (f fakeOrchard) ListWorkers(context.Context) ([]orchard.Worker, error) { return f.workers, nil }

func TestCheckWorkerShape(t *testing.T) {
	ctx := context.Background()

	t.Run("legacy launch shape is flagged with the fix", func(t *testing.T) {
		e := newHealEnv(t)
		e.studioLegacy()
		got := checkWorkerShape(ctx, e.opts(), nil)
		if got.OK || !strings.Contains(got.Remediation, "grove install --role worker") {
			t.Fatalf("got %+v", got)
		}
		for _, want := range []string{"run-worker.sh", "second worker", "legacy credential"} {
			if !strings.Contains(got.Detail, want) {
				t.Errorf("detail lacks %q: %s", want, got.Detail)
			}
		}
	})
	t.Run("a wrapper without the guest-agent transport and a bare program path", func(t *testing.T) {
		e := newHealEnv(t)
		opts := e.opts()
		e.write(workerWrapperPath(opts), "#!/bin/zsh\nexec orchard worker run --name w\n", 0o700)
		e.write(filepath.Join(opts.launchDaemonsDir(), "com.grove.nomad.plist"), `<key>ProgramArguments</key><array><string>nomad</string></array>`, 0o644)
		got := checkWorkerShape(ctx, opts, nil)
		if got.OK || !strings.Contains(got.Detail, "--guest-agent-transport") || !strings.Contains(got.Detail, "bare program nomad") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a duplicate stack under another account", func(t *testing.T) {
		e := newHealEnv(t)
		opts := e.opts()
		e.write(workerIdentityPath(opts), renderWorkerIdentityRecord(workerIdentity{Name: "studio", Labels: "host=studio"}), 0o600)
		e.write(filepath.Join(opts.UsersDir, "gmecocci", "Library", "LaunchAgents", orchardWorkerLabel+".plist"),
			`<array><string>worker</string><string>run</string><string>--name</string><string>studio</string></array>`, 0o644)
		got := checkWorkerShape(ctx, opts, nil)
		if got.OK || !strings.Contains(got.Detail, "gmecocci") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("identity that no pool selector matches", func(t *testing.T) {
		e := newHealEnv(t)
		fleetPath := e.controlPlane()
		opts := e.opts()
		e.write(workerIdentityPath(opts), renderWorkerIdentityRecord(workerIdentity{Name: "gm2211s-Mac-Studio.local", Labels: "host=gm2211s-Mac-Studio.local"}), 0o600)
		opts.OrchardClient = func(config.Endpoint) (orchard.Client, error) {
			return fakeOrchard{workers: []orchard.Worker{
				{Name: "gm2211s-Mac-Studio.local", Labels: map[string]string{"host": "gm2211s-Mac-Studio.local"}},
				{Name: "mb", Labels: map[string]string{"host": "macbook"}},
			}}, nil
		}
		got := checkWorkerShape(ctx, opts, &config.Config{Fleet: fleetPath})
		if got.OK || !strings.Contains(got.Detail, "linux") || strings.Contains(got.Detail, "pool(s) linux, macos") || !strings.Contains(got.Remediation, "--name") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a healed worker is fine", func(t *testing.T) {
		e := newHealEnv(t)
		opts := e.opts()
		e.write(workerWrapperPath(opts), workerWrapper(Options{Hostname: "studio", Home: e.home}, "/x/orchard"), 0o700)
		if got := checkWorkerShape(ctx, opts, nil); !got.OK {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestRewriteFleetSelectors(t *testing.T) {
	t.Run("only pools the old labels satisfied", func(t *testing.T) {
		out, edits, err := rewriteFleetSelectors([]byte(fleetYAML), "host=studio", "host=s2")
		if err != nil || len(edits) != 1 || edits[0].Pool != "linux" {
			t.Fatalf("edits=%v err=%v", edits, err)
		}
		if !strings.Contains(string(out), "host: s2") || !strings.Contains(string(out), "host: macbook") {
			t.Errorf("out:\n%s", out)
		}
	})
	t.Run("already rewritten is untouched", func(t *testing.T) {
		out, edits, _ := rewriteFleetSelectors([]byte(fleetYAML), "host=gone", "host=s2")
		if len(edits) != 0 || string(out) != fleetYAML {
			t.Errorf("edits=%v", edits)
		}
	})
	t.Run("a label the new worker lacks is an error", func(t *testing.T) {
		if _, _, err := rewriteFleetSelectors([]byte(fleetYAML), "host=studio", "arch=arm64"); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestRetireWorkerRecord_NeedsCredentials(t *testing.T) {
	err := Options{}.retireWorkerRecord(context.Background(), config.Endpoint{URL: "https://cp"}, "x")
	if err == nil {
		t.Error("expected an error without an Orchard credential")
	}
}

// A rename on a Mac whose LaunchDaemon is already loaded restarts the worker onto the new wrapper
// in the same run, so the control plane's side is finished in that run too.
func TestHeal_RenameWithLoadedDaemonFinishesInOneRun(t *testing.T) {
	e := newHealEnv(t)
	fleetPath := e.controlPlane()
	first := e.opts()
	first.Hostname, first.Controller, first.Token = "studio", "https://cp:6120", "tok"
	if _, out, err := e.install(first); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	e.runRoot(first)

	rename := e.opts()
	rename.Hostname, rename.Token = "studio-2", "tok2"
	if _, out, err := e.install(rename); err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	if !strings.Contains(e.read(fleetPath), "host: studio-2") || len(e.retired) != 1 || e.retired[0] != "studio" {
		t.Errorf("rename not finished: retired=%v\n%s", e.retired, e.read(fleetPath))
	}
	if rec, _ := readWorkerIdentityRecord(rename); len(rec.Pending) != 0 {
		t.Errorf("pending left over: %+v", rec)
	}
}
