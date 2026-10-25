// Package health watches where sealed pools' jobs get lost and acts on machines that keep losing
// them: a VM that loses jobs again and again is rebuilt, and a Mac whose VMs keep losing them stops
// getting work until an operator resumes it (`grove worker resume`).
//
// It reads the attempt records the job drivers keep (dispatch.Job.Attempts). A job grove
// quarantined is left out: it takes machines down, so its losses say nothing about them.
package health

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/orchard"
)

// Defaults for Monitor's thresholds.
const (
	// DefaultWindow is how far back losses count.
	DefaultWindow = time.Hour
	// DefaultVMLosses is how many different jobs one VM may lose, with no job finishing in it in
	// between, before it is rebuilt.
	DefaultVMLosses = 2
	// DefaultMachineLosses is how many different jobs one machine's VMs may lose, in at least two of
	// its VMs and with no job finishing on it in between, before the machine stops getting work.
	DefaultMachineLosses = 3
)

// Monitor applies the rules on every Tick. Its memory of what it already did lives only in the
// process: after a restart it may rebuild a VM or pause a machine once more for the same losses.
type Monitor struct {
	Jobs    dispatch.Service
	Orchard orchard.Client
	Log     *slog.Logger
	Now     func() time.Time

	Window        time.Duration
	VMLosses      int
	MachineLosses int

	// rebuilt holds the VMs (name/UID) already deleted for rebuilding.
	rebuilt map[string]bool
	// paused holds when the monitor paused each machine; only losses after that count again.
	paused map[string]time.Time
}

// Run ticks every interval until ctx is done.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		m.Tick(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// event is one finished attempt.
type event struct {
	at      time.Time
	job     string
	vm      string // name/UID: a rebuilt VM keeps its name but is a new VM
	vmName  string
	vmUID   string
	machine string
	lost    bool
}

// Tick looks at the recent attempts and acts on what they show.
func (m *Monitor) Tick(ctx context.Context) {
	m.defaults()
	jobs, err := m.Jobs.List(ctx)
	if err != nil {
		m.Log.Warn("health: could not list jobs", "err", err)
		return
	}
	events := recentEvents(jobs, m.Now().Add(-m.Window))

	for vm, losses := range lossesBy(events, func(e event) string { return e.vm }) {
		if len(losses.jobs) < m.VMLosses || m.rebuilt[vm] {
			continue
		}
		m.rebuilt[vm] = true
		// The VM may already have been replaced under the same name; the new one is innocent.
		if current, err := m.Orchard.GetVM(ctx, losses.vmName); err != nil || current == nil || current.UID != losses.vmUID {
			continue
		}
		m.Log.Warn("health: rebuilding a VM that keeps losing jobs", "vm", losses.vmName, "jobs", sortedKeys(losses.jobs))
		if err := m.Orchard.DeleteVM(ctx, losses.vmName); err != nil {
			m.Log.Warn("health: could not delete the VM; it will be retried next time", "vm", losses.vmName, "err", err)
			delete(m.rebuilt, vm)
		}
	}

	byMachine := lossesBy(events, func(e event) string { return e.machine })
	if len(byMachine) == 0 {
		return
	}
	workers, err := m.Orchard.ListWorkers(ctx)
	if err != nil {
		m.Log.Warn("health: could not list machines", "err", err)
		return
	}
	alreadyPaused := map[string]bool{}
	for _, w := range workers {
		alreadyPaused[w.Name] = w.SchedulingPaused
	}
	for machine, losses := range byMachine {
		since := m.paused[machine]
		jobs, vms := losses.after(since)
		if machine == "" || alreadyPaused[machine] || jobs < m.MachineLosses || vms < 2 {
			continue
		}
		m.Log.Warn("health: stopping work on a machine whose VMs keep losing jobs; resume it with `grove worker resume`",
			"machine", machine, "jobs", sortedKeys(losses.jobs))
		if err := m.Orchard.PauseWorker(ctx, machine); err != nil {
			m.Log.Warn("health: could not pause the machine", "machine", machine, "err", err)
			continue
		}
		m.paused[machine] = m.Now()
	}
}

func (m *Monitor) defaults() {
	if m.Log == nil {
		m.Log = slog.Default()
	}
	if m.Now == nil {
		m.Now = time.Now
	}
	if m.Window <= 0 {
		m.Window = DefaultWindow
	}
	if m.VMLosses <= 0 {
		m.VMLosses = DefaultVMLosses
	}
	if m.MachineLosses <= 0 {
		m.MachineLosses = DefaultMachineLosses
	}
	if m.rebuilt == nil {
		m.rebuilt = map[string]bool{}
	}
	if m.paused == nil {
		m.paused = map[string]time.Time{}
	}
}

// recentEvents is every attempt that finished after since, oldest first, leaving out quarantined
// jobs.
func recentEvents(jobs []dispatch.Job, since time.Time) []event {
	var events []event
	for _, job := range jobs {
		if job.Quarantined {
			continue
		}
		for _, a := range job.Attempts {
			if a.FinishedAt == nil || a.FinishedAt.Before(since) {
				continue
			}
			switch a.Outcome {
			case dispatch.AttemptLost, dispatch.AttemptExited:
			default:
				continue // canceled or never set up: says nothing about the machine
			}
			events = append(events, event{
				at: *a.FinishedAt, job: job.ID, vm: a.VM + "/" + a.VMUID, vmName: a.VM, vmUID: a.VMUID,
				machine: a.Worker, lost: a.Outcome == dispatch.AttemptLost,
			})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	return events
}

// losses is what a VM or machine lost since a job last finished in it.
type losses struct {
	vmName string
	vmUID  string
	jobs   map[string]bool
	events []event
}

// after counts the different jobs lost after t, and in how many different VMs.
func (l losses) after(t time.Time) (jobs, vms int) {
	j, v := map[string]bool{}, map[string]bool{}
	for _, e := range l.events {
		if e.at.After(t) {
			j[e.job], v[e.vm] = true, true
		}
	}
	return len(j), len(v)
}

// lossesBy groups events by key, keeping only the losses after each group's last finished job.
func lossesBy(events []event, key func(event) string) map[string]losses {
	out := map[string]losses{}
	for _, e := range events {
		k := key(e)
		if !e.lost {
			delete(out, k) // a job finished there: the slate is clean
			continue
		}
		l, ok := out[k]
		if !ok {
			l = losses{vmName: e.vmName, vmUID: e.vmUID, jobs: map[string]bool{}}
		}
		l.jobs[e.job] = true
		l.events = append(l.events, e)
		out[k] = l
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
