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

const hostXcodeFleet = `pools:
  - name: macos
    image: ghcr.io/example/macos:latest
    perWorker: 1
    hostXcode:
      path: /Applications/Xcode_27.0.app
      buildVersion: 17A123
`

func TestOrchardControllerArgs(t *testing.T) {
	got := strings.Join(OrchardControllerArgs("100.64.0.1:6120", false), " ")
	if want := "controller run --listen 100.64.0.1:6120 --insecure-no-tls"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	got = strings.Join(OrchardControllerArgs("100.64.0.1:6120", true), " ")
	if want := "controller run --listen 100.64.0.1:6120 --insecure-no-tls --insecure-allow-host-dirs"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// planUnits runs the control-plane plan in a temp home (optionally with a fleet.yaml already in
// place) and returns the rendered controller unit text and the warning output.
func planControllerUnit(t *testing.T, goos, fleetYAML string) (unit, out string) {
	t.Helper()
	home := t.TempDir()
	if fleetYAML != "" {
		dir := filepath.Join(home, ".config", "grove")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(fleetYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := NewFakeRunner()
	scriptWorkerEnrollmentToken(r, home)
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, goos)
	scriptPinnedDownloads(r, opts)
	var buf bytes.Buffer
	steps, err := BuildPlan(r, opts, &buf)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	path := filepath.Join(opts.LaunchAgentsDir(), "com.grove.orchard-controller.plist")
	if goos != "darwin" {
		path = filepath.Join(opts.SystemdUserDir(), "grove-orchard-controller.service")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), buf.String()
}

func TestControlPlanePlan_ControllerPlistCarriesListenAndTLSFlags(t *testing.T) {
	plist, _ := planControllerUnit(t, "darwin", "")
	for _, want := range []string{
		"\t\t<string>controller</string>\n\t\t<string>run</string>\n\t\t<string>--listen</string>\n\t\t<string>100.64.0.1:6120</string>\n\t\t<string>--insecure-no-tls</string>\n\t</array>",
		"<key>ORCHARD_HOME</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "host-dirs") {
		t.Errorf("host dirs enabled without a hostXcode pool:\n%s", plist)
	}
	if strings.Contains(plist, "ORCHARD_BOOTSTRAP_ADMIN_TOKEN") || strings.Contains(plist, "TOKEN") {
		t.Errorf("controller plist must never carry a token:\n%s", plist)
	}
}

func TestControlPlanePlan_ControllerPlistHostDirsFollowsFleet(t *testing.T) {
	plist, _ := planControllerUnit(t, "darwin", hostXcodeFleet)
	if !strings.Contains(plist, "<string>--insecure-allow-host-dirs</string>") {
		t.Errorf("a hostXcode pool needs --insecure-allow-host-dirs:\n%s", plist)
	}
}

func TestControlPlanePlan_ControllerSystemdUnitCarriesFlags(t *testing.T) {
	unit, _ := planControllerUnit(t, "linux", hostXcodeFleet)
	if !strings.Contains(unit, " controller run --listen 100.64.0.1:6120 --insecure-no-tls --insecure-allow-host-dirs\n") {
		t.Errorf("systemd unit lacks the flags:\n%s", unit)
	}
	if strings.Contains(unit, "TOKEN") {
		t.Errorf("unit must never carry a token:\n%s", unit)
	}
}

// The live incident: launchd's loaded job came from an older plist, with the old flags and a
// bootstrap token in its environment.
const driftLaunchctlPrint = `gui/501/com.grove.orchard-controller = {
	active count = 1
	state = running
	program = /Users/argos/.local/bin/orchard
	arguments = {
		/Users/argos/.local/bin/orchard
		controller
		run
		--listen
		100.101.229.60:6120
		--insecure-no-tls
		--insecure-allow-host-dirs
	}
	default environment = {
		PATH => /usr/bin:/bin:/usr/sbin:/sbin
	}
	environment = {
		ORCHARD_HOME => /Users/argos/.config/grove/orchard-controller
		ORCHARD_BOOTSTRAP_ADMIN_TOKEN => s3cret-value
	}
	pid = 81389
}
`

func controllerDoctorFixture(t *testing.T, goos string) (Options, *config.Config, *FakeRunner) {
	t.Helper()
	home := t.TempDir()
	opts := Options{Home: home, GOOS: goos}
	var unitPath string
	if goos == "darwin" {
		unitPath = filepath.Join(opts.LaunchAgentsDir(), orchardControllerLabel+".plist")
	} else {
		unitPath = filepath.Join(opts.SystemdUserDir(), orchardControllerService)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Orchard.URL = "http://100.101.229.60:6120"
	return opts, cfg, NewFakeRunner()
}

func TestCheckOrchardControllerDrift_FlagsOldFlagsAndBootstrapToken(t *testing.T) {
	opts, cfg, r := controllerDoctorFixture(t, "darwin")
	// Rendered unit has no host dirs (no fleet.yaml); the running one has the flag, and the
	// program path differs (argos's home vs this temp home).
	r.Script(fmt.Sprintf("launchctl print gui/%d/com.grove.orchard-controller", os.Getuid()), FakeResult{Stdout: driftLaunchctlPrint})
	got := checkOrchardControllerDrift(context.Background(), r, opts, cfg)
	if len(got) != 2 {
		t.Fatalf("want unit + credential results, got %+v", got)
	}
	unit, cred := got[0], got[1]
	if unit.OK || !strings.Contains(unit.Detail, "extra --insecure-allow-host-dirs") || !strings.Contains(unit.Detail, "program /Users/argos/.local/bin/orchard") {
		t.Errorf("unit check: %+v", unit)
	}
	if cred.OK || !strings.Contains(cred.Detail, "ORCHARD_BOOTSTRAP_ADMIN_TOKEN") || cred.Remediation == "" {
		t.Errorf("credential check: %+v", cred)
	}
	if strings.Contains(fmt.Sprintf("%+v", got), "s3cret-value") {
		t.Errorf("a token value leaked into a check result: %+v", got)
	}
}

func TestCheckOrchardControllerDrift_MatchingUnitIsClean(t *testing.T) {
	opts, cfg, r := controllerDoctorFixture(t, "darwin")
	prog := filepath.Join(orchardBinDir(opts), orchardBinName)
	r.Script(fmt.Sprintf("launchctl print gui/%d/com.grove.orchard-controller", os.Getuid()), FakeResult{Stdout: "\targuments = {\n\t\t" + prog +
		"\n\t\tcontroller\n\t\trun\n\t\t--listen\n\t\t100.101.229.60:6120\n\t\t--insecure-no-tls\n\t}\n\tenvironment = {\n\t\tORCHARD_HOME => /x\n\t}\n\tpid = 7\n"})
	r.Script("ps eww -p 7 -o command=", FakeResult{Stdout: prog + " controller run ORCHARD_HOME=/x PATH=/usr/bin"})
	got := checkOrchardControllerDrift(context.Background(), r, opts, cfg)
	if len(got) != 2 || !got[0].OK || !got[1].OK {
		t.Fatalf("want two passing checks, got %+v", got)
	}
}

func TestCheckOrchardControllerDrift_TokenOnlyInProcessEnvironment(t *testing.T) {
	opts, cfg, r := controllerDoctorFixture(t, "darwin")
	prog := filepath.Join(orchardBinDir(opts), orchardBinName)
	r.Script(fmt.Sprintf("launchctl print gui/%d/com.grove.orchard-controller", os.Getuid()), FakeResult{Stdout: "\targuments = {\n\t\t" + prog +
		"\n\t\tcontroller\n\t\trun\n\t\t--listen\n\t\t100.101.229.60:6120\n\t\t--insecure-no-tls\n\t}\n\tpid = 7\n"})
	r.Script("ps eww -p 7 -o command=", FakeResult{Stdout: prog + " controller run ORCHARD_BOOTSTRAP_ADMIN_TOKEN=abc ORCHARD_HOME=/x"})
	got := checkOrchardControllerDrift(context.Background(), r, opts, cfg)
	if len(got) != 2 || !got[0].OK || got[1].OK {
		t.Fatalf("want a clean unit and a failing credential check, got %+v", got)
	}
}

func TestCheckOrchardControllerDrift_SkipsWhenNothingToCompare(t *testing.T) {
	opts, cfg, r := controllerDoctorFixture(t, "darwin")
	// Not loaded (launchctl print fails): reachability is another check's job.
	r.Script(fmt.Sprintf("launchctl print gui/%d/com.grove.orchard-controller", os.Getuid()), FakeResult{Err: os.ErrNotExist})
	if got := checkOrchardControllerDrift(context.Background(), r, opts, cfg); len(got) != 0 {
		t.Errorf("not loaded: %+v", got)
	}
	// No controller unit on this host (a worker or client).
	if got := checkOrchardControllerDrift(context.Background(), NewFakeRunner(), Options{Home: t.TempDir(), GOOS: "darwin"}, cfg); len(got) != 0 {
		t.Errorf("no unit: %+v", got)
	}
}

func TestCheckOrchardControllerDrift_Systemd(t *testing.T) {
	opts, cfg, r := controllerDoctorFixture(t, "linux")
	prog := filepath.Join(orchardBinDir(opts), orchardBinName)
	r.Script("systemctl --user show "+orchardControllerService+" -p MainPID -p ExecStart -p Environment", FakeResult{Stdout: "MainPID=42\n" +
		"ExecStart={ path=" + prog + " ; argv[]=" + prog + " controller run ; ignore_errors=no ; start_time=[n/a] }\n" +
		"Environment=ORCHARD_HOME=/x\n"})
	r.Script("cat /proc/42/environ", FakeResult{Stdout: "ORCHARD_HOME=/x\x00ORCHARD_BOOTSTRAP_ADMIN_TOKEN=abc\x00"})
	got := checkOrchardControllerDrift(context.Background(), r, opts, cfg)
	if len(got) != 2 || got[0].OK || !strings.Contains(got[0].Detail, "missing") || got[1].OK {
		t.Fatalf("want drift and a token finding, got %+v", got)
	}
}

func TestOrchardControllerDriftStep_ReportsButNeverRestarts(t *testing.T) {
	opts, _, r := controllerDoctorFixture(t, "darwin")
	cfgDir := opts.ConfigDir()
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("orchard:\n  url: http://100.101.229.60:6120\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROVE_CONFIG", "")
	r.Script(fmt.Sprintf("launchctl print gui/%d/com.grove.orchard-controller", os.Getuid()), FakeResult{Stdout: driftLaunchctlPrint})
	var out bytes.Buffer
	ok, err := orchardControllerDriftStep(r, opts, cfgDir, &out).Check(context.Background())
	if err != nil || !ok {
		t.Fatalf("the drift step must always pass: ok=%v err=%v", ok, err)
	}
	for _, want := range []string{"orchard-controller-unit", "orchard-controller-credential", "ORCHARD_BOOTSTRAP_ADMIN_TOKEN"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("warning output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "s3cret-value") {
		t.Errorf("a token value was printed:\n%s", out.String())
	}
	for _, c := range r.Calls {
		if c.Name == "launchctl" && len(c.Args) > 0 && c.Args[0] != "print" {
			t.Errorf("the drift step ran a mutating launchctl command: %+v", c)
		}
	}
}
