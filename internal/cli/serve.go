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
	"gopkg.in/yaml.v3"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/gitauth"
	"github.com/gm2211/grove/internal/health"
	"github.com/gm2211/grove/internal/install"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/sandbox"
	"github.com/gm2211/grove/internal/secrets"
	"github.com/gm2211/grove/internal/server"
	"github.com/gm2211/grove/internal/tsauth"
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
	if err := tsauth.CheckListen(ctx, addr, nil); err != nil {
		return fmt.Errorf("%w (set server.listen in config.yaml, e.g. `grove config set server.listen <tailnet-ip>:6120`)", err)
	}

	oc, err := wire.NewOrchardClient(cfg.Orchard)
	if err != nil {
		return fmt.Errorf("orchard client: %w", err)
	}
	nc, err := wire.NewNomadClient(cfg.Nomad)
	if err != nil {
		return fmt.Errorf("nomad client: %w", err)
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
		slog.Warn("serve: could not determine pools for EnsureJobs; skipping job registration", "config", cfgPath, "err", perr)
	} else if len(pools) == 0 {
		slog.Warn("serve: no pools found in fleet spec; no parameterized jobs registered", "fleet", cfg.Fleet)
	}

	// The private-repo credential store is shared by value-of-pointer between dispatch (which
	// reads it on every submit) and the HTTP API (which arms and disarms it), so `grove github
	// enable` takes effect on the very next job with no restart. It starts disarmed: a fresh
	// control plane clones public repositories only.
	gitAuthStore, err := gitauth.NewStore(filepath.Join(filepath.Dir(cfgPath), "github.json"))
	if err != nil {
		return fmt.Errorf("private-repo credential store: %w", err)
	}

	// Named job secrets: like the private-repo credential, one store shared by dispatch (which
	// resolves names on submit) and the HTTP API (where the operator sets them and where each
	// caller's right to name one is checked).
	secretStore, err := secrets.NewStore(filepath.Join(filepath.Dir(cfgPath), "secrets.json"))
	if err != nil {
		return fmt.Errorf("named secret store: %w", err)
	}

	poolHealth := &fleet.PoolHealth{}
	ds, err := dispatch.New(nc, dispatch.Options{PoolHealth: poolHealth, Artifacts: ac, ArtifactRetention: cfg.Artifacts.Retention, Pools: pools, RepoAuth: gitAuthStore, Secrets: secretStore})
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

	// Rebuild a sealed VM that keeps losing jobs, and stop sending work to a Mac whose VMs keep
	// losing them, from the attempt records the job drivers keep (see internal/health).
	monitor := &health.Monitor{Jobs: ds, Orchard: oc}
	go func() {
		if err := monitor.Run(ctx, dispatch.DefaultReconcileInterval); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("serve: machine health monitor stopped", "err", err)
		}
	}()

	serverOpts := server.Options{
		Token: cfg.Server.Token, Version: Version,
		ControllerURL: cfg.Orchard.URL, ServerURL: cfg.Server.URL,
		GitAuth: gitAuthStore, Secrets: secretStore,
	}
	if ts := tailscaleAuthenticator(cfg.Server.Tailscale); ts != nil {
		serverOpts.Tailscale = ts
		serverOpts.TrustWorkerHosts = cfg.Server.Tailscale.TrustWorkerHosts
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
	srv := server.New(oc, nc, ds, ac, serverOpts)

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
		loop.health = poolHealth
		if purger, ok := nomad.AsNodePurger(nc); ok {
			loop.nodes = &fleet.NodeSweeper{Nodes: purger}
		}
		if loads, ok := nomad.AsNodeLoads(nc); ok {
			loop.jobs, loop.vmLoads, loop.tracker = ds, loads, &fleet.LoadTracker{}
		}
		go loop.run(ctx, interval)
	}

	if cfg.Server.Socket != "" {
		uids, err := server.LocalUIDs(cfg.Server.SocketUsers)
		if err != nil {
			return err
		}
		l, err := server.ListenLocal(cfg.Server.Socket, uids, slog.Default())
		if err != nil {
			return fmt.Errorf("server.socket: %w", err)
		}
		local := &http.Server{Handler: srv, ConnContext: server.LocalConnContext, ReadHeaderTimeout: 30 * time.Second}
		go func() {
			if err := local.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("grove serve: local socket stopped", "socket", cfg.Server.Socket, "err", err)
			}
		}()
		slog.Info("grove serve: listening on the local socket", "socket", cfg.Server.Socket, "users", cfg.Server.SocketUsers)
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

	// nodes purges Nomad nodes that stay down (see fleet.NodeSweeper); nil skips that.
	nodes *fleet.NodeSweeper
	// tokenSweepPaused and nodeSweepPaused hold until when a sweep is skipped after Nomad refused
	// it (403): the token in config.yaml can't do it, so retrying every tick only floods the log.
	tokenSweepPaused time.Time
	nodeSweepPaused  time.Time

	// enrolment is what the reconciler remembers about the VMs it has enrolled into Nomad (see
	// fleet.Reconciler.Enrol), and warnedEnrol the enrolment error last logged.
	enrolment   fleet.EnrolState
	warnedEnrol string
	// health is told why pools can't get nodes (enrolment errors, boot loops), so unplaceable
	// jobs fail fast; nil skips that.
	health *fleet.PoolHealth
	// warnedRollCall is the roll call error last logged.
	warnedRollCall string

	// jobs, vmLoads and tracker feed elastic pools' autoscaling (see fleet.JobLoad); nil leaves
	// elastic pools at their current size.
	jobs    dispatch.Service
	vmLoads nomad.NodeLoads
	tracker *fleet.LoadTracker
}

// scaleUpAfter is how long a job must wait unplaced before it counts as demand for more VMs, so
// a job Nomad places within a scheduling pass or two doesn't start one.
const scaleUpAfter = 30 * time.Second

// jobLoad reads the jobs waiting per pool and which VMs are busy, for the autoscaler. It returns
// nil, and elastic pools hold their size this tick, when either can't be read.
func (l *fleetReconcileLoop) jobLoad(ctx context.Context, now time.Time) *fleet.JobLoad {
	if l.jobs == nil || l.vmLoads == nil || l.tracker == nil {
		return nil
	}
	jobs, err := l.jobs.List(ctx)
	if err != nil {
		slog.Warn("serve: fleet reconciler: can't list jobs; elastic pools hold their size", "err", err)
		return nil
	}
	busy, err := l.vmLoads.VMLoads(ctx)
	if err != nil {
		slog.Warn("serve: fleet reconciler: can't read Nomad node load; elastic pools hold their size", "err", err)
		return nil
	}
	return l.tracker.Observe(now, waitingByPool(jobs, now), busy)
}

// waitingByPool counts the jobs waiting unplaced for at least scaleUpAfter, per the pool they
// wait in: the fallback pool for a job that moved there.
func waitingByPool(jobs []dispatch.Job, now time.Time) map[string]int {
	waiting := map[string]int{}
	for _, j := range jobs {
		if j.Status != dispatch.StatusPending || j.AllocID != "" || now.Sub(j.SubmittedAt) < scaleUpAfter {
			continue
		}
		pool := j.Request.Pool
		if j.Fallback != nil {
			pool = j.Fallback.To
		}
		waiting[pool]++
	}
	return waiting
}

// deniedSweepPause is how long a sweep Nomad refused with a 403 is skipped before trying again,
// in case the operator has fixed the token in the meantime.
const deniedSweepPause = time.Hour

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
	options, err := fleetOptions(l.cfg)
	if err != nil {
		slog.Warn("serve: fleet reconciler: invalid Nomad endpoint; skipping", "err", err)
		l.status.Report(fleet.Plan{}, err, time.Now())
		return
	}

	options.Load = l.jobLoad(ctx, time.Now())
	r := &fleet.Reconciler{
		Client:  l.client,
		Spec:    spec,
		Options: options,
	}

	plan, err := r.Plan(ctx)
	if err != nil {
		slog.Warn("serve: fleet reconciler: plan failed", "err", err)
		l.status.Report(fleet.Plan{}, err, time.Now())
		return
	}

	// Before applying the plan, whose failure ends the tick: a VM waiting for its Nomad identity
	// must not wait on an unrelated create.
	enrolErr := r.Enrol(ctx, &l.enrolment)
	if l.health != nil {
		l.health.ObserveEnrol(enrolErr)
		if vms, verr := l.client.ListVMs(ctx); verr == nil {
			l.health.ObserveFleet(vms, plan)
		}
	}
	if err := enrolErr; err != nil {
		if msg := err.Error(); msg != l.warnedEnrol {
			slog.Warn("serve: fleet reconciler: enrolling VMs into Nomad failed", "err", err)
			l.warnedEnrol = msg
		}
	} else {
		l.warnedEnrol = ""
	}

	actions, err := r.RollCall(ctx)
	for _, a := range actions {
		slog.Warn("serve: fleet reconciler: roll call took a Nomad node out of service",
			"node", a.NodeID, "name", a.Name, "reason", a.Reason, "deletedVM", a.DeletedVM)
	}
	if err != nil {
		if msg := err.Error(); msg != l.warnedRollCall {
			slog.Warn("serve: fleet reconciler: roll call failed", "err", err)
			l.warnedRollCall = msg
		}
	} else {
		l.warnedRollCall = ""
	}

	if !plan.Empty() {
		slog.Info("fleet: " + planSummary(plan))
		if err := r.Apply(ctx, plan); err != nil {
			slog.Warn("serve: fleet reconciler: apply failed", "err", err)
			l.status.Report(plan, err, time.Now())
			return
		}
		if l.tracker != nil {
			l.tracker.Applied(plan, time.Now())
		}
	}
	// Every tick, not only when the plan changed something: a deleted VM's token is revoked
	// once Orchard has actually removed the VM, usually ticks after the delete.
	now := time.Now()
	if now.After(l.tokenSweepPaused) {
		if err := r.SweepNodeTokens(ctx); errors.Is(err, nomad.ErrPermissionDenied) {
			l.tokenSweepPaused = now.Add(deniedSweepPause)
			slog.Warn("serve: fleet reconciler: Nomad refused to list ACL tokens, so per-VM node tokens are never revoked; "+
				"set config.yaml nomad.token to a management token (`nomad acl bootstrap`, or `nomad acl token create -type=management`). Retrying in "+deniedSweepPause.String(), "err", err)
		} else if err != nil {
			slog.Warn("serve: fleet reconciler: node token sweep failed", "err", err)
		}
	}
	if l.nodes != nil && now.After(l.nodeSweepPaused) {
		purged, err := l.nodes.Sweep(ctx)
		if purged > 0 {
			slog.Info("serve: fleet reconciler: purged Nomad nodes that stayed down", "count", purged)
		}
		if errors.Is(err, nomad.ErrPermissionDenied) {
			l.nodeSweepPaused = now.Add(deniedSweepPause)
			slog.Warn("serve: fleet reconciler: Nomad refused to purge down nodes (needs node:write); "+
				"set config.yaml nomad.token to a management token. Retrying in "+deniedSweepPause.String(), "err", err)
		} else if err != nil {
			slog.Warn("serve: fleet reconciler: down-node sweep failed", "err", err)
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
		part := "+create " + c.Spec.Name
		if c.Reason != "" {
			part += " (" + c.Reason + ")"
		}
		parts = append(parts, part)
	}
	for _, d := range plan.Deletes {
		parts = append(parts, "-delete "+d.Name+" ("+d.Reason+")")
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
		runnerImage, err := dispatch.RunnerImageOrDefault(p.RunnerImage)
		if err != nil {
			return nil, fmt.Errorf("pool %q runnerImage: %w", p.Name, err)
		}
		pools = append(pools, dispatch.PoolConfig{
			Name:        p.Name,
			RunnerImage: runnerImage,
			CPU:         int(p.JobCPUOrDefault()),
			Memory:      int(p.JobMemoryOrDefault()),
			Isolated:    p.Isolated(),
			Slots:       sealedVMSlots(p),
			OS:          poolOS(p),
			Fallback:    spec.FallbackFor(p),
			NodePool:    nodePoolFor(cfg, p),
		})
	}
	return pools, nil
}

// nodePoolFor is the Nomad node pool p's jobs are pinned to: its own name for a shared pool once
// Nomad runs over mutual TLS, since only then do its VMs enrol into that node pool (see
// fleet.Reconciler.Enrol); "" otherwise.
func nodePoolFor(cfg *config.Config, p fleet.Pool) string {
	if cfg.Nomad.NodeCert == "" || p.Isolated() {
		return ""
	}
	return p.Name
}

// poolOS is the guest OS of p's VMs, as `grove sandbox run --os` takes it: sandbox.OSDarwin for
// the macos pool, sandbox.OSLinux for every other.
func poolOS(p fleet.Pool) string {
	if p.MacOS() {
		return sandbox.OSDarwin
	}
	return sandbox.OSLinux
}

// sealedVMGuestReserveMiB is the memory a sealed VM keeps for its own OS and docker daemon when
// sealedVMSlots works out how many jobs fit.
const sealedVMGuestReserveMiB = 512

// sealedVMSlots is how many jobs one VM of a sealed pool runs at once: as many as fit its CPU (at
// Nomad's 2000 MHz per core) and memory at the pool's per-job sizing, and at least one. Nomad
// does this sum for a shared pool's VMs; a sealed VM runs no Nomad client. A sealed macOS VM runs
// exactly one job, and is then deleted and recreated (see internal/sandbox).
func sealedVMSlots(p fleet.Pool) int {
	if !p.Isolated() {
		return 0
	}
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

// tailscaleAuthenticator builds the Tailscale identity authenticator for `grove serve`, or nil
// when it is disabled or this machine has no tailscale CLI (tokens then remain the only way in).
func tailscaleAuthenticator(cfg config.ServerTailscaleConfig) *tsauth.Authenticator {
	if cfg.Disabled {
		slog.Info("serve: Tailscale identity auth disabled by server.tailscale.disabled")
		return nil
	}
	bin, err := install.FindTailscale(nil, nil)
	if err != nil {
		slog.Info("serve: Tailscale identity auth off: no usable tailscale CLI", "err", err)
		return nil
	}
	slog.Info("serve: Tailscale identity auth on", "tailscale", bin, "configGrants", len(cfg.Grants),
		"policyCapability", tsauth.CapabilityName)
	return tsauth.New(tsauth.CLIWhoIs{Runner: install.ExecRunner{}, Bin: bin}, cfg.Grants)
}
