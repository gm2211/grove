package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

// NodeDrainer drains Nomad nodes. The concrete nomad client implements it (nomad.Client's
// DrainNode).
type NodeDrainer interface {
	DrainNode(ctx context.Context, nodeID string, enable bool, deadline time.Duration) error
}

// drainDeadline is how long a recycled VM's allocations get to finish once the control plane
// drains its node; the shutdown script then waits for them (see nomadDrainDeadline).
const drainDeadline = 2 * time.Hour

// ttlDrainLead is how long before Orchard recycles a VM at its TTL the control plane drains its
// node, so no new job lands on it while its shutdown script waits for the last ones. A few
// reconciler ticks.
const ttlDrainLead = 2 * time.Minute

// RollCallAction is one Nomad node the roll call took out of service.
type RollCallAction struct {
	NodeID string
	Name   string
	Reason string
	// DeletedVM is the VM deleted along with it, if any, so the fleet rebuilds it.
	DeletedVM string
}

// RollCall checks every live, eligible Nomad node against Orchard's VMs, the one record of what
// grove created. A node that names no VM, more than one node claiming the same VM, or a node
// outside its pool's node pool is drained, which leaves it ineligible for good: Nomad never places
// another job there (a purge would not do: a live node comes straight back with the identity it
// holds). The VM it claims, if any, is deleted so the fleet rebuilds it with a fresh identity. The
// control plane's own sandbox-host node is left alone.
//
// Orchard forgets a VM as soon as it is deleted, while its shutdown script still waits for its
// jobs; grove drains a VM's node before it deletes it (Apply) or before Orchard recycles it at its
// TTL (below), so such a node is already ineligible and the roll call passes over it. One deleted
// some other way (`orchard delete vm`) is drained here instead: its running jobs still get
// drainDeadline to finish.
//
// It also drains, ahead of time, every shared VM that Orchard will recycle at its TTL within
// ttlDrainLead: VMs can't drain themselves (their node token is read-only), so this is what stops
// new jobs landing on a VM whose shutdown script is waiting out its last ones.
//
// A no-op without Options.NodeTLS: without strict client introduction anything may join anyway.
func (r *Reconciler) RollCall(ctx context.Context) ([]RollCallAction, error) {
	o := r.Options
	if o.NodeTLS == nil || o.Nodes == nil || o.NodeDrainer == nil {
		return nil, nil
	}
	vms, err := r.Client.ListVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	nodes, err := o.Nodes.ListNodeSummaries(ctx)
	if err != nil {
		return nil, err
	}

	byName := map[string]orchard.VM{}
	for _, vm := range vms {
		byName[vm.Name] = vm
	}
	live := map[string][]nomad.NodeSummary{}
	for _, n := range nodes {
		if n.Status == "down" || !n.Eligible || n.Name == nomad.SandboxHostNodeName {
			continue
		}
		live[n.Name] = append(live[n.Name], n)
	}
	names := make([]string, 0, len(live))
	for name := range live {
		names = append(names, name)
	}
	sort.Strings(names)

	var actions []RollCallAction
	var errs []error
	now := o.now()
	for _, name := range names {
		claimed := live[name]
		vm, known := byName[name]
		pool, _, _, parsed := ParseVMName(name)
		reason := ""
		switch {
		case !known || !parsed:
			reason = "no VM of this name in Orchard"
		case len(claimed) > 1:
			reason = fmt.Sprintf("%d Nomad nodes claim this VM", len(claimed))
		case claimed[0].NodePool != pool:
			reason = fmt.Sprintf("registered in node pool %q, not %q", claimed[0].NodePool, pool)
		default:
			if vm.TTL > 0 && !vm.CreatedAt.IsZero() && !now.Before(vm.CreatedAt.Add(vm.TTL-ttlDrainLead)) {
				if err := o.NodeDrainer.DrainNode(ctx, claimed[0].ID, true, drainDeadline); err != nil {
					errs = append(errs, fmt.Errorf("drain %s before its TTL: %w", name, err))
				}
			}
			continue
		}

		deleted := ""
		for _, n := range claimed {
			if err := o.NodeDrainer.DrainNode(ctx, n.ID, true, drainDeadline); err != nil {
				errs = append(errs, fmt.Errorf("take node %s (%s) out of service: %w", n.ID, name, err))
				continue
			}
			actions = append(actions, RollCallAction{NodeID: n.ID, Name: name, Reason: reason})
		}
		if known && parsed {
			if err := r.Client.DeleteVM(ctx, name); err != nil {
				errs = append(errs, fmt.Errorf("delete vm %s: %w", name, err))
			} else {
				deleted = name
			}
		}
		for i := range actions {
			if actions[i].Name == name {
				actions[i].DeletedVM = deleted
			}
		}
	}
	return actions, errors.Join(errs...)
}

// drainBeforeDelete drains the Nomad node of every shared VM in deletes from the control plane,
// letting its allocations finish (see drainDeadline), before Apply deletes the VM. Best effort: a
// failure is logged and the VM is still deleted, since its shutdown script still waits for its
// allocations. A no-op without Options.NodeDrainer.
func (r *Reconciler) drainBeforeDelete(ctx context.Context, deletes []PlannedDelete) {
	o := r.Options
	if o.NodeDrainer == nil || o.Nodes == nil || len(deletes) == 0 {
		return
	}
	nodes, err := o.Nodes.ListNodeSummaries(ctx)
	if err != nil {
		o.logger().Printf("fleet: can't list Nomad nodes to drain deleted VMs: %v", err)
		return
	}
	ids := map[string][]string{}
	for _, n := range nodes {
		if n.Status != "down" && n.Eligible {
			ids[n.Name] = append(ids[n.Name], n.ID)
		}
	}
	for _, d := range deletes {
		for _, id := range ids[d.Name] {
			if err := o.NodeDrainer.DrainNode(ctx, id, true, drainDeadline); err != nil {
				o.logger().Printf("fleet: draining %s before deleting it: %v", d.Name, err)
			}
		}
	}
}
