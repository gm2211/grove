package fleet

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// Autoscaling timings. See Pool.MinPerWorker/MaxPerWorker.
const (
	// BootGrace is how long a new VM of a pool may take to join Nomad before the pool may scale
	// up again: scaling up one VM at a time keeps a burst of jobs from starting VMs the first new
	// one would have served.
	BootGrace = 10 * time.Minute
	// IdleTimeout is how long a VM above its pool's minimum must sit with no job before it's
	// removed.
	IdleTimeout = 15 * time.Minute
	// SwapIdle is how long a VM must have been idle before it's swapped for another pool's.
	SwapIdle = 2 * time.Minute
	// SwapCooldown is how long a worker waits between swaps, so two pools with alternating
	// demand don't flap a worker's VMs back and forth.
	SwapCooldown = 15 * time.Minute
)

// JobLoad is what the Reconciler knows about jobs and VM activity, to size elastic pools. Build it
// with a LoadTracker. With no JobLoad (Options.Load nil) elastic pools keep their current size,
// clamped to their bounds.
type JobLoad struct {
	// Waiting counts, per pool, the jobs waiting unplaced in it.
	Waiting map[string]int
	// Busy has an entry for each VM whose Nomad node has joined: true while it runs a job.
	Busy map[string]bool
	// IdleSince is when each joined, idle VM was first seen idle.
	IdleSince map[string]time.Time
	// LastSwap is when each worker last had a VM swapped for another pool's.
	LastSwap map[string]time.Time
}

// LoadTracker keeps what JobLoad needs across reconciler ticks, in process memory only: after a
// restart, every VM's idle time starts over, which only delays a scale-down.
type LoadTracker struct {
	idleSince map[string]time.Time
	lastSwap  map[string]time.Time
}

// Observe records this tick's job and VM activity and returns the JobLoad to plan with.
func (t *LoadTracker) Observe(now time.Time, waiting map[string]int, busy map[string]bool) *JobLoad {
	idle := make(map[string]time.Time, len(busy))
	for vm, b := range busy {
		if b {
			continue
		}
		since, ok := t.idleSince[vm]
		if !ok {
			since = now
		}
		idle[vm] = since
	}
	t.idleSince = idle
	if t.lastSwap == nil {
		t.lastSwap = map[string]time.Time{}
	}
	return &JobLoad{Waiting: waiting, Busy: busy, IdleSince: idle, LastSwap: t.lastSwap}
}

// Applied records the swaps of a plan that was applied, for SwapCooldown.
func (t *LoadTracker) Applied(plan Plan, now time.Time) {
	for _, d := range plan.Deletes {
		if d.Swap {
			if t.lastSwap == nil {
				t.lastSwap = map[string]time.Time{}
			}
			t.lastSwap[d.Worker] = now
		}
	}
}

// scaling is the elastic pools' sizing decision for one Plan.
type scaling struct {
	// indexes holds the VM indexes (see VMName) each elastic pool should run, per worker.
	indexes map[string]map[string][]int
	// createReasons and deleteReasons explain the VMs scaling adds or removes, by VM name.
	createReasons map[string]string
	deleteReasons map[string]string
	swaps         map[string]bool
}

func (s *scaling) poolIndexes(pool, worker string) []int {
	return s.indexes[pool][worker]
}

// scale decides how many VMs each elastic pool runs on each worker: the current count clamped to
// the pool's bounds, then at most one VM more for a pool with waiting jobs (on the roomiest
// worker that fits it, or, when none does, in place of an idle VM of another elastic pool, at
// most one swap per tick), and at most one VM less per pool idle past IdleTimeout.
func (r *Reconciler) scale(workers []orchard.Worker, vms []orchard.VM) *scaling {
	now := r.Options.now()
	load := r.Options.Load
	s := &scaling{
		indexes:       map[string]map[string][]int{},
		createReasons: map[string]string{},
		deleteReasons: map[string]string{},
		swaps:         map[string]bool{},
	}
	pools := map[string]Pool{}
	for _, p := range r.Spec.Pools {
		pools[p.Name] = p
	}

	// Start from what runs now.
	byName := map[string]orchard.VM{}
	for _, vm := range vms {
		byName[vm.Name] = vm
	}
	elastic := []Pool{}
	for _, p := range r.Spec.Pools {
		if !p.Elastic() || isolationProblem(p) != "" {
			continue
		}
		elastic = append(elastic, p)
		min, max := p.Bounds()
		s.indexes[p.Name] = map[string][]int{}
		for _, w := range workers {
			if !eligible(p, w) {
				continue
			}
			var idx []int
			for _, vm := range vms {
				pool, worker, n, ok := ParseVMName(vm.Name)
				if ok && pool == p.Name && worker == w.Name && vm.Labels[LabelWorkerPin] == worker {
					idx = append(idx, n)
				}
			}
			sort.Ints(idx)
			if len(idx) > max {
				idx = idx[:max]
			}
			for len(idx) < min {
				idx = addIndex(idx)
			}
			s.indexes[p.Name][w.Name] = idx
		}
	}
	if load == nil {
		return s
	}

	swapped := false
	for _, p := range elastic {
		waiting := load.Waiting[p.Name]
		if waiting > 0 {
			if s.booting(p, byName, load, now) {
				continue
			}
			if w, ok := s.roomiest(p, workers, pools, byName); ok {
				s.add(p, w, fmt.Sprintf("scale up: %d job(s) waiting", waiting))
				continue
			}
			if !swapped {
				swapped = s.swap(p, waiting, workers, pools, load, now)
			}
			continue
		}
		s.scaleDown(p, load, now)
	}
	return s
}

func eligible(p Pool, w orchard.Worker) bool {
	return !w.Offline && !w.SchedulingPaused && labelsContain(w.Labels, p.WorkerSelector)
}

// addIndex appends the lowest index not yet in idx (sorted).
func addIndex(idx []int) []int {
	n := 0
	for _, i := range idx {
		if i == n {
			n++
		}
	}
	idx = append(idx, n)
	sort.Ints(idx)
	return idx
}

func (s *scaling) add(p Pool, worker, reason string) {
	before := s.indexes[p.Name][worker]
	after := addIndex(append([]int(nil), before...))
	for _, n := range after {
		if !containsInt(before, n) {
			s.createReasons[VMName(p.Name, worker, n)] = reason
		}
	}
	s.indexes[p.Name][worker] = after
}

func (s *scaling) remove(pool, worker string, n int, reason string) {
	idx := s.indexes[pool][worker]
	out := idx[:0:0]
	for _, i := range idx {
		if i != n {
			out = append(out, i)
		}
	}
	s.indexes[pool][worker] = out
	s.deleteReasons[VMName(pool, worker, n)] = reason
}

// booting reports whether p already has a VM on its way up: planned this tick, or created less
// than BootGrace ago and not yet joined to Nomad.
func (s *scaling) booting(p Pool, byName map[string]orchard.VM, load *JobLoad, now time.Time) bool {
	for worker, idx := range s.indexes[p.Name] {
		for _, n := range idx {
			name := VMName(p.Name, worker, n)
			if _, planned := s.createReasons[name]; planned {
				return true
			}
			vm, ok := byName[name]
			if !ok {
				return true // about to be created to reach the minimum
			}
			if _, joined := load.Busy[name]; vm.Status == "pending" || (!joined && vm.Status != "failed" && now.Sub(vm.CreatedAt) < BootGrace) {
				return true
			}
		}
	}
	return false
}

// roomiest picks the eligible worker below p's maximum with the most free memory that fits one
// more of p's VMs.
func (s *scaling) roomiest(p Pool, workers []orchard.Worker, pools map[string]Pool, byName map[string]orchard.VM) (string, bool) {
	_, max := p.Bounds()
	best, bestFree := "", int64(-1)
	for _, w := range workers {
		if !eligible(p, w) || len(s.indexes[p.Name][w.Name]) >= max {
			continue
		}
		free, ok := s.fits(p, w, pools, byName)
		if ok && free > bestFree {
			best, bestFree = w.Name, free
		}
	}
	return best, best != ""
}

// macOSVMsPerHost is Apple's limit on macOS guests running at once on one Mac.
const macOSVMsPerHost = 2

// fits reports whether one more of p's VMs fits on w next to every VM w runs or is planned to
// run, and how much memory (MiB) would be left. A worker that doesn't report a resource isn't
// limited by it.
func (s *scaling) fits(p Pool, w orchard.Worker, pools map[string]Pool, byName map[string]orchard.VM) (int64, bool) {
	vms, cpu, mem, macs := 0, uint64(0), uint64(0), 0
	count := func(pool Pool, cpuUsed, memUsed uint64) {
		vms++
		cpu += cpuUsed
		mem += memUsed
		if pool.MacOS() {
			macs++
		}
	}
	// Planned VMs of elastic pools.
	planned := map[string]bool{}
	for pool, byWorker := range s.indexes {
		for _, n := range byWorker[w.Name] {
			name := VMName(pool, w.Name, n)
			planned[name] = true
			count(pools[pool], pools[pool].CPU, pools[pool].Memory)
		}
	}
	// Everything else on the worker, including VMs still being deleted.
	for name, vm := range byName {
		if planned[name] || hostOf(vm, vm.Labels[LabelWorkerPin]) != w.Name {
			continue
		}
		pool, _, _, _ := ParseVMName(name)
		count(pools[pool], vm.CPU, vm.Memory)
	}

	if slots := w.Resources["org.cirruslabs.tart-vms"]; slots > 0 && uint64(vms+1) > slots {
		return 0, false
	}
	if p.MacOS() && macs+1 > macOSVMsPerHost {
		return 0, false
	}
	if cores := w.Resources["logical-cores"]; cores > 0 && cpu+p.CPU > cores {
		return 0, false
	}
	total := w.Resources["memory-mib"]
	if total == 0 {
		return 1 << 40, true
	}
	if mem+p.Memory > total {
		return 0, false
	}
	return int64(total - mem - p.Memory), true
}

// swap frees room for one of p's VMs by removing an idle VM of another elastic pool that has no
// waiting jobs and stays at or above its minimum, on a worker past its SwapCooldown. p's VM is
// created on a later tick, once the removed VM is gone.
func (s *scaling) swap(p Pool, waiting int, workers []orchard.Worker, pools map[string]Pool, load *JobLoad, now time.Time) bool {
	_, max := p.Bounds()
	type victim struct {
		pool, worker string
		n            int
		idle         time.Duration
	}
	var best *victim
	for _, w := range workers {
		if !eligible(p, w) || len(s.indexes[p.Name][w.Name]) >= max {
			continue
		}
		if last, ok := load.LastSwap[w.Name]; ok && now.Sub(last) < SwapCooldown {
			continue
		}
		for qName, byWorker := range s.indexes {
			q := pools[qName]
			if qName == p.Name || load.Waiting[qName] > 0 {
				continue
			}
			qMin, _ := q.Bounds()
			idx := byWorker[w.Name]
			if len(idx) <= qMin {
				continue
			}
			for _, n := range idx {
				name := VMName(qName, w.Name, n)
				busy, joined := load.Busy[name]
				since, ok := load.IdleSince[name]
				if !joined || busy || !ok || now.Sub(since) < SwapIdle {
					continue
				}
				if best == nil || now.Sub(since) > best.idle {
					best = &victim{pool: qName, worker: w.Name, n: n, idle: now.Sub(since)}
				}
			}
		}
	}
	if best == nil {
		return false
	}
	s.remove(best.pool, best.worker, best.n, fmt.Sprintf("swap for %s: %d job(s) waiting", p.Name, waiting))
	s.swaps[VMName(best.pool, best.worker, best.n)] = true
	return true
}

// scaleDown removes p's VM that has been idle longest, if past IdleTimeout, from a worker above
// p's minimum.
func (s *scaling) scaleDown(p Pool, load *JobLoad, now time.Time) {
	min, _ := p.Bounds()
	var worker string
	var n int
	var longest time.Duration
	for w, idx := range s.indexes[p.Name] {
		if len(idx) <= min {
			continue
		}
		for _, i := range idx {
			name := VMName(p.Name, w, i)
			busy, joined := load.Busy[name]
			since, ok := load.IdleSince[name]
			if !joined || busy || !ok {
				continue
			}
			if idle := now.Sub(since); idle >= IdleTimeout && idle > longest {
				worker, n, longest = w, i, idle
			}
		}
	}
	if worker != "" {
		s.remove(p.Name, worker, n, "scale down: idle for "+strings.TrimSuffix(longest.Truncate(time.Minute).String(), "0s"))
	}
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
