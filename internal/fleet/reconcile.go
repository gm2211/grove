package fleet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

// LabelWorkerPin mirrors github.com/cirruslabs/orchard/pkg/resource/v1.LabelWorkerName (the fork
// can't be imported from this package without pulling Orchard's own types into fleet's contract
// surface — see internal/orchard/client.go's vmToV1, which sets this same key). Every VM grove
// creates carries it, pinning the VM to the worker it was planned for; the Reconciler also reads
// it back as the fallback source of a VM's "host" while the VM is still pending and Orchard's own
// Worker field is empty (see PoolAndHost below).
const LabelWorkerPin = "org.cirruslabs.orchard.worker-name"

// Options tunes the Reconciler's behaviour. The zero value is usable.
type Options struct {
	// TailscaleAuthKey, when set, makes generated StartupScripts join the tailnet (see
	// scripts.go). Typically config.Config.Tailscale.AuthKey.
	TailscaleAuthKey string
	// TailscaleTags are the ACL tags isolated VMs claim when they join the tailnet; empty means
	// DefaultIsolatedTailscaleTag. Typically config.Config.Tailscale.Tags.
	TailscaleTags []string
	// NomadRPCAddress, when set, points macOS and isolated worker clients at the Nomad RPC
	// listener. It is derived from the configured Nomad HTTP URL by the CLI.
	NomadRPCAddress string
	// NodeTokens, when set, gives every VM it creates its own node-scoped Nomad ACL token (in the
	// shutdown script only, so the VM can drain itself) and turns on ACL enforcement in the VM's
	// Nomad client. Set it when the control plane's Nomad runs with ACLs on, i.e. config.yaml has
	// a nomad.token. Nil leaves both out.
	NodeTokens nomad.NodeTokens
	// Now returns the current time; defaults to time.Now. Overridable for tests.
	Now func() time.Time
	// Logger receives Run's per-tick errors without stopping the loop. Defaults to log.Default().
	Logger *log.Logger
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}

	return time.Now()
}

func (o Options) logger() *log.Logger {
	if o.Logger != nil {
		return o.Logger
	}

	return log.Default()
}

// Reconciler makes Orchard match a fleet Spec: for every online, non-paused worker whose labels
// satisfy a pool's WorkerSelector, it ensures pool.PerWorker VMs exist, pinned to that worker;
// VMs that are no longer desired (worker gone offline/paused, selector no longer matches,
// perWorker shrank) or whose spec has drifted are deleted.
type Reconciler struct {
	Client  orchard.Client
	Spec    *Spec
	Options Options
}

// PlannedCreate is one VM the Reconciler intends to create.
type PlannedCreate struct {
	Pool   string
	Worker string
	Spec   orchard.VMSpec
}

// PlannedDelete is one VM the Reconciler intends to delete.
type PlannedDelete struct {
	Name   string
	Pool   string
	Worker string
	Reason string
}

// PlannedBlock is a pool the Reconciler refused to build VMs for, and why.
type PlannedBlock struct {
	Pool   string
	Reason string
}

// Plan is the result of comparing desired state (Spec) against observed state (Orchard).
type Plan struct {
	Creates []PlannedCreate
	Deletes []PlannedDelete
	// Blocked lists pools whose VMs can't be built with the current configuration (see
	// Options.isolationProblem). A blocked pool gets no new VMs; its already-isolated VMs keep
	// running, and any other VM of it is deleted, so a pool asked to be isolated never keeps an
	// unisolated VM.
	Blocked []PlannedBlock
}

// Empty reports whether applying this Plan would be a no-op.
func (p Plan) Empty() bool {
	return len(p.Creates) == 0 && len(p.Deletes) == 0
}

// BlockedError summarises Blocked as one error, or returns nil when no pool is blocked, so
// callers can report it even when the rest of the plan applied cleanly.
func (p Plan) BlockedError() error {
	if len(p.Blocked) == 0 {
		return nil
	}

	reasons := make([]string, len(p.Blocked))
	for i, b := range p.Blocked {
		reasons[i] = fmt.Sprintf("pool %q blocked: %s", b.Pool, b.Reason)
	}
	return errors.New(strings.Join(reasons, "; "))
}

// Plan lists workers and VMs and computes the creates/deletes needed to match r.Spec.
func (r *Reconciler) Plan(ctx context.Context) (Plan, error) {
	workers, err := r.Client.ListWorkers(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("list workers: %w", err)
	}

	vms, err := r.Client.ListVMs(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("list vms: %w", err)
	}

	var plan Plan

	desired := make(map[string]bool)
	blocked := make(map[string]string)

	for _, pool := range r.Spec.Pools {
		if problem := r.Options.isolationProblem(pool); problem != "" {
			plan.Blocked = append(plan.Blocked, PlannedBlock{Pool: pool.Name, Reason: problem})
			blocked[pool.Name] = problem

			// Already-isolated VMs are safe to leave running until the configuration is fixed;
			// every other VM of this pool falls through to the sweep below and is deleted.
			for _, vm := range vms {
				vmPool, worker, _, ok := ParseVMName(vm.Name)
				if ok && vmPool == pool.Name && vm.Labels[LabelWorkerPin] == worker && isolatedVM(vm) {
					desired[vm.Name] = true
				}
			}
			continue
		}

		for _, worker := range workers {
			if worker.Offline || worker.SchedulingPaused {
				continue
			}

			if !labelsContain(worker.Labels, pool.WorkerSelector) {
				continue
			}

			for n := 0; n < pool.PerWorker; n++ {
				name := VMName(pool.Name, worker.Name, n)
				desired[name] = true

				spec := r.vmSpec(pool, worker.Name, name)

				existing, ok := findVM(vms, name)
				if ok && !specDrifted(spec, existing) {
					continue // already up to date
				}

				if ok {
					plan.Deletes = append(plan.Deletes, PlannedDelete{
						Name:   name,
						Pool:   pool.Name,
						Worker: worker.Name,
						Reason: "spec changed",
					})
				}

				plan.Creates = append(plan.Creates, PlannedCreate{
					Pool:   pool.Name,
					Worker: worker.Name,
					Spec:   spec,
				})
			}
		}
	}

	// Anything grove-managed that isn't desired anymore: worker went offline/paused, pool or
	// workerSelector no longer matches, perWorker shrank, or the pool was removed entirely.
	//
	// VM labels can't tell us that anymore (they're worker selectors now, see Pool.Labels), so
	// "grove-managed" is determined the same way pool/host are derived for display: the name
	// parses as "<pool>-<worker>-<n>" (ParseVMName) *and* the VM is pinned to that same worker
	// (LabelWorkerPin) — every VM grove creates satisfies both, so this only ever picks up VMs
	// grove itself made, never an unrelated resource that happens to share the naming shape.
	for _, vm := range vms {
		pool, worker, _, ok := ParseVMName(vm.Name)
		if !ok || vm.Labels[LabelWorkerPin] != worker {
			continue
		}

		if desired[vm.Name] {
			continue
		}

		reason := "no longer desired"
		if problem, ok := blocked[pool]; ok {
			reason = "pool blocked: " + problem
		}

		plan.Deletes = append(plan.Deletes, PlannedDelete{
			Name:   vm.Name,
			Pool:   pool,
			Worker: hostOf(vm, worker),
			Reason: reason,
		})
	}

	sort.Slice(plan.Creates, func(i, j int) bool { return plan.Creates[i].Spec.Name < plan.Creates[j].Spec.Name })
	sort.Slice(plan.Deletes, func(i, j int) bool { return plan.Deletes[i].Name < plan.Deletes[j].Name })

	return plan, nil
}

// Apply executes a Plan: deletes first (so a spec-changed VM's old copy is gone before its
// replacement is created), then creates.
func (r *Reconciler) Apply(ctx context.Context, plan Plan) error {
	for _, d := range plan.Deletes {
		if err := r.Client.DeleteVM(ctx, d.Name); err != nil {
			return fmt.Errorf("delete vm %s: %w", d.Name, err)
		}
	}

	for _, c := range plan.Creates {
		spec, err := r.withNodeToken(ctx, c.Spec)
		if err != nil {
			return fmt.Errorf("create vm %s: %w", c.Spec.Name, err)
		}
		if _, err := r.Client.CreateVM(ctx, spec); err != nil {
			return fmt.Errorf("create vm %s: %w", c.Spec.Name, err)
		}
	}

	return nil
}

// withNodeToken fills a planned spec's node-token placeholders with a freshly minted per-VM token.
// A spec without the block (Options.NodeTokens nil) is returned unchanged. If minting fails (e.g.
// nomad.token isn't a management token) the VM is still created, without the block, so a token
// problem never shrinks the fleet; its drain on recycle then fails as it did before node tokens. A
// token minted for a VM whose CreateVM then fails is revoked by the next SweepNodeTokens.
func (r *Reconciler) withNodeToken(ctx context.Context, spec orchard.VMSpec) (orchard.VMSpec, error) {
	if !strings.Contains(spec.ShutdownScript, nodeTokenSecretPlaceholder) {
		return spec, nil
	}

	var tok nomad.NodeToken
	err := errors.New("no Nomad node token issuer configured")
	if r.Options.NodeTokens != nil {
		tok, err = r.Options.NodeTokens.CreateNodeToken(ctx, spec.Name)
	}
	if err != nil {
		r.Options.logger().Printf("fleet: creating %s without a Nomad node token (it can't drain itself on recycle): %v", spec.Name, err)
		spec.ShutdownScript = withoutNodeACL(spec.ShutdownScript)
		return spec, nil
	}

	spec.ShutdownScript = strings.NewReplacer(
		nodeTokenSecretPlaceholder, shQuote(tok.SecretID),
		nodeTokenAccessorPlaceholder, tok.AccessorID,
	).Replace(spec.ShutdownScript)
	return spec, nil
}

// nodeTokenGrace is how old an unreferenced node token must be before SweepNodeTokens revokes it,
// so a token minted for a VM that a concurrent `grove fleet apply` is about to create survives.
const nodeTokenGrace = 15 * time.Minute

var nodeTokenAccessorRE = regexp.MustCompile(`(?m)^# grove node token accessor ([0-9A-Za-z-]+) `)

// SweepNodeTokens revokes every per-VM Nomad token (see Options.NodeTokens) that no VM in Orchard
// still holds. A deleted VM keeps its token until Orchard has actually removed it, because its
// shutdown script needs the token to drain. A no-op when Options.NodeTokens is nil.
func (r *Reconciler) SweepNodeTokens(ctx context.Context) error {
	if r.Options.NodeTokens == nil {
		return nil
	}

	vms, err := r.Client.ListVMs(ctx)
	if err != nil {
		return fmt.Errorf("list vms: %w", err)
	}
	held := make(map[string]bool)
	for _, vm := range vms {
		for _, m := range nodeTokenAccessorRE.FindAllStringSubmatch(vm.ShutdownScript, -1) {
			held[m[1]] = true
		}
	}

	tokens, err := r.Options.NodeTokens.ListNodeTokens(ctx)
	if err != nil {
		return err
	}
	cutoff := r.Options.now().Add(-nodeTokenGrace)
	var errs []error
	for _, t := range tokens {
		if held[t.AccessorID] || t.CreateTime.After(cutoff) {
			continue
		}
		if err := r.Options.NodeTokens.RevokeNodeToken(ctx, t.AccessorID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run plans and applies on every tick of interval (and once immediately) until ctx is done.
// A failed tick is logged rather than fatal, since it's usually a transient Orchard error and
// the next tick will retry.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) error {
	logger := r.Options.logger()
	lastBlocked := ""

	tick := func() {
		plan, err := r.Plan(ctx)
		if err != nil {
			logger.Printf("fleet: plan failed: %v", err)
			return
		}

		blocked := ""
		if err := plan.BlockedError(); err != nil {
			blocked = err.Error()
			if blocked != lastBlocked {
				logger.Printf("fleet: %s", blocked)
			}
		}
		lastBlocked = blocked

		if err := r.Apply(ctx, plan); err != nil {
			logger.Printf("fleet: apply failed: %v", err)
		}

		if err := r.SweepNodeTokens(ctx); err != nil {
			logger.Printf("fleet: node token sweep failed: %v", err)
		}
	}

	tick()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			tick()
		}
	}
}

// vmSpec builds the orchard.VMSpec grove wants running for the n-th VM of pool on worker. Its
// Labels are pool.Labels verbatim (nothing derived is added — see Pool.Labels' doc comment) plus
// the worker pin vmToV1 applies from Worker; pool/host bookkeeping lives in the VM's *name* and
// in Worker, not in labels.
func (r *Reconciler) vmSpec(pool Pool, worker, name string) orchard.VMSpec {
	startup := buildStartupScript(pool, worker, name, r.Options)
	shutdown, shutdownTimeout := buildShutdownScript(pool, r.Options.NodeTokens != nil)

	spec := orchard.VMSpec{
		Name:            name,
		Worker:          worker,
		Image:           pool.Image,
		CPU:             pool.CPU,
		Memory:          pool.Memory,
		DiskSize:        pool.DiskSize,
		Labels:          pool.Labels,
		RestartPolicy:   "OnFailure",
		Headless:        true,
		Username:        pool.Username,
		Password:        pool.Password,
		StartupScript:   startup,
		ShutdownScript:  shutdown,
		ShutdownTimeout: shutdownTimeout,
		TTL:             pool.TTL.Std(),
		HostDirs:        hostDirsForPool(pool),
	}

	if pool.Isolated() {
		spec.Softnet = true
		spec.SoftnetBlock = []string{softnetBlockHost}
	}

	return spec
}

// NomadRPCAddressFromHTTPURL derives the default Nomad RPC endpoint from the configured HTTP
// API URL. This assumes a direct Nomad listener on the same host; Nomad's HTTP API and RPC
// listener use ports 4646 and 4647 respectively. A proxy or custom RPC port needs its own
// explicit configuration field.
func NomadRPCAddressFromHTTPURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse Nomad HTTP URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", fmt.Errorf("Nomad HTTP URL must include an http or https host")
	}
	return net.JoinHostPort(u.Hostname(), "4647"), nil
}

func hostDirsForPool(pool Pool) []orchard.HostDir {
	if pool.HostXcode == nil {
		return nil
	}

	return []orchard.HostDir{{Name: "grove-xcode.app", Path: pool.HostXcode.Path, ReadOnly: true}}
}

func findVM(vms []orchard.VM, name string) (orchard.VM, bool) {
	for _, vm := range vms {
		if vm.Name == name {
			return vm, true
		}
	}

	return orchard.VM{}, false
}

func labelsContain(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}

	return true
}

// specDrifted reports whether an already-existing VM no longer matches what fleet.yaml now
// wants, field by field. This replaces the old grove.spec label-hash comparison now that VM
// labels are worker selectors Orchard evaluates (see Pool.Labels) rather than free-form grove
// bookkeeping: there's nowhere left to stash a hash, so the desired orchard.VMSpec (built fresh
// from the current Pool) is compared directly against the observed orchard.VM.
func specDrifted(desired orchard.VMSpec, existing orchard.VM) bool {
	if desired.Image != existing.Image ||
		desired.DiskSize != existing.DiskSize ||
		desired.RestartPolicy != existing.RestartPolicy ||
		withoutNodeACL(desired.StartupScript) != withoutNodeACL(existing.StartupScript) ||
		withoutNodeACL(desired.ShutdownScript) != withoutNodeACL(existing.ShutdownScript) ||
		desired.ShutdownTimeout != existing.ShutdownTimeout ||
		desired.TTL != existing.TTL ||
		!slices.Equal(desired.HostDirs, existing.HostDirs) ||
		desired.Softnet != existing.Softnet ||
		!slices.Equal(desired.SoftnetBlock, existing.SoftnetBlock) {
		return true
	}

	// CPU/Memory come from Orchard's *assigned* resources (v1.VM's AssignedCPU/AssignedMemory —
	// see vmFromV1), which the controller only populates once it has actually scheduled the VM
	// onto a worker. Comparing them while the VM is still "pending" would read them as zero and
	// treat every freshly created, not-yet-scheduled VM as instantly drifted — recreating it
	// forever instead of ever letting it reach a worker. Skip that comparison until Orchard has
	// something to report.
	if existing.Status == "pending" {
		return false
	}

	return desired.CPU != existing.CPU || desired.Memory != existing.Memory
}

var nodeACLBlocksRE = regexp.MustCompile(`(?s)# grove:(node-token|client-acl) begin\n.*?# grove:(node-token|client-acl) end\n\n?`)

// withoutNodeACL strips the node-token and client-acl blocks (see scripts.go) from a script before
// drift comparison. Each VM's token differs, and turning Nomad ACLs on shouldn't recreate every
// healthy VM at once: VMs made before it pick the blocks up on their next recycle.
func withoutNodeACL(script string) string {
	return nodeACLBlocksRE.ReplaceAllString(script, "")
}

// hostOf is a VM's best-known host: the controller-observed Worker once Orchard has actually
// scheduled it, falling back to the worker grove pinned/planned it for (fallback, typically
// parsed from the VM's own name) while it's still pending and Worker is empty.
func hostOf(vm orchard.VM, fallback string) string {
	if vm.Worker != "" {
		return vm.Worker
	}

	return fallback
}

// PoolAndHost derives a VM's pool and host the same way the Reconciler does internally, for
// callers that only observe VMs (internal/server's /fleet normalisation, `grove fleet status`)
// and never plan against them: pool comes from parsing the VM's name (see ParseVMName), and host
// prefers the controller-observed Worker, falling back first to the VM's own worker-pin label and
// then to the worker parsed out of its name, in case Worker is empty because Orchard hasn't
// scheduled it yet.
func PoolAndHost(vm orchard.VM) (pool, host string) {
	pool, worker, _, ok := ParseVMName(vm.Name)
	if !ok {
		worker = ""
	}

	if pin := vm.Labels[LabelWorkerPin]; pin != "" {
		worker = pin
	}

	return pool, hostOf(vm, worker)
}
