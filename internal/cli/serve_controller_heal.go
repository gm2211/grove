package cli

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/install"
	"github.com/gm2211/grove/internal/orchard"
)

const (
	// controllerHealInterval is how often `grove serve` looks whether the Orchard controller
	// drifted from the unit grove renders; controllerHealFirst is the first look after start.
	controllerHealInterval = 30 * time.Minute
	controllerHealFirst    = 5 * time.Minute
	// controllerRestartBackoff spaces reloads: one that didn't clear the drift is not repeated
	// every half hour.
	controllerRestartBackoff = 24 * time.Hour
)

// runControllerHeal keeps this control plane's Orchard controller on the rendered LaunchAgent
// (install.HealOrchardController), only while idle says no job is pending or running.
func runControllerHeal(ctx context.Context, cfgDir string, oc orchard.Client, idle func(context.Context) bool) {
	var lastRestart time.Time
	reachable := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := oc.ListWorkers(ctx)
		return err
	}
	tick := func() {
		if !lastRestart.IsZero() && time.Since(lastRestart) < controllerRestartBackoff {
			return
		}
		if !idle(ctx) {
			return
		}
		heal, err := install.HealOrchardController(ctx, install.ExecRunner{}, install.Options{}, cfgDir, reachable)
		if heal.Restarted {
			lastRestart = time.Now()
		}
		why := strings.Join(heal.Why, "; ")
		switch {
		case err != nil:
			slog.Warn("serve: could not move the Orchard controller onto the rendered unit", "why", why, "rolledBack", heal.RolledBack, "err", err)
		case heal.Restarted:
			slog.Info("serve: reloaded the Orchard controller onto the rendered unit", "why", why)
		case heal.Rewrote:
			slog.Info("serve: rewrote the Orchard controller's plist to the rendered one", "why", why)
		}
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(controllerHealFirst):
	}
	tick()
	t := time.NewTicker(controllerHealInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
