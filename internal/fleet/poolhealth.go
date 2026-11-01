package fleet

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/snooze"
)

// WorkerBootLoop is the VMs of one Orchard worker that keep restarting (see BootLoopRestarts).
type WorkerBootLoop struct {
	Worker string
	VMs    []string
	// MaxRestarts is the highest restart count among VMs.
	MaxRestarts uint64
	// Last is when the most recently (re)started of those VMs began running; zero when unknown.
	Last time.Time
}

// BootLoops groups the boot-looping VMs in vms by the worker they run on, sorted by worker.
func BootLoops(vms []orchard.VM) []WorkerBootLoop {
	byWorker := map[string]*WorkerBootLoop{}
	for _, vm := range vms {
		if vm.RestartCount < BootLoopRestarts {
			continue
		}
		w := byWorker[vm.Worker]
		if w == nil {
			w = &WorkerBootLoop{Worker: vm.Worker}
			byWorker[vm.Worker] = w
		}
		w.VMs = append(w.VMs, vm.Name)
		if vm.RestartCount > w.MaxRestarts {
			w.MaxRestarts = vm.RestartCount
		}
		last := vm.StartedAt
		if last.IsZero() {
			last = vm.CreatedAt
		}
		if last.After(w.Last) {
			w.Last = last
		}
	}
	out := make([]WorkerBootLoop, 0, len(byWorker))
	for _, w := range byWorker {
		sort.Strings(w.VMs)
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Worker < out[j].Worker })
	return out
}

// PoolHealth remembers, from the fleet reconciler's ticks, why a shared pool has no node that can
// take a job, so the job service can fail a job fast with that reason instead of letting it sit
// until its pending timeout. Safe for concurrent use; the zero value is ready.
type PoolHealth struct {
	// Pauses, when set, says why paused workers are paused, so a pool whose workers are only
	// snoozed can wait for them (WaitReason) and an operator-paused pool gets a clearer failure.
	// Set it before the first Observe call.
	Pauses interface {
		Get(worker string) snooze.Entry
	}

	mu sync.Mutex

	enrolPool   map[string]string // pool -> last enrolment error
	enrolGlobal string            // enrolment error that names no pool (listing VMs or nodes failed)
	noWorker    map[string]bool
	blocked     map[string]string
	bootLooping map[string]int      // pool -> boot-looping VMs
	healthy     map[string]int      // pool -> running VMs that are not boot-looping
	paused      map[string][]string // pool -> paused workers matching it
	pausedOnly  map[string]bool     // pool -> every matching worker is paused
}

// ObserveEnrol records the outcome of one Enrol pass: nil clears every enrolment problem.
func (h *PoolHealth) ObserveEnrol(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enrolPool, h.enrolGlobal = map[string]string{}, ""
	for _, e := range flattenErrors(err) {
		var ee *EnrolError
		if errors.As(e, &ee) && ee.Pool != "" {
			h.enrolPool[ee.Pool] = e.Error()
		} else {
			h.enrolGlobal = e.Error()
		}
	}
}

// ObserveFleet records the VMs Orchard has and the plan computed from them.
func (h *PoolHealth) ObserveFleet(vms []orchard.VM, plan Plan) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noWorker, h.blocked = map[string]bool{}, map[string]string{}
	h.bootLooping, h.healthy = map[string]int{}, map[string]int{}
	h.paused, h.pausedOnly = map[string][]string{}, map[string]bool{}
	for _, p := range plan.Unmatched {
		h.noWorker[p] = true
	}
	for _, p := range plan.Paused {
		h.paused[p.Pool] = append([]string(nil), p.Workers...)
	}
	for _, p := range plan.PausedOnly {
		h.pausedOnly[p.Pool] = true
	}
	for _, b := range plan.Blocked {
		h.blocked[b.Pool] = b.Reason
	}
	for _, vm := range vms {
		pool, _, _, ok := ParseVMName(vm.Name)
		if !ok {
			continue
		}
		switch {
		case vm.RestartCount >= BootLoopRestarts:
			h.bootLooping[pool]++
		case vm.Status == "running":
			h.healthy[pool]++
		}
	}
}

// UnplaceableReason says why no job of pool can currently get a node, or "" when nothing known
// stops the pool from getting one. It is only evidence: the caller must also have seen that the
// pool has no eligible Nomad node.
func (h *PoolHealth) UnplaceableReason(pool string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.pausedOnly[pool]:
		return h.pausedReasonLocked(pool)
	case h.noWorker[pool]:
		return fmt.Sprintf("no worker matches pool %s", pool)
	case h.blocked[pool] != "":
		return fmt.Sprintf("pool %s has no ready node: pool blocked: %s", pool, h.blocked[pool])
	case h.enrolPool[pool] != "":
		return fmt.Sprintf("pool %s has no ready node: enrollment failing: %s", pool, h.enrolPool[pool])
	case h.enrolGlobal != "":
		return fmt.Sprintf("pool %s has no ready node: enrollment failing: %s", pool, h.enrolGlobal)
	case h.bootLooping[pool] > 0 && h.healthy[pool] == 0:
		return fmt.Sprintf("pool %s VMs boot-looping (%d restarting, none healthy)", pool, h.bootLooping[pool])
	}
	return ""
}

// pausedReasonLocked says why every worker of pool is paused, for a job that can not wait.
func (h *PoolHealth) pausedReasonLocked(pool string) string {
	var parts []string
	for _, w := range h.paused[pool] {
		why := "paused in Orchard"
		if h.Pauses != nil {
			if rs := h.Pauses.Get(w).Reasons(time.Now()); len(rs) > 0 {
				why = strings.Join(rs, " + ") + " pause"
			}
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", w, why))
	}
	return fmt.Sprintf("every worker for pool %s is paused: %s; resume one with `grove worker resume <worker>`", pool, strings.Join(parts, ", "))
}

// WaitReason says whether a snooze is a reason for a pool's job to keep waiting. Workers of a pool
// are fungible and the queue is per pool: a job is never held for one worker. The snooze matters
// only (a) to keep the job from being failed when no other worker could ever run it, and (b) to
// push its pending timeout past the snooze, since a busy pool may stay busy until then.
//
// reason is the Job.PendingReason to show ("" when no snooze concerns the pool); until is the end
// of the latest snooze among the pool's workers; exclusive is true when no worker other than the
// snoozed ones could ever run the pool's jobs (the job must not be failed as unplaceable).
func (h *PoolHealth) WaitReason(pool string, now time.Time) (reason string, until time.Time, exclusive bool) {
	if h == nil || h.Pauses == nil {
		return "", time.Time{}, false
	}
	h.mu.Lock()
	workers := append([]string(nil), h.paused[pool]...)
	exclusive = h.pausedOnly[pool]
	h.mu.Unlock()

	var parts []string
	for _, w := range workers {
		e := h.Pauses.Get(w)
		if !e.Active(now) {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s snoozed until %s", w, e.Snooze.Until.Local().Format("15:04")))
		if e.Snooze.Until.After(until) {
			until = e.Snooze.Until
		}
	}
	if len(parts) == 0 {
		return "", time.Time{}, false
	}
	if exclusive {
		return fmt.Sprintf("queued: workers for pool %s snoozed (%s)", pool, strings.Join(parts, ", ")), until, true
	}
	return fmt.Sprintf("waiting for a free %s worker (%s)", pool, strings.Join(parts, ", ")), until, false
}

func flattenErrors(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, flattenErrors(e)...)
		}
		return out
	}
	return []error{err}
}

// Describe renders w as "worker mac1: 2 VMs boot-looping (up to 9 restarts, last started <t>)".
func (w WorkerBootLoop) Describe() string {
	s := fmt.Sprintf("worker %s: %d VM(s) boot-looping (up to %d restarts", w.Worker, len(w.VMs), w.MaxRestarts)
	if !w.Last.IsZero() {
		s += ", last started " + w.Last.UTC().Format(time.RFC3339)
	}
	return s + "): " + strings.Join(w.VMs, ", ")
}
