package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/server"
	"github.com/gm2211/grove/internal/wire"
)

// defaultFleetReconcileInterval is `grove serve`'s default tick for its background fleet
// reconciler loop (see runFleetReconciler) — the same cadence `grove fleet watch` defaults to.
const defaultFleetReconcileInterval = 30 * time.Second

func init() {
	var listen string
	var fleetReconcileInterval time.Duration
	var noFleetReconcile bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the grove control-plane HTTP API + embedded UI.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd.Context(), listen, fleetReconcileInterval, noFleetReconcile)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "listen address, e.g. 0.0.0.0:6120 (default: server.listen from config, else 127.0.0.1:6120)")
	cmd.Flags().DurationVar(&fleetReconcileInterval, "fleet-reconcile-interval", defaultFleetReconcileInterval,
		"how often to reconcile Orchard against fleet.yaml (recreates TTL-recycled VMs; see ARCHITECTURE.md -> Recycling/hygiene)")
	cmd.Flags().BoolVar(&noFleetReconcile, "no-fleet-reconcile", false,
		"disable the fleet reconciler loop (TTL-recycled VMs will NOT be recreated automatically)")
	Root.AddCommand(cmd)
}

func runServe(ctx context.Context, listen string, fleetReconcileInterval time.Duration, noFleetReconcile bool) error {
	cfg, cfgPath, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	addr := listen
	if addr == "" {
		addr = cfg.Server.Listen
	}
	if addr == "" {
		addr = "127.0.0.1:6120"
	}

	oc, err := wire.NewOrchardClient(cfg.Orchard)
	if err != nil {
		return fmt.Errorf("orchard client: %w", err)
	}
	nc, err := wire.NewNomadClient(cfg.Nomad)
	if err != nil {
		return fmt.Errorf("nomad client: %w", err)
	}

	ac, err := wire.NewArtifactsClient(cfg.Artifacts)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotConfigured) {
			slog.Info("serve: no artifact store configured; job artifact download/listing is disabled", "err", err)
			ac = nil
		} else {
			return fmt.Errorf("artifacts client: %w", err)
		}
	}

	pools, perr := poolConfigs(cfg)
	if perr != nil {
		slog.Warn("serve: could not determine pools for EnsureJobs; skipping job registration", "config", cfgPath, "err", perr)
	} else if len(pools) == 0 {
		slog.Warn("serve: no pools found in fleet spec; no parameterized jobs registered", "fleet", cfg.Fleet)
	}

	ds, err := dispatch.New(nc, dispatch.Options{ArtifactsBase: cfg.Artifacts.Bucket, Artifacts: ac, Pools: pools})
	if err != nil {
		return fmt.Errorf("dispatch service: %w", err)
	}

	if len(pools) > 0 {
		if err := dispatch.EnsureJobs(ctx, nc, pools); err != nil {
			slog.Warn("serve: EnsureJobs failed; continuing to serve anyway", "err", err)
		}
	}

	// Periodically reconcile every non-terminal job's status against Nomad, so an orphaned job
	// (its dispatched Nomad job 404s — e.g. a dev Nomad agent restarted with its data dir wiped) or
	// one stuck pending past dispatch.Options.PendingTimeout gets marked lost even if nothing polls
	// it via Get/List in the meantime. Runs for the life of the process; a failed tick is logged by
	// RunReconciler itself and doesn't stop the sweep.
	go func() {
		if err := ds.RunReconciler(ctx, dispatch.DefaultReconcileInterval); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("serve: job reconciler sweep stopped", "err", err)
		}
	}()

	srv := server.New(oc, nc, ds, ac, server.Options{Token: cfg.Server.Token, Version: Version})

	// Recreate TTL-recycled VMs: ARCHITECTURE.md's "Recycling / hygiene" step 3 says grove's fleet
	// reconciler notices a deleted VM and recreates it, but until now `grove serve` never actually
	// ran one — only `grove fleet watch` did, which nobody runs alongside `grove serve` in
	// practice. Wire it in here too, so a TTL-expired VM (deleted by Orchard, see step 2) comes
	// back on its own instead of the fleet silently shrinking.
	if cfg.Fleet == "" {
		slog.Info("serve: fleet is not configured; fleet reconciler disabled (TTL-recycled VMs will not be recreated)")
	} else if noFleetReconcile {
		slog.Info("serve: fleet reconciler disabled by --no-fleet-reconcile")
	} else {
		interval := fleetReconcileInterval
		if interval <= 0 {
			interval = defaultFleetReconcileInterval
		}
		loop := newFleetReconcileLoop(cfg, oc, srv.FleetReconcile())
		go loop.run(ctx, interval)
	}

	slog.Info("grove serve: listening", "addr", addr)
	return http.ListenAndServe(addr, srv)
}

// fleetReconcileLoop periodically reconciles Orchard to match fleet.yaml, the same work
// `grove fleet watch` does, run inline by `grove serve` (see runServe) so a TTL-recycled VM (see
// ARCHITECTURE.md -> "Recycling / hygiene") gets recreated without a separate `grove fleet watch`
// process. Unlike `grove fleet watch` (which loads fleet.yaml once at startup), it reloads and
// re-validates fleet.yaml on every tick — cheap, and it means an operator editing pools in
// fleet.yaml takes effect on the next tick without restarting `grove serve`.
type fleetReconcileLoop struct {
	cfg    *config.Config
	client orchard.Client
	status *server.FleetReconcileStatus

	// warnedInvalid tracks whether the current streak of fleet.yaml load failures has already been
	// logged, so a fleet.yaml that's missing/invalid for an extended period logs once (not once per
	// tick) until it's fixed or starts failing for a new reason.
	warnedInvalid bool
}

func newFleetReconcileLoop(cfg *config.Config, client orchard.Client, status *server.FleetReconcileStatus) *fleetReconcileLoop {
	return &fleetReconcileLoop{cfg: cfg, client: client, status: status}
}

// run ticks immediately and then every interval until ctx is done.
func (l *fleetReconcileLoop) run(ctx context.Context, interval time.Duration) {
	l.status.SetEnabled(true, interval)

	l.tick(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

// tick reloads fleet.yaml, plans, and — if the plan does anything — applies it, publishing the
// outcome to l.status either way. A missing/invalid fleet.yaml or a Plan/Apply failure is logged
// and skipped; it never stops the loop or crashes `grove serve`'s API/UI, since the next tick
// retries from scratch.
func (l *fleetReconcileLoop) tick(ctx context.Context) {
	spec, err := fleet.Load(l.cfg.Fleet)
	if err != nil {
		if !l.warnedInvalid {
			slog.Warn("serve: fleet reconciler: fleet spec missing or invalid; skipping until it's fixed", "fleet", l.cfg.Fleet, "err", err)
			l.warnedInvalid = true
		}
		l.status.Report(fleet.Plan{}, err, time.Now())
		return
	}
	l.warnedInvalid = false

	r := &fleet.Reconciler{
		Client: l.client,
		Spec:   spec,
		Options: fleet.Options{
			TailscaleAuthKey: l.cfg.Tailscale.AuthKey,
		},
	}

	plan, err := r.Plan(ctx)
	if err != nil {
		slog.Warn("serve: fleet reconciler: plan failed", "err", err)
		l.status.Report(fleet.Plan{}, err, time.Now())
		return
	}

	if !plan.Empty() {
		slog.Info("fleet: " + planSummary(plan))
		if err := r.Apply(ctx, plan); err != nil {
			slog.Warn("serve: fleet reconciler: apply failed", "err", err)
			l.status.Report(plan, err, time.Now())
			return
		}
	}

	l.status.Report(plan, nil, time.Now())
}

// planSummary renders a Plan as e.g. "+create linux-mac1-0 / -delete linux-mac2-0" for the
// reconciler's per-tick Info log — only called when the plan actually does something.
func planSummary(plan fleet.Plan) string {
	parts := make([]string, 0, len(plan.Creates)+len(plan.Deletes))
	for _, c := range plan.Creates {
		parts = append(parts, "+create "+c.Spec.Name)
	}
	for _, d := range plan.Deletes {
		parts = append(parts, "-delete "+d.Name)
	}
	return strings.Join(parts, " / ")
}

// poolConfigs reads cfg.Fleet (fleet.yaml) and returns the configured pools' EnsureJobs config, so
// `grove serve` can register grove-<kind>-<pool> jobs for each of them at startup. Returns (nil,
// nil) if no fleet spec is configured.
func poolConfigs(cfg *config.Config) ([]dispatch.PoolConfig, error) {
	if cfg.Fleet == "" {
		return nil, nil
	}
	data, err := os.ReadFile(cfg.Fleet)
	if err != nil {
		return nil, fmt.Errorf("read fleet spec %s: %w", cfg.Fleet, err)
	}
	var spec fleet.Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse fleet spec %s: %w", cfg.Fleet, err)
	}
	pools := make([]dispatch.PoolConfig, 0, len(spec.Pools))
	for _, p := range spec.Pools {
		pools = append(pools, dispatch.PoolConfig{
			Name:              p.Name,
			CPU:               int(p.JobCPUOrDefault()),
			Memory:            int(p.JobMemoryOrDefault()),
			AllowDockerSocket: p.AllowDockerSocket,
		})
	}
	return pools, nil
}
