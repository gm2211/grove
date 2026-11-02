package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func init() { agentRecheckDelay = 0 }

// rootlessEnv is a scratch Mac for a rootless worker: launchd reports every job loaded and
// running its program, like a Mac whose user is logged in.
func rootlessEnv(t *testing.T) (*healEnv, Options) {
	t.Helper()
	e := newHealEnv(t)
	opts := e.opts()
	opts.Controller, opts.Token, opts.Hostname = "https://cp.tailnet.ts.net:6120", "worker-secret-6f32f988", "test-worker"
	opts.Rootless = true
	e.agentsRunning(opts)
	return e, opts
}

func (e *healEnv) agentsRunning(opts Options) {
	for _, d := range workerDaemons(opts) {
		e.r.Script(fmt.Sprintf("launchctl print gui/%d/%s", os.Getuid(), d.Label), FakeResult{Stdout: "program = " + d.Program + "\n\tpid = 4242\n"})
	}
}

func hasStep(steps []Step, name string) bool {
	for _, s := range steps {
		if s.Name == name {
			return true
		}
	}
	return false
}

func callIndex(r *FakeRunner, cmdLine string) int {
	for i, c := range r.Calls {
		if c.String() == cmdLine {
			return i
		}
	}
	return -1
}

func countCalls(r *FakeRunner, prefix string) int {
	n := 0
	for _, c := range r.Calls {
		if strings.HasPrefix(c.String(), prefix) {
			n++
		}
	}
	return n
}

func TestRootless_PlanReplacesTheRootSteps(t *testing.T) {
	e, opts := rootlessEnv(t)
	scriptTailscaleUp(e.r)
	scriptPinnedDownloads(e.r, opts)
	opts.Softnet = true
	steps, err := BuildPlan(e.r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"worker-agents", "keep-awake", "host-firewall", "softnet-root", "worker-mode"} {
		if !hasStep(steps, want) {
			t.Errorf("rootless plan lacks %s: %v", want, stepNames(steps))
		}
	}
	for _, gone := range []string{"worker-daemons", "disable-sleep", "daemon-stage:orchard-worker", "daemon-stage:broker", "host-firewall-stage"} {
		if hasStep(steps, gone) {
			t.Errorf("rootless plan still has %s", gone)
		}
	}
	for _, s := range steps {
		if s.Privileged {
			t.Errorf("rootless plan has the privileged step %s", s.Name)
		}
	}
}

func TestRootless_ChosenByConfigButNotAgainstTheFlag(t *testing.T) {
	e := newHealEnv(t)
	opts := e.opts()
	opts.Controller, opts.Token, opts.Hostname = "https://cp.tailnet.ts.net:6120", "worker-secret-6f32f988", "test-worker"
	if err := config.Save(configFilePath(opts), &config.Config{Worker: config.WorkerConfig{Rootless: true}}); err != nil {
		t.Fatal(err)
	}
	scriptTailscaleUp(e.r)
	scriptPinnedDownloads(e.r, opts)
	steps, err := BuildPlan(e.r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !hasStep(steps, "worker-agents") || hasStep(steps, "worker-daemons") {
		t.Errorf("worker.rootless in config.yaml should keep the install rootless: %v", stepNames(steps))
	}

	opts.NoAutoRootless = true // --rootless=false
	steps, err = BuildPlan(e.r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if hasStep(steps, "worker-agents") || !hasStep(steps, "worker-daemons") {
		t.Errorf("--rootless=false should win over config.yaml: %v", stepNames(steps))
	}
}

func TestAutoRootless(t *testing.T) {
	ctx := context.Background()
	scripted := func(groups string) (*FakeRunner, Options) {
		e := newHealEnv(t)
		e.r.Script("/usr/bin/id -Gn", FakeResult{Stdout: groups})
		return e.r, e.opts()
	}

	r, opts := scripted("staff everyone")
	var out bytes.Buffer
	if !AutoRootless(ctx, r, opts, &out) {
		t.Fatal("a user who isn't an admin should get a rootless install")
	}
	for _, want := range []string{"firewall", "isolated", "logged in"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the notice should say what is lost (%q):\n%s", want, out.String())
		}
	}

	r, opts = scripted("staff admin everyone")
	if AutoRootless(ctx, r, opts, io.Discard) {
		t.Error("an admin keeps the root-based install")
	}

	r, opts = scripted("staff everyone")
	opts.NoAutoRootless = true
	if AutoRootless(ctx, r, opts, io.Discard) {
		t.Error("--rootless=false must turn the automatic choice off")
	}

	r, opts = scripted("staff everyone")
	_, installed := daemonPaths(opts, orchardWorkerLabel)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if AutoRootless(ctx, r, opts, io.Discard) {
		t.Error("a worker an admin already installed as LaunchDaemons must not flip to agents on its own")
	}
}

func TestRootless_InstallRendersAbsoluteAgentsAndLoadsThemInTheGUIDomain(t *testing.T) {
	e, opts := rootlessEnv(t)
	results, out, err := e.install(opts)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	gui := fmt.Sprintf("gui/%d", os.Getuid())
	for _, d := range workerDaemons(opts) {
		plist := agentPlistPath(opts, d.Label)
		body := e.read(plist)
		if strings.Contains(body, "UserName") {
			t.Errorf("%s: a LaunchAgent has no UserName:\n%s", d.Label, body)
		}
		if want, _ := RenderLaunchAgent(d.Spec); body != want {
			t.Errorf("%s is not the rendered job:\n%s", plist, body)
		}
		for _, key := range []string{"<key>KeepAlive</key>\n\t<true/>", "<key>RunAtLoad</key>\n\t<true/>", filepath.Join(opts.LogDir(), d.Name+".log")} {
			if !strings.Contains(body, key) {
				t.Errorf("%s lacks %q", plist, key)
			}
		}
		if prog := plistProgram(plist); !filepath.IsAbs(prog) {
			t.Errorf("%s runs %q, which launchd can't find from its minimal PATH", plist, prog)
		}
		boot, bs := callIndex(e.r, "launchctl bootout "+gui+"/"+d.Label), callIndex(e.r, "launchctl bootstrap "+gui+" "+plist)
		if boot < 0 || bs < 0 || boot > bs {
			t.Errorf("%s: want bootout then bootstrap in %s, got bootout@%d bootstrap@%d", d.Label, gui, boot, bs)
		}
	}
	if !strings.Contains(e.read(agentPlistPath(opts, orchardWorkerLabel)), workerWrapperPath(opts)) {
		t.Error("the worker agent should run the same wrapper the daemon does")
	}
	if got := e.read(agentPlistPath(opts, brokerLabel)); !strings.Contains(got, "<string>broker</string>") {
		t.Errorf("the broker agent should run `grove broker`:\n%s", got)
	}
	if cfg, err := config.LoadFrom(configFilePath(opts)); err != nil || !cfg.Worker.Rootless {
		t.Errorf("config.yaml should record worker.rootless: %+v %v", cfg, err)
	}
	for _, res := range results {
		if res.Skipped {
			t.Errorf("a rootless install leaves nothing for root: %s was skipped", res.Step)
		}
	}
	if countCalls(e.r, "sudo") > 0 || countCalls(e.r, "/usr/bin/sudo") > 0 {
		t.Error("a rootless install must not run sudo")
	}
}

func TestRootless_RerunChangesAndRestartsNothing(t *testing.T) {
	e, opts := rootlessEnv(t)
	if _, out, err := e.install(opts); err != nil {
		t.Fatalf("first install: %v\n%s", err, out)
	}
	before := countCalls(e.r, "launchctl bootstrap")
	if before != 3 { // worker, broker, keep-awake
		t.Fatalf("first install bootstrapped %d jobs, want 3", before)
	}
	e.r.Calls = nil
	results, out, err := e.install(opts)
	if err != nil {
		t.Fatalf("second install: %v\n%s", err, out)
	}
	if n := needed(results, "worker-agents", "keep-awake", "worker-mode", "retire-launchagents"); len(n) != 0 {
		t.Errorf("second run still needs %v", n)
	}
	if countCalls(e.r, "launchctl bootstrap")+countCalls(e.r, "launchctl bootout")+countCalls(e.r, "kill") != 0 {
		t.Errorf("a no-op re-run must not touch launchd: %v", e.r.Calls)
	}
}

func TestRootless_OldShapeAgentIsReplacedInPlaceAndTheNewOnesSurviveRetire(t *testing.T) {
	e, opts := rootlessEnv(t)
	opts.Controller, opts.Token, opts.Hostname = "", "", "" // healing: everything comes from the old install
	e.studioLegacy()
	oldPlist := agentPlistPath(opts, orchardWorkerLabel)
	old := e.read(oldPlist)

	if _, out, err := e.install(opts); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	now := e.read(oldPlist)
	want, _ := RenderLaunchAgent(workerDaemons(opts)[0].Spec)
	if now == old || now != want {
		t.Errorf("the old-shape agent at %s was not replaced in place:\n%s", oldPlist, now)
	}
	if got, err := os.ReadFile(filepath.Join(backupDir(opts), "LaunchAgents__"+orchardWorkerLabel+".plist")); err != nil || string(got) != old {
		t.Errorf("the old agent should be backed up first: %v", err)
	}
	// retire-launchagents removed the truly legacy pieces, now that the new agent runs...
	for _, p := range []string{legacyWorkerScriptPath(opts), legacyWorkerBootstrapPath(opts)} {
		if !e.gone(p) {
			t.Errorf("%s should be retired once the agent runs", p)
		}
	}
	// ...and left the agents rootless mode just installed.
	for _, label := range []string{orchardWorkerLabel, brokerLabel} {
		if e.gone(agentPlistPath(opts, label)) {
			t.Errorf("retire-launchagents removed the new %s agent", label)
		}
	}
	if n := countCalls(e.r, "launchctl bootout gui/"+fmt.Sprint(os.Getuid())+"/"+orchardWorkerLabel); n < 1 {
		t.Error("the old job must be booted out of the gui domain before the new one loads")
	}
}

func TestRootless_RetireKeepsTheLegacyFilesUntilTheAgentRuns(t *testing.T) {
	e, opts := rootlessEnv(t)
	opts.Controller, opts.Token, opts.Hostname = "", "", ""
	e.studioLegacy()
	e.r.Script(fmt.Sprintf("launchctl print gui/%d/%s", os.Getuid(), orchardWorkerLabel), FakeResult{Err: os.ErrNotExist})
	results, _, err := e.install(opts)
	if err == nil {
		t.Fatalf("worker-agents must fail when the worker is not loaded afterwards: %v", results)
	}
	if e.gone(legacyWorkerScriptPath(opts)) || e.gone(legacyWorkerBootstrapPath(opts)) {
		t.Error("run-worker.sh and worker-bootstrap must stay until the new agent runs")
	}
}

func TestRootless_KeepAwakeAgent(t *testing.T) {
	e, opts := rootlessEnv(t)
	if _, out, err := e.install(opts); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	plist := agentPlistPath(opts, keepAwakeLabel)
	body := e.read(plist)
	for _, want := range []string{"<string>com.grove.keepawake</string>", "<string>/usr/bin/caffeinate</string>", "<string>-ims</string>", "<key>KeepAlive</key>\n\t<true/>"} {
		if !strings.Contains(body, want) {
			t.Errorf("keep-awake plist lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "UserName") {
		t.Error("keep-awake is a LaunchAgent: no UserName")
	}
	if !e.r.CalledWith("launchctl bootstrap " + fmt.Sprintf("gui/%d", os.Getuid()) + " " + plist) {
		t.Error("keep-awake agent was not bootstrapped into the gui domain")
	}
	if e.r.CalledWith("pmset -g") || e.r.CalledWith("sudo pmset -a disablesleep 1") {
		t.Error("rootless must not touch pmset")
	}
}

func TestRootless_ExistingCaffeinateAgentCountsAsKeepAwake(t *testing.T) {
	e, opts := rootlessEnv(t)
	other := filepath.Join(opts.LaunchAgentsDir(), "com.gmecocci.caffeinate.plist")
	e.write(other, `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.gmecocci.caffeinate</string>
<key>ProgramArguments</key><array><string>/usr/bin/caffeinate</string><string>-ims</string></array>
<key>KeepAlive</key><true/><key>RunAtLoad</key><true/>
</dict></plist>
`, 0o644)
	e.r.Script(fmt.Sprintf("launchctl print gui/%d/com.gmecocci.caffeinate", os.Getuid()), FakeResult{Stdout: "program = /usr/bin/caffeinate\n\tpid = 99\n"})
	// grove's own agent is not there: launchd has never heard of it.
	e.r.Script(fmt.Sprintf("launchctl print gui/%d/%s", os.Getuid(), keepAwakeLabel), FakeResult{Err: os.ErrNotExist})

	results, out, err := e.install(opts)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if n := needed(results, "keep-awake"); len(n) != 0 {
		t.Errorf("an existing caffeinate agent should satisfy keep-awake, got %v", n)
	}
	if !e.gone(agentPlistPath(opts, keepAwakeLabel)) {
		t.Error("a second keep-awake agent was installed next to the user's own")
	}
	if label, ok := existingKeepAwake(context.Background(), e.r, opts); !ok || label != "com.gmecocci.caffeinate" {
		t.Errorf("existingKeepAwake = %q, %v", label, ok)
	}
	if c := checkKeepAwake(context.Background(), e.r, opts); !c.OK || !strings.Contains(c.Detail, "com.gmecocci.caffeinate") {
		t.Errorf("doctor: %+v", c)
	}

	// An agent that runs caffeinate with no flag that holds off sleep does not count.
	e.write(other, strings.Replace(e.read(other), "<string>-ims</string>", "<string>-u</string>", 1), 0o644)
	if _, ok := existingKeepAwake(context.Background(), e.r, opts); ok {
		t.Error("caffeinate -u does not keep the Mac awake")
	}
}

func TestRootless_FirewallIsSkippedWithWhatIsLost(t *testing.T) {
	e, opts := rootlessEnv(t)
	opts.Softnet = true
	scriptTailscaleUp(e.r)
	scriptPinnedDownloads(e.r, opts)
	var out bytes.Buffer
	steps, err := BuildPlan(e.r, opts, &out)
	if err != nil {
		t.Fatal(err)
	}
	results, err := RunPlan(context.Background(), steps, PlanOptions{Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"[skip]  host-firewall",
		"VMs on this Mac are NOT fenced off the host's other ports and your LAN",
		"[skip]  softnet-root",
		"network: isolated",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript lacks %q:\n%s", want, text)
		}
	}
	unavailable := map[string]bool{}
	for _, res := range results {
		if res.Unavailable != "" {
			unavailable[res.Step] = true
		}
	}
	if !unavailable["host-firewall"] || !unavailable["softnet-root"] {
		t.Errorf("the skipped steps should be reported as unavailable: %v", unavailable)
	}
	if countCalls(e.r, "pfctl") > 0 {
		t.Error("rootless must not touch pf")
	}
}

func TestRootless_NoRootBlockIsPrinted(t *testing.T) {
	e, opts := rootlessEnv(t)
	scriptTailscaleUp(e.r)
	scriptPinnedDownloads(e.r, opts)
	var out bytes.Buffer
	steps, err := BuildPlan(e.r, opts, &out)
	if err != nil {
		t.Fatal(err)
	}
	results, err := RunPlan(context.Background(), steps, PlanOptions{Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	// This is applyInstall's selection of what is left for root.
	var rootSteps []Step
	for _, res := range results {
		for _, s := range steps {
			if res.Skipped && s.Name == res.Step && s.Privileged {
				rootSteps = append(rootSteps, s)
			}
		}
	}
	var rootOut bytes.Buffer
	outcome, err := RunRootSteps(context.Background(), e.r, rootSteps, RootOptions{Out: &rootOut, NoSudo: true})
	if err != nil || outcome != RootNone {
		t.Fatalf("outcome = %v, err = %v; want RootNone", outcome, err)
	}
	if rootOut.Len() != 0 || strings.Contains(out.String(), "GROVE_ROOT_STEPS") || strings.Contains(out.String(), "[root]") {
		t.Errorf("a rootless install prints no sudo block:\n%s%s", out.String(), rootOut.String())
	}
	if pending, err := PendingRootSteps(context.Background(), e.r, opts); err != nil || len(pending) != 0 {
		t.Errorf("PendingRootSteps = %v, %v; want none", stepNames(pending), err)
	}
}

func TestRootless_Doctor(t *testing.T) {
	ctx := context.Background()
	e, opts := rootlessEnv(t)
	d := workerDaemons(opts)[0]

	if c := checkWorkerAgent(ctx, e.r, opts); c.OK || !strings.Contains(c.Detail, "not the one this grove renders") {
		t.Fatalf("not installed: %+v", c)
	}
	plist, _ := RenderLaunchAgent(d.Spec)
	e.write(agentPlistPath(opts, d.Label), plist, 0o644)
	if c := checkWorkerAgent(ctx, e.r, opts); !c.OK || !strings.Contains(c.Detail, "LaunchAgent") {
		t.Fatalf("healthy: %+v", c)
	}
	gui := fmt.Sprintf("launchctl print gui/%d/%s", os.Getuid(), d.Label)
	e.r.Script(gui, FakeResult{Err: os.ErrNotExist})
	if c := checkWorkerAgent(ctx, e.r, opts); c.OK || !strings.Contains(c.Detail, "logged in") {
		t.Fatalf("not loaded: %+v", c)
	}
	e.r.Script(gui, FakeResult{Stdout: "program = " + d.Program + "\n\tstate = not running\n"})
	if c := checkWorkerAgent(ctx, e.r, opts); c.OK || !strings.Contains(c.Detail, "not running") {
		t.Fatalf("loaded, not running: %+v", c)
	}

	if c := checkRootlessFirewall(); !c.OK || !c.Warn || !strings.Contains(c.Detail, "not fenced off") {
		t.Errorf("rootless firewall must be a warning, not a failure: %+v", c)
	}

	cfg := &config.Config{Worker: config.WorkerConfig{Rootless: true}}
	e.r.Script(gui, FakeResult{Stdout: "program = " + d.Program + "\n\tpid = 7\n"})
	var names []string
	for _, c := range RunDoctor(ctx, e.r, opts, cfg) {
		names = append(names, c.Name)
		if c.Name == "host-firewall" && (!c.OK || !c.Warn) {
			t.Errorf("host-firewall in a rootless doctor run: %+v", c)
		}
	}
	joined := strings.Join(names, " ")
	if !strings.Contains(joined, "worker-agent") || strings.Contains(joined, "worker-daemon") {
		t.Errorf("doctor should check agents, not daemons: %s", joined)
	}
}

// --firewall keeps the VM firewall in a rootless install: its root step goes to an admin while the
// worker stays LaunchAgents. Once installed, later rootless runs keep it without the flag.
func TestRootless_FirewallKeepsTheRootStepOnly(t *testing.T) {
	e, opts := rootlessEnv(t)
	scriptTailscaleUp(e.r)
	scriptPinnedDownloads(e.r, opts)
	opts.Softnet = true
	opts.Firewall = true
	steps, err := BuildPlan(e.r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"worker-agents", "keep-awake", "host-firewall-stage", "host-firewall", "softnet-root"} {
		if !hasStep(steps, want) {
			t.Errorf("rootless --firewall plan lacks %s: %v", want, stepNames(steps))
		}
	}
	if hasStep(steps, "worker-daemons") || hasStep(steps, "disable-sleep") {
		t.Errorf("--firewall must not switch the worker to daemons: %v", stepNames(steps))
	}
	for _, s := range steps {
		switch {
		case s.Name == "host-firewall" && (!s.Privileged || s.SkipReason != ""):
			t.Errorf("host-firewall must be the root step, not skipped: %+v", s)
		case s.Name != "host-firewall" && s.Privileged:
			t.Errorf("only host-firewall may need root, got %s", s.Name)
		case s.Name == "softnet-root" && s.SkipReason == "":
			t.Errorf("softnet-root stays skipped")
		}
	}

	// Already installed by an admin: kept (and kept current) without the flag, and doctor checks it.
	opts.Firewall = false
	_, installed := hostFirewallPaths(opts)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !rootlessFirewallWanted(opts) {
		t.Error("an installed firewall must stay in the rootless plan")
	}
}
