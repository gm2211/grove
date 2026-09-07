package mcp

import "github.com/gm2211/grove/internal/dispatch"

// FleetSummary is a compact, LLM-friendly rollup of the raw FleetResponse plus the running job
// count, meant to answer "what does the fleet look like right now" in one glance instead of
// requiring the caller to parse every worker/VM/node record.
type FleetSummary struct {
	Hosts       []string       `json:"hosts"`
	Workers     WorkerCounts   `json:"workers"`
	VMs         VMCounts       `json:"vms"`
	Nodes       NodeCounts     `json:"nodes"`
	RunningJobs int            `json:"running_jobs"`
	Raw         *FleetResponse `json:"raw"`
}

type WorkerCounts struct {
	Total    int `json:"total"`
	Online   int `json:"online"`
	Cordoned int `json:"cordoned"`
}

type VMCounts struct {
	Total    int            `json:"total"`
	ByPool   map[string]int `json:"by_pool"`
	ByStatus map[string]int `json:"by_status"`
}

type NodeCounts struct {
	Total int `json:"total"`
	Ready int `json:"ready"`
}

// summarizeFleet normalises a FleetResponse (plus, if available, the job list) into a
// FleetSummary. jobs may be nil if the /jobs call failed or was skipped; runningJobs then falls
// back to summing each node's RunningAllocs.
func summarizeFleet(fleet *FleetResponse, jobs []dispatch.Job) *FleetSummary {
	s := &FleetSummary{
		Hosts: make([]string, 0, len(fleet.Workers)),
		VMs: VMCounts{
			ByPool:   map[string]int{},
			ByStatus: map[string]int{},
		},
		Raw: fleet,
	}

	for _, w := range fleet.Workers {
		s.Hosts = append(s.Hosts, w.Name)
		s.Workers.Total++
		if !w.Offline {
			s.Workers.Online++
		}
		if w.SchedulingPaused {
			s.Workers.Cordoned++
		}
	}

	for _, vm := range fleet.VMs {
		s.VMs.Total++
		pool := vm.Labels["pool"]
		if pool == "" {
			pool = "(unlabelled)"
		}
		s.VMs.ByPool[pool]++
		status := vm.Status
		if status == "" {
			status = "(unknown)"
		}
		s.VMs.ByStatus[status]++
	}

	runningFromAllocs := 0
	for _, n := range fleet.Nodes {
		s.Nodes.Total++
		if n.Status == "ready" {
			s.Nodes.Ready++
		}
		runningFromAllocs += n.RunningAllocs
	}

	if jobs != nil {
		running := 0
		for _, j := range jobs {
			if j.Status == dispatch.StatusRunning {
				running++
			}
		}
		s.RunningJobs = running
	} else {
		s.RunningJobs = runningFromAllocs
	}

	return s
}
