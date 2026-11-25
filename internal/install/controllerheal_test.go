package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/config"
)

func controllerHealFixture(t *testing.T) (Options, string, string, *FakeRunner) {
	t.Helper()
	controllerHealPoll, controllerHealDeadline, controllerHealSettle = time.Millisecond, 5*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		controllerHealPoll, controllerHealDeadline, controllerHealSettle = 5*time.Second, 2*time.Minute, time.Second
	})
	opts, cfg, r := controllerDoctorFixture(t, "darwin")
	cfgDir := filepath.Join(opts.Home, ".config", "grove")
	if err := config.Save(filepath.Join(cfgDir, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	// The plist an older grove rendered: a bare `controller run`.
	plist := filepath.Join(opts.LaunchAgentsDir(), orchardControllerLabel+".plist")
	old, err := renderControllerPlist(opts, cfgDir, "/Users/argos/.local/bin/orchard", []string{"controller", "run"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Script(fmt.Sprintf("launchctl print gui/%d/com.grove.orchard-controller", os.Getuid()), FakeResult{Stdout: driftLaunchctlPrint})
	return opts, cfgDir, plist, r
}

// The Studio's case: a stale plist on disk, and a running controller with an extra flag and a
// bootstrap admin token. The plist is rewritten to the rendered one and the job reloaded onto it.
func TestHealOrchardController_ReloadsADriftedController(t *testing.T) {
	opts, cfgDir, plist, r := controllerHealFixture(t)
	heal, err := HealOrchardController(context.Background(), r, opts, cfgDir, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if !heal.Rewrote || !heal.Restarted || heal.RolledBack {
		t.Fatalf("heal = %+v", heal)
	}
	got, _ := os.ReadFile(plist)
	program, args, _ := expectedControllerUnit(opts, &config.Config{Orchard: config.Endpoint{URL: "http://100.101.229.60:6120"}})
	want, _ := renderControllerPlist(opts, cfgDir, program, args)
	if string(got) != want {
		t.Fatalf("plist =\n%s\nwant the rendered one:\n%s", got, want)
	}
	if !strings.Contains(string(got), "<string>--listen</string>") || strings.Contains(string(got), "BOOTSTRAP") {
		t.Fatalf("rendered plist lacks --listen or carries the bootstrap token:\n%s", got)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if !r.CalledWith("launchctl bootout "+domain+"/"+orchardControllerLabel) || !r.CalledWith("launchctl bootstrap "+domain+" "+plist) {
		t.Fatalf("controller not reloaded: %+v", r.Calls)
	}
	if strings.Contains(fmt.Sprintf("%+v", heal), "s3cret-value") {
		t.Fatalf("a token value reached the result: %+v", heal)
	}
}

// A reloaded controller that never answers is put back on what was running, without the token.
func TestHealOrchardController_RollsBackWhenTheControllerNeverAnswers(t *testing.T) {
	opts, cfgDir, plist, r := controllerHealFixture(t)
	heal, err := HealOrchardController(context.Background(), r, opts, cfgDir, func(context.Context) error { return errors.New("connection refused") })
	if err == nil || !heal.RolledBack {
		t.Fatalf("heal = %+v, err = %v; want a rollback", heal, err)
	}
	got, _ := os.ReadFile(plist)
	for _, want := range []string{"/Users/argos/.local/bin/orchard", "<string>--insecure-allow-host-dirs</string>", "<string>100.101.229.60:6120</string>"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("rolled-back plist lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "BOOTSTRAP") || strings.Contains(string(got), "s3cret") {
		t.Fatalf("the rollback brought the bootstrap token back:\n%s", got)
	}
	n := 0
	for _, c := range r.Calls {
		if c.Name == "launchctl" && len(c.Args) > 0 && c.Args[0] == "bootstrap" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("bootstrap ran %d times, want 2 (reload, rollback)", n)
	}
}

// A controller that runs the rendered unit, from the rendered plist, is left alone.
func TestHealOrchardController_LeavesAMatchingControllerAlone(t *testing.T) {
	opts, cfgDir, plist, r := controllerHealFixture(t)
	program, args, _ := expectedControllerUnit(opts, &config.Config{Orchard: config.Endpoint{URL: "http://100.101.229.60:6120"}})
	want, _ := renderControllerPlist(opts, cfgDir, program, args)
	if err := os.WriteFile(plist, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Script(fmt.Sprintf("launchctl print gui/%d/com.grove.orchard-controller", os.Getuid()), FakeResult{Stdout: "\targuments = {\n\t\t" + program +
		"\n\t\t" + strings.Join(args, "\n\t\t") + "\n\t}\n\tpid = 7\n"})
	heal, err := HealOrchardController(context.Background(), r, opts, cfgDir, func(context.Context) error { return nil })
	if err != nil || heal.Rewrote || heal.Restarted {
		t.Fatalf("heal = %+v, err = %v; want nothing done", heal, err)
	}
	for _, c := range r.Calls {
		if c.Name == "launchctl" && c.Args[0] != "print" {
			t.Fatalf("ran %s on a matching controller", c.String())
		}
	}
}
