package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/gitauth"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/sandbox"
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
	if legacy := cfg.LegacyKeys(); len(legacy) > 0 {
		slog.Warn("serve: ignoring settings for Nomad and in-VM Tailscale, which grove no longer uses (re-run `grove install --role control-plane` to remove them)", "keys", legacy)
	}
	if legacy := cfg.Artifacts.LegacyKeys(); len(legacy) > 0 {
		slog.Warn("serve: ignoring legacy S3/MinIO artifact settings; artifacts are kept on this machine's disk now (re-run `grove install --role control-plane` to remove them)", "keys", legacy)
	}
	ac, err := wire.NewArtifactsClient(cfg.Artifacts)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotConfigured) {
			slog.Info("serve: no artifact store directory; job artifact download/listing is disabled", "err", err)
			ac = nil
		} else {
			return fmt.Errorf("artifact store: %w", err)
		}
	}

	pools, perr := poolConfigs(cfg)
	if perr != nil {
		slog.Warn("serve: could not read the pools from the fleet spec; every job will be refused", "config", cfgPath, "err", perr)
	} else if len(pools) == 0 {
		slog.Warn("serve: no pools found in the fleet spec; every job will be refused", "fleet", cfg.Fleet)
	}

	// The private-repo credential store is shared by value-of-pointer between dispatch (which
	// reads it on every submit) and the HTTP API (which arms and disarms it), so `grove github
	// enable` takes effect on the very next job with no restart. It starts disarmed: a fresh
	// control plane clones public repositories only.
	gitAuthStore, err := gitauth.NewStore(filepath.Join(filepath.Dir(cfgPath), "github.json"))
	if err != nil {
		return fmt.Errorf("private-repo credential store: %w", err)
	}

	// Jobs run inside this process: dispatch queues them per pool (persisted next to the job
	// history, so a queued job survives a restart) and drives each one in a sealed VM of its pool
	// through Orchard (internal/sandbox). A job that was running when this process last stopped is
	// marked lost on startup and its VM recycled — see dispatch.New.
	var uploader artifacts.Uploader
	if ac != nil {
		uploader, _ = ac.(artifacts.Uploader)
	}
	executor := &dispatch.SandboxExecutor{Orchard: oc, Uploader: uploader, SlotDir: sandboxSlotDir()}
	ds, err := dispatch.New(executor, dispatch.Options{Artifacts: ac, ArtifactRetention: cfg.Artifacts.Retention, Pools: pools, RepoAuth: gitAuthStore})
	if err != nil {
		return fmt.Errorf("dispatch service: %w", err)
	}

	// Housekeeping for the life of the process: expires finished jobs' artifacts and log files.
	// A failed tick is logged by RunReconciler itself and doesn't stop the sweep.
	go func() {
		if err := ds.RunReconciler(ctx, dispatch.DefaultReconcileInterval); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("serve: job reconciler sweep stopped", "err", err)
		}
	}()

	serverOpts := server.Options{
		Token: cfg.Server.Token, Version: Version,
		ControllerURL: cfg.Orchard.URL, ServerURL: cfg.Server.URL,
		GitAuth: gitAuthStore,
	}
	accessStore, err := server.NewAccessStore(filepath.Join(filepath.Dir(cfgPath), "devices.json"))
	if err != nil {
		return fmt.Errorf("device credential store: %w", err)
	}
	serverOpts.AccessStore = accessStore
	if cfg.Enrollment.IssuerToken != "" {
		issuer, issuerErr := orchard.New(cfg.Orchard.URL, cfg.Enrollment.IssuerToken)
		if issuerErr != nil {
			return fmt.Errorf("enrollment issuer: %w", issuerErr)
		}
		if typed, ok := issuer.(interface {
			IssueWorkerBootstrap(context.Context, string) (string, error)
		}); ok {
			serverOpts.IssueWorkerBootstrap = typed.IssueWorkerBootstrap
		} else {
			return errors.New("enrollment issuer does not support worker credential creation")
		}
	}
	srv := server.New(oc, ds, ac, serverOpts)

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
	// warnedBlocked is the blocked-pool message last logged, for the same once-per-streak logging.
	warnedBlocked string
	// warnedSpec is the fleet.yaml warnings last logged, logged again only when they change.
	warnedSpec string
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
	if warnings := strings.Join(spec.Warnings, "; "); warnings != l.warnedSpec {
		if warnings != "" {
			slog.Warn("serve: fleet reconciler: "+warnings, "fleet", l.cfg.Fleet)
		}
		l.warnedSpec = warnings
	}

	r := &fleet.Reconciler{Client: l.client, Spec: spec}

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
	// A blocked pool is a configuration problem, not a failed tick: the rest of the plan still
	// applied, but the operator has to see why that pool has no VMs.
	blockedErr := plan.BlockedError()
	blocked := ""
	if blockedErr != nil {
		blocked = blockedErr.Error()
		if blocked != l.warnedBlocked {
			slog.Warn("serve: fleet reconciler: pool blocked", "err", blockedErr)
		}
	}
	l.warnedBlocked = blocked
	l.status.Report(plan, blockedErr, time.Now())
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

// poolConfigs reads cfg.Fleet (fleet.yaml) and returns the configured pools as the dispatch
// service runs their jobs. Returns (nil, nil) if no fleet spec is configured.
func poolConfigs(cfg *config.Config) ([]dispatch.PoolConfig, error) {
	if cfg.Fleet == "" {
		return nil, nil
	}
	spec, err := fleet.Load(cfg.Fleet)
	if err != nil {
		return nil, err
	}
	pools := make([]dispatch.PoolConfig, 0, len(spec.Pools))
	for _, p := range spec.Pools {
		runnerImage, err := dispatch.RunnerImageOrDefault(p.RunnerImage)
		if err != nil {
			return nil, fmt.Errorf("pool %q runnerImage: %w", p.Name, err)
		}
		pools = append(pools, dispatch.PoolConfig{
			Name:        p.Name,
			RunnerImage: runnerImage,
			CPU:         int(p.JobCPUOrDefault()),
			Memory:      int(p.JobMemoryOrDefault()),
			Slots:       sealedVMSlots(p),
			OS:          poolOS(p),
		})
	}
	return pools, nil
}

// poolOS is the guest OS of p's VMs, as sandbox.Request.OS takes it: sandbox.OSDarwin for the
// macos pool, sandbox.OSLinux for every other.
func poolOS(p fleet.Pool) string {
	if p.MacOS() {
		return sandbox.OSDarwin
	}
	return sandbox.OSLinux
}

// sealedVMGuestReserveMiB is the memory a sealed VM keeps for its own OS and docker daemon when
// sealedVMSlots works out how many jobs fit.
const sealedVMGuestReserveMiB = 512

// sealedVMSlots is how many jobs one of a pool's (sealed) VMs runs at once: as many as fit its CPU
// (at 2000 MHz per core) and memory at the pool's per-job sizing, and at least one. A macOS VM
// runs exactly one job, and is then deleted and recreated (see internal/sandbox).
func sealedVMSlots(p fleet.Pool) int {
	if p.MacOS() {
		return 1
	}
	byCPU := int(p.CPU*2000) / int(p.JobCPUOrDefault())
	byMemory := 0
	if p.Memory > sealedVMGuestReserveMiB {
		byMemory = int(p.Memory-sealedVMGuestReserveMiB) / int(p.JobMemoryOrDefault())
	}
	return max(1, min(byCPU, byMemory))
}

// sandboxSlotDir holds the per-VM slot lock files the dispatch service's sandbox runners share.
func sandboxSlotDir() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "grove", "sandbox-slots")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "grove", "sandbox-slots")
	}
	return filepath.Join(os.TempDir(), "grove-sandbox-slots")
}
