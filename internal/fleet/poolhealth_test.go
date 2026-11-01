package fleet

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/snooze"
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

type pausesStub map[string]snooze.Entry

func (p pausesStub) Get(w string) snooze.Entry { return p[w] }

func TestPoolHealth_WaitReason(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	until := now.Add(2 * time.Hour)
	stub := pausesStub{
		"g-macbook-m5.local": {Snooze: &snooze.Snooze{Until: until, Mode: snooze.ModeDrain}},
		"opmac":              {Operator: &snooze.Operator{By: "me"}},
	}
	h := &PoolHealth{Pauses: stub}

	// Only worker is snoozed: exclusive wait.
	h.ObserveFleet(nil, Plan{
		Paused:     []PlannedPause{{Pool: "macos", Workers: []string{"g-macbook-m5.local"}}},
		PausedOnly: []PlannedPause{{Pool: "macos", Workers: []string{"g-macbook-m5.local"}}},
	})
	reason, wu, excl := h.WaitReason("macos", now)
	if !excl || !wu.Equal(until) || !strings.Contains(reason, "queued: workers for pool macos snoozed (g-macbook-m5.local snoozed until ") {
		t.Fatalf("wait = %q %v %v", reason, wu, excl)
	}
	// Exclusive and snoozed: the unplaceable path must be skipped by the caller, but the health
	// reason still reads as paused.
	if r := h.UnplaceableReason("macos"); !strings.Contains(r, "every worker for pool macos is paused") {
		t.Fatalf("reason = %q", r)
	}

	// Other Macs exist (busy): not exclusive, worded as waiting for a free worker, not the laptop.
	h.ObserveFleet(nil, Plan{Paused: []PlannedPause{{Pool: "macos", Workers: []string{"g-macbook-m5.local"}}}})
	reason, wu, excl = h.WaitReason("macos", now)
	if excl || !wu.Equal(until) || !strings.HasPrefix(reason, "waiting for a free macos worker (g-macbook-m5.local snoozed until ") {
		t.Fatalf("wait = %q %v %v", reason, wu, excl)
	}
	if r := h.UnplaceableReason("macos"); r != "" {
		t.Fatalf("reason = %q", r)
	}

	// Expired snooze, operator-only pause: no wait, clearer fail-fast message.
	h.ObserveFleet(nil, Plan{
		Paused:     []PlannedPause{{Pool: "mac2", Workers: []string{"opmac"}}},
		PausedOnly: []PlannedPause{{Pool: "mac2", Workers: []string{"opmac"}}},
	})
	if r, _, _ := h.WaitReason("mac2", now); r != "" {
		t.Fatalf("operator pause must not wait: %q", r)
	}
	if r := h.UnplaceableReason("mac2"); !strings.Contains(r, "opmac (operator pause)") || !strings.Contains(r, "grove worker resume") {
		t.Fatalf("reason = %q", r)
	}
	if r, _, _ := h.WaitReason("macos", until.Add(time.Minute)); r != "" {
		t.Fatalf("expired snooze waits: %q", r)
	}
	var none *PoolHealth
	if r, _, _ := none.WaitReason("x", now); r != "" {
		t.Fatal("nil PoolHealth waits")
	}
}
