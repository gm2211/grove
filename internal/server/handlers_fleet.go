package server

import (
	"net/http"
	"sync"

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
	Workers []FleetEntry `json:"workers"`
	VMs     []FleetEntry `json:"vms"`
	Nodes   []FleetEntry `json:"nodes"`
}

func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var (
		workers          []orchard.Worker
		vms              []orchard.VM
		nodes            []nomad.Node
		wErr, vErr, nErr error
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); workers, wErr = s.orchard.ListWorkers(ctx) }()
	go func() { defer wg.Done(); vms, vErr = s.orchard.ListVMs(ctx) }()
	go func() { defer wg.Done(); nodes, nErr = s.nomad.ListNodes(ctx) }()
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
