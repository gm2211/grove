package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/orchard"
)

// Capacity is a normalised resource capacity for a FleetEntry.
type Capacity struct {
	CPU       uint64 `json:"cpu,omitempty"`
	MemoryMiB uint64 `json:"memoryMiB,omitempty"`
	VMSlots   int    `json:"vmSlots,omitempty"`
}

// FleetEntry is one normalised row of GET /fleet, a worker or a VM.
type FleetEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // worker | vm
	Host string `json:"host,omitempty"`
	// Pool is set for vm entries: which fleet.yaml pool the VM belongs to, derived from its name
	// (orchard.VM no longer carries a usable "pool" label — see internal/fleet.PoolAndHost and
	// fleet.Pool.Labels' doc comment for why).
	Pool     string            `json:"pool,omitempty"`
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
	Workers []FleetEntry `json:"workers"`
	VMs     []FleetEntry `json:"vms"`
	// Deprecated: Nodes listed Nomad nodes. grove no longer uses Nomad; it is always empty, and
	// kept so clients reading it don't break.
	Nodes     []FleetEntry          `json:"nodes"`
	FetchedAt time.Time             `json:"fetchedAt"`
	Totals    FleetTotals           `json:"totals"`
	Reconcile FleetReconcileSummary `json:"reconcile"`
}

// FleetTotals is a set of at-a-glance counts derived from the same fleet snapshot as
// Workers/VMs/Nodes, so a UI/caller doesn't have to recompute them client-side.
type FleetTotals struct {
	WorkersOnline   int `json:"workersOnline"`
	WorkersCordoned int `json:"workersCordoned"`
	VMsRunning      int `json:"vmsRunning"`
	// Deprecated: NodesReady and NodesDraining counted Nomad nodes; always 0.
	NodesReady    int `json:"nodesReady"`
	NodesDraining int `json:"nodesDraining"`
	JobsRunning   int `json:"jobsRunning"`
	JobsPending   int `json:"jobsPending"`
}

func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var (
		workers    []orchard.Worker
		vms        []orchard.VM
		jobs       []dispatch.Job
		wErr, vErr error
		jErr       error
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); workers, wErr = s.orchard.ListWorkers(ctx) }()
	go func() { defer wg.Done(); vms, vErr = s.orchard.ListVMs(ctx) }()
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
	if jErr != nil {
		// Jobs are supplementary to /fleet's core payload (workers/vms) — don't fail the
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
		Nodes:   []FleetEntry{},
	}
	for _, wrk := range workers {
		resp.Workers = append(resp.Workers, normalizeWorker(wrk, runningByWorker[wrk.Name]))
	}
	for _, v := range vms {
		resp.VMs = append(resp.VMs, normalizeVM(v))
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
	for _, j := range jobs {
		switch j.Status {
		case dispatch.StatusRunning:
			resp.Totals.JobsRunning++
		case dispatch.StatusPending:
			resp.Totals.JobsPending++
		}
	}

	resp.Reconcile = s.fleetReconcile.summary()

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

	pool, host := fleet.PoolAndHost(v)

	// The guest startup script embeds a pool's own setup commands, which may hold secrets, and
	// GET /fleet is read scope. Nothing outside the reconciler (which
	// reads orchard.VM directly, never this API) needs either script, so drop both from Raw for
	// every caller, operator included.
	v.StartupScript = ""
	v.ShutdownScript = ""

	return FleetEntry{
		Name:   v.Name,
		Kind:   "vm",
		Host:   host,
		Pool:   pool,
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
