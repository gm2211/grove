package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Waits of HealOrchardController, variables so tests don't sleep.
var (
	controllerHealPoll     = 5 * time.Second
	controllerHealDeadline = 2 * time.Minute
	controllerHealSettle   = time.Second
)

// ControllerHeal says what HealOrchardController did.
type ControllerHeal struct {
	// Rewrote: the controller's plist on disk was not the one grove renders, and now is.
	Rewrote bool
	// Restarted: the running controller differed from the rendered unit (flags, program, or a
	// bootstrap admin token in its environment) and was reloaded onto it.
	Restarted bool
	// RolledBack: the reloaded controller never answered, so the plist was put back to what was
	// running before (without any bootstrap token) and reloaded again.
	RolledBack bool
	// Why lists the drift that was found, in doctor's words.
	Why []string
}

// HealOrchardController keeps a macOS control plane's Orchard controller on the LaunchAgent grove
// renders, so a drift `grove doctor` reports doesn't wait for an operator:
//
//   - a plist on disk that differs from the rendered one is rewritten (an older grove rendered a
//     bare `controller run`, which a reboot would start listening on every interface with TLS,
//     cutting off every worker);
//   - a running controller that differs from the rendered unit, or holds
//     ORCHARD_BOOTSTRAP_ADMIN_TOKEN in its environment, is reloaded onto it (bootout, then
//     bootstrap: launchd keeps a job's old arguments and environment until then).
//
// The caller decides when: only while no job is pending or running (the reload pauses scheduling
// for seconds; running VMs keep running and workers reconnect). After a reload it waits for
// reachable to succeed; if it never does, it puts back a plist with the arguments that were
// running (never the token) and reloads that, so a bad render can't take the fleet down. The token
// value is never read: only its name is compared.
func HealOrchardController(ctx context.Context, r Runner, opts Options, cfgDir string, reachable func(context.Context) error) (ControllerHeal, error) {
	var heal ControllerHeal
	if opts.goos() != "darwin" || !controllerUnitInstalled(opts) {
		return heal, nil
	}
	cfg, err := loadOrInitConfig(cfgDir)
	if err != nil {
		return heal, err
	}
	program, args, ok := expectedControllerUnit(opts, cfg)
	if !ok {
		return heal, nil // nothing to render from: the doctor says so
	}
	plistPath := filepath.Join(opts.LaunchAgentsDir(), orchardControllerLabel+".plist")
	want, err := renderControllerPlist(opts, cfgDir, program, args)
	if err != nil {
		return heal, err
	}
	onDisk, err := os.ReadFile(plistPath)
	if err != nil {
		return heal, err
	}

	restart := false
	for _, c := range checkOrchardControllerDrift(ctx, r, opts, cfg) {
		if !c.OK && !c.Warn {
			restart = true
			heal.Why = append(heal.Why, c.Name+": "+c.Detail)
		}
	}
	if string(onDisk) != want {
		if err := writeFile(plistPath, want); err != nil {
			return heal, fmt.Errorf("rewrite %s: %w", plistPath, err)
		}
		heal.Rewrote = true
		heal.Why = append(heal.Why, "the controller's plist was not the one grove renders")
	}
	if !restart {
		return heal, nil
	}

	// What to go back to: the arguments that were running, with the rendered environment (so
	// never a bootstrap token). Without them, the old plist on disk.
	rollback := string(onDisk)
	if run, ok := inspectRunningController(ctx, r, opts); ok && len(run.Argv) > 0 {
		if s, err := renderControllerPlist(opts, cfgDir, run.Argv[0], run.Argv[1:]); err == nil {
			rollback = s
		}
	}
	if err := reloadController(ctx, r, plistPath); err != nil {
		return heal, err
	}
	heal.Restarted = true
	if err := waitReachable(ctx, reachable); err == nil {
		return heal, nil
	} else {
		if werr := writeFile(plistPath, rollback); werr != nil {
			return heal, fmt.Errorf("the reloaded controller never answered (%v), and putting the old plist back failed: %w", err, werr)
		}
		heal.RolledBack = true
		if rerr := reloadController(ctx, r, plistPath); rerr != nil {
			return heal, fmt.Errorf("the reloaded controller never answered (%v), and reloading the old one failed: %w", err, rerr)
		}
		return heal, fmt.Errorf("the reloaded controller never answered (%v); put back what was running", err)
	}
}

// renderControllerPlist renders the controller's LaunchAgent as the control-plane install does.
func renderControllerPlist(opts Options, cfgDir, program string, args []string) (string, error) {
	logDir := opts.LogDir()
	return RenderLaunchAgent(LaunchAgentSpec{
		Label: orchardControllerLabel, Program: program, Args: args,
		Env:       map[string]string{"ORCHARD_HOME": controlPlaneOrchardHome(cfgDir)},
		KeepAlive: true, RunAtLoad: true,
		StdoutPath: filepath.Join(logDir, "orchard-controller.log"),
		StderrPath: filepath.Join(logDir, "orchard-controller.err.log"),
	})
}

// reloadController boots the controller's job out of this user's GUI domain and back in from
// plistPath. Bootstrap right after bootout can fail while launchd still tears the old job down,
// so it is retried briefly.
func reloadController(ctx context.Context, r Runner, plistPath string) error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_, _, _ = r.Run(ctx, "launchctl", "bootout", domain+"/"+orchardControllerLabel)
	var err error
	for i := 0; i < 5; i++ {
		if _, _, err = r.Run(ctx, "launchctl", "bootstrap", domain, plistPath); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(controllerHealSettle):
		}
	}
	return fmt.Errorf("launchctl bootstrap %s %s: %w", domain, plistPath, err)
}

func waitReachable(ctx context.Context, reachable func(context.Context) error) error {
	deadline := time.Now().Add(controllerHealDeadline)
	for {
		err := reachable(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(controllerHealPoll):
		}
	}
}
