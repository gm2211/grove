package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

func TestHandleFleet_Normalizes(t *testing.T) {
	oc := &fakeOrchard{
		workers: []orchard.Worker{
			{
				Name:             "mac1",
				Arch:             "arm64",
				Offline:          false,
				SchedulingPaused: false,
				Resources:        map[string]uint64{"logical-cores": 8, "memory-mib": 16384, "org.cirruslabs.tart-vms": 2},
				Labels:           map[string]string{"role": "worker"},
			},
		},
		vms: []orchard.VM{
			{Name: "linux-mac1-0", Worker: "mac1", Status: "running", CPU: 4, Memory: 8192, Labels: map[string]string{"org.cirruslabs.orchard.worker-name": "mac1"}},
		},
	}
	nc := &fakeNomad{
		nodes: []nomad.Node{
			{ID: "node-1", Name: "linux-mac1-0", Status: "ready", Meta: map[string]string{"pool": "linux", "vm": "linux-mac1-0", "host": "mac1"}, MemoryMiB: 8192, RunningAllocs: 1},
		},
	}
	srv := newTestServer(oc, nc, nil, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp FleetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Workers) != 1 || resp.Workers[0].Name != "mac1" || resp.Workers[0].Kind != "worker" {
		t.Fatalf("workers = %+v", resp.Workers)
	}
	if c := resp.Workers[0].Capacity; c.CPU != 8 || c.MemoryMiB != 16384 || c.VMSlots != 2 {
		t.Errorf("worker capacity = %+v", c)
	}
	if resp.Workers[0].Running != 1 {
		t.Errorf("worker running = %d, want 1 (one running vm on mac1)", resp.Workers[0].Running)
	}
	if !resp.Workers[0].Online {
		t.Errorf("worker online = false, want true")
	}

	if len(resp.VMs) != 1 || resp.VMs[0].Kind != "vm" || !resp.VMs[0].Online {
		t.Fatalf("vms = %+v", resp.VMs)
	}
	if resp.VMs[0].Host != "mac1" {
		t.Errorf("vm host = %q, want mac1", resp.VMs[0].Host)
	}
	if resp.VMs[0].Pool != "linux" {
		t.Errorf("vm pool = %q, want linux (derived from the VM name, not a label)", resp.VMs[0].Pool)
	}

	if len(resp.Nodes) != 1 || resp.Nodes[0].Kind != "node" || !resp.Nodes[0].Online {
		t.Fatalf("nodes = %+v", resp.Nodes)
	}
	if resp.Nodes[0].Running != 1 {
		t.Errorf("node running = %d, want 1", resp.Nodes[0].Running)
	}
}

func TestHandleFleet_Totals(t *testing.T) {
	oc := &fakeOrchard{
		workers: []orchard.Worker{
			{Name: "mac1", Offline: false, SchedulingPaused: false},
			{Name: "mac2", Offline: true, SchedulingPaused: false},
			{Name: "mac3", Offline: false, SchedulingPaused: true},
		},
		vms: []orchard.VM{
			{Name: "linux-mac1-0", Worker: "mac1", Status: "running"},
			{Name: "linux-mac1-1", Worker: "mac1", Status: "stopped"},
		},
	}
	nc := &fakeNomad{
		nodes: []nomad.Node{
			{ID: "node-1", Name: "n1", Status: "ready", Drain: false},
			{ID: "node-2", Name: "n2", Status: "ready", Drain: true},
			{ID: "node-3", Name: "n3", Status: "down", Drain: false},
		},
	}
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{
		"j1": {ID: "j1", Status: dispatch.StatusRunning},
		"j2": {ID: "j2", Status: dispatch.StatusRunning},
		"j3": {ID: "j3", Status: dispatch.StatusPending},
		"j4": {ID: "j4", Status: dispatch.StatusSuccess},
	}}
	srv := newTestServer(oc, nc, ds, Options{})

	before := time.Now().UTC()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	after := time.Now().UTC()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp FleetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.FetchedAt.IsZero() || resp.FetchedAt.Before(before) || resp.FetchedAt.After(after) {
		t.Errorf("FetchedAt = %v, want between %v and %v", resp.FetchedAt, before, after)
	}

	want := FleetTotals{
		WorkersOnline:   2,
		WorkersCordoned: 1,
		VMsRunning:      1,
		NodesReady:      2,
		NodesDraining:   1,
		JobsRunning:     2,
		JobsPending:     1,
	}
	if resp.Totals != want {
		t.Errorf("Totals = %+v, want %+v", resp.Totals, want)
	}
}

func TestHandleFleet_UpstreamErrorBecomesBadGateway(t *testing.T) {
	oc := &fakeOrchard{listErr: errors.New("boom")}
	srv := newTestServer(oc, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}
