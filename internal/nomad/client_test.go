package nomad

import (
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

func TestNodeFromAPI_MapsResourcesAndAllocs(t *testing.T) {
	n := &nomadapi.Node{
		ID:                    "node-1",
		Name:                  "mac1",
		Status:                "ready",
		SchedulingEligibility: "eligible",
		Drain:                 false,
		NodeClass:             "linux",
		Datacenter:            "dc1",
		Attributes:            map[string]string{"os.name": "linux"},
		Meta:                  map[string]string{"pool": "linux", "host": "mac1"},
		NodeResources: &nomadapi.NodeResources{
			Cpu:    nomadapi.NodeCpuResources{CpuShares: 3200},
			Memory: nomadapi.NodeMemoryResources{MemoryMB: 8192},
		},
	}

	out := nodeFromAPI(n, 3)

	if out.ID != "node-1" || out.Name != "mac1" || out.Status != "ready" {
		t.Fatalf("unexpected mapping: %+v", out)
	}

	if out.CPUMHz != 3200 || out.MemoryMiB != 8192 {
		t.Errorf("want cpuMHz/memoryMiB 3200/8192, got %d/%d", out.CPUMHz, out.MemoryMiB)
	}

	if out.RunningAllocs != 3 {
		t.Errorf("want 3 running allocs, got %d", out.RunningAllocs)
	}

	if out.Meta["pool"] != "linux" || out.Meta["host"] != "mac1" {
		t.Errorf("want meta pool/host preserved, got %+v", out.Meta)
	}
}

func TestNodeFromAPI_NilNodeResources(t *testing.T) {
	out := nodeFromAPI(&nomadapi.Node{ID: "node-1"}, 0)

	if out.CPUMHz != 0 || out.MemoryMiB != 0 {
		t.Errorf("want zero cpu/memory when NodeResources is nil, got %d/%d", out.CPUMHz, out.MemoryMiB)
	}
}

func TestRunningAllocCount(t *testing.T) {
	allocs := []*nomadapi.Allocation{
		{ClientStatus: nomadapi.AllocClientStatusRunning},
		{ClientStatus: nomadapi.AllocClientStatusRunning},
		{ClientStatus: nomadapi.AllocClientStatusComplete},
		{ClientStatus: nomadapi.AllocClientStatusFailed},
	}

	if got := runningAllocCount(allocs); got != 2 {
		t.Errorf("want 2 running allocs, got %d", got)
	}
}

func TestPickTask_Deterministic(t *testing.T) {
	states := map[string]*nomadapi.TaskState{
		"zzz": {State: "running"},
		"aaa": {State: "dead"},
	}

	name, ts := pickTask(states)
	if name != "aaa" || ts.State != "dead" {
		t.Errorf("want the lowest-sorting task name (aaa), got %q", name)
	}
}

func TestPickTask_Empty(t *testing.T) {
	name, ts := pickTask(nil)
	if name != "" || ts != nil {
		t.Errorf("want empty result for no task states, got (%q, %+v)", name, ts)
	}
}

func TestLastExitCode(t *testing.T) {
	ts := &nomadapi.TaskState{
		Events: []*nomadapi.TaskEvent{
			{Type: "Started"},
			{Type: "Terminated", ExitCode: 1},
			{Type: "Restarting"},
			{Type: "Terminated", ExitCode: 0},
		},
	}

	code, ok := lastExitCode(ts)
	if !ok || code != 0 {
		t.Errorf("want the most recent Terminated exit code (0), got (%d, %v)", code, ok)
	}
}

func TestLastExitCode_NoTerminatedEvent(t *testing.T) {
	ts := &nomadapi.TaskState{Events: []*nomadapi.TaskEvent{{Type: "Started"}}}

	if _, ok := lastExitCode(ts); ok {
		t.Error("want ok=false when there's no Terminated event")
	}
}

func TestFinishedAt(t *testing.T) {
	if got := finishedAt(&nomadapi.TaskState{}); got != nil {
		t.Errorf("want nil for a zero FinishedAt, got %v", got)
	}

	when := time.Now().Truncate(time.Second)

	got := finishedAt(&nomadapi.TaskState{FinishedAt: when})
	if got == nil || !got.Equal(when) {
		t.Errorf("want %v, got %v", when, got)
	}
}

func TestAllocFromStub(t *testing.T) {
	created := time.Now().Add(-time.Minute)

	stub := &nomadapi.AllocationListStub{
		ID:           "alloc-1",
		JobID:        "job-1",
		NodeID:       "node-1",
		NodeName:     "mac1",
		ClientStatus: "complete",
		CreateTime:   created.UnixNano(),
		TaskStates: map[string]*nomadapi.TaskState{
			"run": {
				Events: []*nomadapi.TaskEvent{{Type: "Terminated", ExitCode: 7}},
			},
		},
	}

	out := allocFromStub(stub)

	if out.ID != "alloc-1" || out.JobID != "job-1" || out.TaskName != "run" {
		t.Fatalf("unexpected mapping: %+v", out)
	}

	if out.ExitCode == nil || *out.ExitCode != 7 {
		t.Errorf("want exit code 7, got %+v", out.ExitCode)
	}

	if !out.CreatedAt.Equal(created.Truncate(time.Nanosecond)) {
		t.Errorf("want createdAt %v, got %v", created, out.CreatedAt)
	}
}

func TestJobNameOf(t *testing.T) {
	if got := jobNameOf(nil); got != "<unknown>" {
		t.Errorf("want <unknown> for nil job, got %q", got)
	}

	id := "build-123"
	if got := jobNameOf(&nomadapi.Job{ID: &id}); got != id {
		t.Errorf("want %q, got %q", id, got)
	}
}
