package fleet

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

func TestPoolHealth_EnrolmentErrorNamesPoolAndClears(t *testing.T) {
	var h PoolHealth
	cause := errors.New("nomad: read node pool linux: permission denied 403")
	h.ObserveEnrol(errors.Join(&EnrolError{VM: "linux-studio-0", Pool: "linux", Err: cause}))

	got := h.UnplaceableReason("linux")
	if !strings.HasPrefix(got, "pool linux has no ready node: enrollment failing: enrol linux-studio-0:") || !strings.Contains(got, "403") {
		t.Errorf("reason = %q", got)
	}
	if h.UnplaceableReason("macos") != "" {
		t.Errorf("an error in pool linux must not condemn pool macos")
	}
	h.ObserveEnrol(nil)
	if r := h.UnplaceableReason("linux"); r != "" {
		t.Errorf("reason after a clean pass = %q, want none", r)
	}
}

func TestPoolHealth_GlobalEnrolmentErrorCoversEveryPool(t *testing.T) {
	var h PoolHealth
	h.ObserveEnrol(fmt.Errorf("list nodes: %w", errors.New("403")))
	if r := h.UnplaceableReason("anything"); !strings.Contains(r, "enrollment failing: list nodes") {
		t.Errorf("reason = %q", r)
	}
}

func TestPoolHealth_BootLoopOnlyWhenNoHealthyVM(t *testing.T) {
	var h PoolHealth
	looping := orchard.VM{Name: "linux-a-0", Status: "running", RestartCount: 9}
	h.ObserveFleet([]orchard.VM{looping}, Plan{})
	if r := h.UnplaceableReason("linux"); !strings.Contains(r, "pool linux VMs boot-looping") {
		t.Errorf("reason = %q", r)
	}
	h.ObserveFleet([]orchard.VM{looping, {Name: "linux-a-1", Status: "running"}}, Plan{})
	if r := h.UnplaceableReason("linux"); r != "" {
		t.Errorf("a healthy VM exists, reason = %q", r)
	}
}

func TestPoolHealth_NoMatchingWorkerAndBlocked(t *testing.T) {
	var h PoolHealth
	h.ObserveFleet(nil, Plan{Unmatched: []string{"linux"}, Blocked: []PlannedBlock{{Pool: "mac", Reason: "needs isolation"}}})
	if r := h.UnplaceableReason("linux"); r != "no worker matches pool linux" {
		t.Errorf("reason = %q", r)
	}
	if r := h.UnplaceableReason("mac"); !strings.Contains(r, "needs isolation") {
		t.Errorf("reason = %q", r)
	}
}

func TestBootLoops_GroupsByWorker(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	loops := BootLoops([]orchard.VM{
		{Name: "linux-a-0", Worker: "a", RestartCount: 9, StartedAt: t0},
		{Name: "linux-a-1", Worker: "a", RestartCount: 4, StartedAt: t0.Add(time.Hour)},
		{Name: "linux-b-0", Worker: "b", RestartCount: 1},
	})
	if len(loops) != 1 || loops[0].Worker != "a" || len(loops[0].VMs) != 2 || loops[0].MaxRestarts != 9 || !loops[0].Last.Equal(t0.Add(time.Hour)) {
		t.Fatalf("loops = %+v", loops)
	}
	if d := loops[0].Describe(); !strings.Contains(d, "2 VM(s) boot-looping") || !strings.Contains(d, "9 restarts") {
		t.Errorf("describe = %q", d)
	}
}
