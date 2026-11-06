package fleet

import (
	"fmt"
	"sort"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

// Problem kinds, as the API and the UI name them.
const (
	// ProblemNeedsUnlock: a macOS VM can't boot because its worker user's login keychain is locked
	// (NeedsLoginSession). Someone must log that user in, or unlock the keychain for the worker.
	ProblemNeedsUnlock = "needs-unlock"
	// ProblemBootLoop: Orchard keeps restarting the VM (BootLoopRestarts or more).
	ProblemBootLoop = "boot-loop"
	// ProblemFailed: the VM failed for another reason and waits for the reconciler to rebuild it.
	ProblemFailed = "failed"
	// ProblemNotJoined: the VM has run for NotJoinedAfter with no ready Nomad node of its name, so
	// it takes no job.
	ProblemNotJoined = "not-joined"
)

// NotJoinedAfter is how long a running VM may go without a ready Nomad node before it counts as
// stuck: a healthy one boots, enrols and registers well within it.
const NotJoinedAfter = 5 * time.Minute

// Problem is one VM that can't take jobs, and why, for an operator to see at a glance.
type Problem struct {
	Kind     string    `json:"kind"`
	VM       string    `json:"vm"`
	Pool     string    `json:"pool,omitempty"`
	Worker   string    `json:"worker,omitempty"`
	Restarts uint64    `json:"restarts,omitempty"`
	Since    time.Time `json:"since,omitempty"`
	Detail   string    `json:"detail"`
}

// Problems lists the VMs in vms that can't take jobs, most urgent kind first, then by VM name. A
// VM shows up once, under the first kind that applies: needs-unlock, boot-loop, failed, not-joined.
func Problems(vms []orchard.VM, nodes []nomad.Node, now time.Time) []Problem {
	ready := map[string]bool{}
	for _, n := range nodes {
		if n.Status == "ready" {
			ready[n.Name] = true
		}
	}
	var out []Problem
	for _, vm := range vms {
		pool, host := PoolAndHost(vm)
		worker := vm.Worker
		if worker == "" {
			worker = host
		}
		p := Problem{VM: vm.Name, Pool: pool, Worker: worker, Restarts: vm.RestartCount, Since: vm.CreatedAt}
		switch {
		case NeedsLoginSession(vm.StatusMessage):
			p.Kind = ProblemNeedsUnlock
			p.Detail = loginSessionHint(worker)
		case vm.RestartCount >= BootLoopRestarts:
			p.Kind = ProblemBootLoop
			p.Detail = fmt.Sprintf("restarted %d times; it never stays up long enough to join Nomad", vm.RestartCount)
			if vm.StatusMessage != "" {
				p.Detail += " (last: " + vm.StatusMessage + ")"
			}
		case vm.Status == "failed":
			p.Kind = ProblemFailed
			p.Detail = "failed"
			if vm.StatusMessage != "" {
				p.Detail += ": " + vm.StatusMessage
			}
		case vm.Status == "running" && !ready[vm.Name] && !vm.StartedAt.IsZero() && now.Sub(vm.StartedAt) >= NotJoinedAfter:
			p.Kind = ProblemNotJoined
			p.Since = vm.StartedAt
			p.Detail = fmt.Sprintf("running for %s with no ready Nomad node, so it takes no job", now.Sub(vm.StartedAt).Round(time.Minute))
		default:
			continue
		}
		out = append(out, p)
	}
	rank := map[string]int{ProblemNeedsUnlock: 0, ProblemBootLoop: 1, ProblemFailed: 2, ProblemNotJoined: 3}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return out[i].VM < out[j].VM
	})
	return out
}
