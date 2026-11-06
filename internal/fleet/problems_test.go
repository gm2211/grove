package fleet

import (
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

func TestProblems_NamesEachStuckVMOnceWithItsReason(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	vms := []orchard.VM{
		{Name: "linux-studio-0", Worker: "studio", Status: "running", StartedAt: now.Add(-time.Hour)},
		{Name: "linux-studio-1", Worker: "studio", Status: "running", StartedAt: now.Add(-time.Minute)},
		{Name: "linux-mini-0", Worker: "mini", Status: "running", StartedAt: now.Add(-time.Hour)},
		{Name: "macos-mbp-0", Worker: "mbp", Status: "running", RestartCount: 10, StatusMessage: "VM started, running startup script..."},
		{Name: "macos-mbp-1", Worker: "mbp", Status: "failed", RestartCount: 4, StatusMessage: "VZErrorDomain -9: Failed to get current host key"},
		{Name: "linux-mbp-0", Worker: "mbp", Status: "failed", StatusMessage: "disk full"},
	}
	nodes := []nomad.Node{
		{Name: "linux-studio-0", Status: "ready"},
		{Name: "linux-mini-0", Status: "down"},
	}
	got := Problems(vms, nodes, now)
	want := []struct{ kind, vm string }{
		{ProblemNeedsUnlock, "macos-mbp-1"},
		{ProblemBootLoop, "macos-mbp-0"},
		{ProblemFailed, "linux-mbp-0"},
		{ProblemNotJoined, "linux-mini-0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].VM != w.vm {
			t.Errorf("problem %d = %s %s, want %s %s", i, got[i].Kind, got[i].VM, w.kind, w.vm)
		}
		if got[i].Detail == "" || got[i].Worker == "" {
			t.Errorf("problem %d has no detail or worker: %+v", i, got[i])
		}
	}
}
