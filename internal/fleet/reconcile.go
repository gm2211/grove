package fleet

import (
	"bytes"
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
	// TailscaleAuthKey is ignored: no VM joins the tailnet. A shared VM reaches the control
	// plane's Nomad through its host Mac's NAT instead, and a tailnet key in a VM's startup script
	// was readable by anything that can list Orchard's VMs.
	TailscaleAuthKey string
	// NomadRPCAddress, when set, points worker clients at the Nomad RPC listener. The CLI derives
	// it from the configured Nomad HTTP URL and resolves a host name to an IP address, since a
	// guest resolves names through public resolvers only (see ResolveNomadRPCAddress).
	NomadRPCAddress string
	// NodeTokens, when set, gives every VM it creates its own node-scoped Nomad ACL token (in the
	// shutdown script only, so the VM can drain itself) and turns on ACL enforcement in the VM's
	// Nomad client. Set it when the control plane's Nomad runs with ACLs on, i.e. config.yaml has
	// a nomad.token. Nil leaves both out.
	NodeTokens nomad.NodeTokens
	// NodeDrainer, when set, drains a VM's Nomad node from the control plane before Apply deletes
	// the VM and before Orchard recycles it at its TTL (see RollCall), and takes nodes the roll
	// call doesn't recognise out of service. Set it with NodeTokens: a VM's own token is read-only,
	// so the VM can't drain itself.
	NodeDrainer NodeDrainer
	// NodeTLS, when set, puts every non-sealed VM's Nomad client on mutual TLS in its pool's node
	// pool, and holds it back until Enrol has pushed in the VM's own certificate (NodeCerts) and
	// introduction token (NodeIntros), which Nodes and NodePools are needed for too. Set all of
	// them when the control plane's Nomad runs TLS with strict client introduction (config.yaml
	// names nomad.nodeCert): without them its VMs cannot register.
	NodeTLS    *NodeTLS
	NodeCerts  NodeCertIssuer
	NodeIntros nomad.NodeIntros
	Nodes      NodeLister
	NodePools  nomad.NodePools
	// Load sizes elastic pools (see Pool.MaxPerWorker and autoscale.go). Nil keeps each elastic
	// pool at its current size, clamped to its bounds.
	Load *JobLoad
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
	// Reason is set when autoscaling adds the VM.
	Reason string
}

// PlannedDelete is one VM the Reconciler intends to delete.
type PlannedDelete struct {
	Name   string
	Pool   string
	Worker string
	Reason string
	// Swap marks a VM removed to make room for another pool's (see SwapCooldown).
	Swap bool
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
	// isolationProblem). A blocked pool gets no new VMs; its already-isolated VMs keep
	// running, and any other VM of it is deleted, so a pool asked to be isolated never keeps an
	// unisolated VM.
	Blocked []PlannedBlock
	// Unmatched lists the shared pools that no online, schedulable worker can host: no VM of
	// them can exist until a matching worker appears, so their jobs can never be placed.
	Unmatched []string
	// Paused lists, per shared pool, the workers matching its selector that are paused
	// (Orchard SchedulingPaused): a snooze, an operator pause or a health pause. Additive: the
	// pool may still have schedulable workers.
	Paused []PlannedPause
	// PausedOnly is the subset of Paused whose pool has NO schedulable worker, only paused ones.
	// Such a pool is not in Unmatched: capacity exists and may come back (a snooze ends), so the
	// job service can queue its jobs instead of failing them.
	PausedOnly []PlannedPause
}

// PlannedPause is the paused workers that match one pool's selector.
type PlannedPause struct {
	Pool    string
	Workers []string
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
	unsafeHosts := make(map[string]string) // "pool/worker" → guestPasswordBlock
	scaled := r.scale(workers, vms)

	for _, pool := range r.Spec.Pools {
		if problem := isolationProblem(pool); problem != "" {
			plan.Blocked = append(plan.Blocked, PlannedBlock{Pool: pool.Name, Reason: problem})
			blocked[pool.Name] = problem

			// Already-isolated VMs are safe to leave running until the configuration is fixed;
			// every other VM of this pool falls through to the sweep below and is deleted.
			for _, vm := range vms {
				vmPool, worker, _, ok := ParseVMName(vm.Name)
				if ok && vmPool == pool.Name && vm.Labels[LabelWorkerPin] == worker && IsolatedVM(vm) {
					desired[vm.Name] = true
				}
			}
			continue
		}

		matched := false
		var pausedMatch []string
		for _, worker := range workers {
			if worker.SchedulingPaused && labelsContain(worker.Labels, pool.WorkerSelector) {
				pausedMatch = append(pausedMatch, worker.Name)
			}
			if worker.Offline || worker.SchedulingPaused {
				continue
			}

			if !labelsContain(worker.Labels, pool.WorkerSelector) {
				continue
			}
			matched = true

			if problem := guestPasswordBlock(pool, worker); problem != "" {
				plan.Blocked = append(plan.Blocked, PlannedBlock{Pool: pool.Name, Reason: problem})
				unsafeHosts[pool.Name+"/"+worker.Name] = problem
				continue // and any VM it has there is swept below
			}

			indexes := scaled.poolIndexes(pool.Name, worker.Name)
			if !pool.Elastic() {
				indexes = make([]int, pool.PerWorker)
				for n := range indexes {
					indexes[n] = n
				}
			}
			for _, n := range indexes {
				name := VMName(pool.Name, worker.Name, n)
				desired[name] = true

				spec := r.vmSpec(pool, worker.Name, name)

				existing, ok := findVM(vms, name)
				reason := "spec changed"
				if ok && !specDrifted(spec, existing) {
					why, due := r.unhealthyVMDue(existing)
					if why == "" || !due {
						continue // already up to date, or unhealthy too recently to rebuild yet
					}
					reason = why
				}

				if ok {
					plan.Deletes = append(plan.Deletes, PlannedDelete{
						Name:   name,
						Pool:   pool.Name,
						Worker: worker.Name,
						Reason: reason,
					})
				}

				plan.Creates = append(plan.Creates, PlannedCreate{
					Pool:   pool.Name,
					Worker: worker.Name,
					Spec:   spec,
					Reason: scaled.createReasons[name],
				})
			}
		}
		if len(pausedMatch) > 0 && !pool.Isolated() {
			sort.Strings(pausedMatch)
			plan.Paused = append(plan.Paused, PlannedPause{Pool: pool.Name, Workers: pausedMatch})
			if !matched {
				plan.PausedOnly = append(plan.PausedOnly, PlannedPause{Pool: pool.Name, Workers: pausedMatch})
			}
		}
		if !matched && !pool.Isolated() && !(len(pausedMatch) > 0) {
			plan.Unmatched = append(plan.Unmatched, pool.Name)
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
		} else if problem, ok := unsafeHosts[pool+"/"+worker]; ok {
			reason = "unsafe: " + problem
		} else if why, ok := scaled.deleteReasons[vm.Name]; ok {
			reason = why
		}

		plan.Deletes = append(plan.Deletes, PlannedDelete{
			Name:   vm.Name,
			Pool:   pool,
			Worker: hostOf(vm, worker),
			Reason: reason,
			Swap:   scaled.swaps[vm.Name],
		})
	}

	sort.Slice(plan.Creates, func(i, j int) bool { return plan.Creates[i].Spec.Name < plan.Creates[j].Spec.Name })
	sort.Slice(plan.Deletes, func(i, j int) bool { return plan.Deletes[i].Name < plan.Deletes[j].Name })

	return plan, nil
}

// FailedVMRetry is how long a VM may be unhealthy (see unhealthyVMDue) before the Reconciler
// deletes and recreates it. Orchard's restart policy only covers a VM that stops on its own; a VM
// whose start failed (image pull, not enough room on the host, a broken disk) stays "failed"
// forever, and so does the slot it holds, so no job of its pool can ever be placed there. The
// wait bounds the churn when the failure is permanent: one rebuild per VM per FailedVMRetry.
const FailedVMRetry = 10 * time.Minute

// BootLoopRestarts is how many times Orchard may restart a VM before the Reconciler counts it as
// boot-looping and rebuilds it. A healthy VM lives one TTL with no restart at all; one whose
// startup script fails is restarted by OnFailure over and over (tens of thousands of times, each
// boot leaving a dead Nomad node behind) and never takes a job.
const BootLoopRestarts = 3

// unhealthyVMDue says why vm is unhealthy ("" when it isn't) and whether it has been around long
// enough (judged from its creation, the only timestamp Orchard keeps for a VM that never started)
// to rebuild it now.
func (r *Reconciler) unhealthyVMDue(vm orchard.VM) (why string, due bool) {
	switch {
	case vm.Status == "failed":
		why = "failed"
		if vm.StatusMessage != "" {
			why += ": " + vm.StatusMessage
		}
	case vm.RestartCount >= BootLoopRestarts:
		why = fmt.Sprintf("boot loop: restarted %d times", vm.RestartCount)
	default:
		return "", false
	}
	return why, vm.CreatedAt.IsZero() || r.Options.now().Sub(vm.CreatedAt) >= FailedVMRetry
}

// Apply executes a Plan: deletes first (so a spec-changed VM's old copy is gone before its
// replacement is created), then creates.
func (r *Reconciler) Apply(ctx context.Context, plan Plan) error {
	r.drainBeforeDelete(ctx, plan.Deletes)
	for _, d := range plan.Deletes {
		if err := r.Client.DeleteVM(ctx, d.Name); err != nil {
			return fmt.Errorf("delete vm %s: %w", d.Name, err)
		}
	}

	for _, c := range plan.Creates {
		spec, err := withGuestPassword(c.Spec)
		if err != nil {
			return fmt.Errorf("create vm %s: %w", c.Spec.Name, err)
		}
		spec, err = r.withNodeToken(ctx, spec)
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
	var enrolment EnrolState

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

		if err := r.Enrol(ctx, &enrolment); err != nil {
			logger.Printf("fleet: enrolment failed: %v", err)
		}

		actions, err := r.RollCall(ctx)
		for _, a := range actions {
			logger.Printf("fleet: roll call took Nomad node %s (%s) out of service: %s", a.NodeID, a.Name, a.Reason)
		}
		if err != nil {
			logger.Printf("fleet: roll call failed: %v", err)
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
	// A failed VM may never have been given any either; FailedVMRetry decides when it's rebuilt.
	if existing.Status == "pending" || existing.Status == "failed" {
		return false
	}

	return desired.CPU != existing.CPU || desired.Memory != existing.Memory
}

var nodeACLBlocksRE = regexp.MustCompile(`(?s)# grove:(node-token|client-acl) begin\n.*?# grove:(node-token|client-acl) end\n\n?`)

// withoutNodeACL strips the node-token and client-acl blocks (see scripts.go) from a script before
// drift comparison. Each VM's token differs, and turning Nomad ACLs on shouldn't recreate every
// healthy VM at once: VMs made before it pick the blocks up on their next recycle.
//
// The guest password block is normalized rather than stripped: its password differs per VM, but
// whether a VM has the block at all must count.
func withoutNodeACL(script string) string {
	script = nodeACLBlocksRE.ReplaceAllString(script, "")
	return guestPasswordBlockRE.ReplaceAllLiteralString(script, guestPasswordScript)
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

// ResolveNomadRPCAddress turns a host:port RPC address into ip:port with lookup, preferring IPv4.
// A guest resolves names only through public resolvers (isolatedDNSServers), so a tailnet name
// like the control plane's MagicDNS one would never resolve inside it; the control plane resolves
// it instead, and the guest dials the address through its host Mac's NAT. An address that is
// already an IP, or "", comes back unchanged. A failed lookup is an error, never a fallback to the
// name: that would change every VM's startup script, and so recreate the fleet, on a DNS hiccup.
func ResolveNomadRPCAddress(ctx context.Context, address string, lookup func(ctx context.Context, network, host string) ([]net.IP, error)) (string, error) {
	if address == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	if net.ParseIP(host) != nil {
		return address, nil
	}
	ips, err := lookup(ctx, "ip", host)
	if err != nil {
		return "", fmt.Errorf("resolve Nomad host %s: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("resolve Nomad host %s: no addresses", host)
	}
	// The address lands in every VM's startup script, so the pick must not depend on the order
	// the resolver happens to answer in: the lowest IPv4 address, else the lowest IPv6 one.
	slices.SortFunc(ips, func(a, b net.IP) int {
		if a4, b4 := a.To4() != nil, b.To4() != nil; a4 != b4 {
			if a4 {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.To16(), b.To16())
	})
	return net.JoinHostPort(ips[0].String(), port), nil
}
