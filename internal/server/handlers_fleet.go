package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

// Capacity is a normalised resource capacity for a FleetEntry.
type Capacity struct {
	CPU       uint64 `json:"cpu,omitempty"`
	MemoryMiB uint64 `json:"memoryMiB,omitempty"`
	VMSlots   int    `json:"vmSlots,omitempty"`
}

// FleetEntry is one normalised row of GET /fleet, whichever of the three sources it came from.
type FleetEntry struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"` // worker | vm | node
	Host     string            `json:"host,omitempty"`
	Arch     string            `json:"arch,omitempty"`
	Online   bool              `json:"online"`
	Cordoned bool              `json:"cordoned"`
	Status   string            `json:"status,omitempty"`
	Capacity Capacity          `json:"capacity"`
	Running  int               `json:"running"`
	Labels   map[string]string `json:"labels,omitempty"`
	Raw      any               `json:"raw"`
}

// FleetResponse is the body of GET /fleet.
type FleetResponse struct {
	Workers   []FleetEntry `json:"workers"`
	VMs       []FleetEntry `json:"vms"`
	Nodes     []FleetEntry `json:"nodes"`
	FetchedAt time.Time    `json:"fetchedAt"`
	Totals    FleetTotals  `json:"totals"`
}

// FleetTotals is a set of at-a-glance counts derived from the same fleet snapshot as
// Workers/VMs/Nodes, so a UI/caller doesn't have to recompute them client-side.
type FleetTotals struct {
	WorkersOnline   int `json:"workersOnline"`
	WorkersCordoned int `json:"workersCordoned"`
	VMsRunning      int `json:"vmsRunning"`
	NodesReady      int `json:"nodesReady"`
	NodesDraining   int `json:"nodesDraining"`
	JobsRunning     int `json:"jobsRunning"`
	JobsPending     int `json:"jobsPending"`
}

func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var (
		workers          []orchard.Worker
		vms              []orchard.VM
		nodes            []nomad.Node
		jobs             []dispatch.Job
		wErr, vErr, nErr error
		jErr             error
	)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); workers, wErr = s.orchard.ListWorkers(ctx) }()
	go func() { defer wg.Done(); vms, vErr = s.orchard.ListVMs(ctx) }()
	go func() { defer wg.Done(); nodes, nErr = s.nomad.ListNodes(ctx) }()
	go func() { defer wg.Done(); jobs, jErr = s.dispatch.List(ctx) }()
	wg.Wait()

	if wErr != nil {
		s.writeError(w, http.StatusBadGateway, wErr)
		return
	}
	if vErr != nil {
		s.writeError(w, http.StatusBadGateway, vErr)
		return
	}
	if nErr != nil {
		s.writeError(w, http.StatusBadGateway, nErr)
		return
	}
	if jErr != nil {
		// Jobs are supplementary to /fleet's core payload (workers/vms/nodes) — don't fail the
		// whole response over it, just leave the jobs-derived totals at zero.
		s.log.Warn("fleet: listing jobs for totals failed", "err", jErr)
		jobs = nil
	}

	runningByWorker := map[string]int{}
	for _, v := range vms {
		if v.Status == "running" {
			runningByWorker[v.Worker]++
		}
	}

	resp := FleetResponse{
		Workers: make([]FleetEntry, 0, len(workers)),
		VMs:     make([]FleetEntry, 0, len(vms)),
		Nodes:   make([]FleetEntry, 0, len(nodes)),
	}
	for _, wrk := range workers {
		resp.Workers = append(resp.Workers, normalizeWorker(wrk, runningByWorker[wrk.Name]))
	}
	for _, v := range vms {
		resp.VMs = append(resp.VMs, normalizeVM(v))
	}
	for _, n := range nodes {
		resp.Nodes = append(resp.Nodes, normalizeNode(n))
	}

	resp.FetchedAt = time.Now().UTC()
	for _, wrk := range workers {
		if !wrk.Offline {
			resp.Totals.WorkersOnline++
		}
		if wrk.SchedulingPaused {
			resp.Totals.WorkersCordoned++
		}
	}
	for _, v := range vms {
		if v.Status == "running" {
			resp.Totals.VMsRunning++
		}
	}
	for _, n := range nodes {
		if n.Status == "ready" {
			resp.Totals.NodesReady++
		}
		if n.Drain {
			resp.Totals.NodesDraining++
		}
	}
	for _, j := range jobs {
		switch j.Status {
		case dispatch.StatusRunning:
			resp.Totals.JobsRunning++
		case dispatch.StatusPending:
			resp.Totals.JobsPending++
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

func normalizeWorker(wrk orchard.Worker, running int) FleetEntry {
	status := "online"
	switch {
	case wrk.Offline:
		status = "offline"
	case wrk.SchedulingPaused:
		status = "paused"
	}
	return FleetEntry{
		Name:     wrk.Name,
		Kind:     "worker",
		Host:     wrk.Name,
		Arch:     wrk.Arch,
		Online:   !wrk.Offline,
		Cordoned: wrk.SchedulingPaused,
		Status:   status,
		Capacity: Capacity{
			CPU:       wrk.Resources["logical-cores"],
			MemoryMiB: wrk.Resources["memory-mib"],
			VMSlots:   int(wrk.Resources["org.cirruslabs.tart-vms"]),
		},
		Running: running,
		Labels:  wrk.Labels,
		Raw:     wrk,
	}
}

func normalizeVM(v orchard.VM) FleetEntry {
	running := 0
	if v.Status == "running" {
		running = 1
	}
	return FleetEntry{
		Name:   v.Name,
		Kind:   "vm",
		Host:   v.Worker,
		Online: v.Status == "running",
		Status: v.Status,
		Capacity: Capacity{
			CPU:       v.CPU,
			MemoryMiB: v.Memory,
		},
		Running: running,
		Labels:  v.Labels,
		Raw:     v,
	}
}

func normalizeNode(n nomad.Node) FleetEntry {
	return FleetEntry{
		Name:     n.Name,
		Kind:     "node",
		Host:     n.Meta["host"],
		Arch:     n.Attributes["cpu.arch"],
		Online:   n.Status == "ready",
		Cordoned: n.Drain || n.Eligibility == "ineligible",
		Status:   n.Status,
		Capacity: Capacity{
			MemoryMiB: uint64(n.MemoryMiB),
		},
		Running: n.RunningAllocs,
		Labels:  n.Meta,
		Raw:     n,
	}
}
