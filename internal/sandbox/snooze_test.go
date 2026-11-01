package sandbox

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/snooze"
)

func writePauses(t *testing.T, h *harness, worker string, mode snooze.Mode, until time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pauses.json")
	st, err := snooze.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSnooze(worker, snooze.Snooze{Until: until, Mode: mode, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.runner.PausesPath = path
}

func TestRun_SnoozeNowEvictsToAnotherMachineWithoutCountingALoss(t *testing.T) {
	running := newFakeVM()
	running.runningInspects = 1 << 30 // never exits by itself
	good := newFakeVM()
	good.runningInspects = 1
	h := retryHarness(t, map[string]*fakeVM{"linux-mac1-0": running, "linux-mac2-0": good})
	h.runner.PollInterval = 5 * time.Millisecond
	writePauses(t, h, "mac1", snooze.ModeNow, time.Now().Add(time.Hour))

	if code := h.runner.Run(context.Background(), retryRequest()); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderrString())
	}
	if !good.created {
		t.Fatal("the evicted job didn't go to another machine")
	}
	rec := readRecord(t, h)
	if rec.Quarantined || len(rec.Attempts) != 2 {
		t.Fatalf("record = %+v", rec)
	}
	if first := rec.Attempts[0]; first.Worker != "mac1" || first.Outcome != dispatch.AttemptEvicted {
		t.Errorf("first attempt = %+v, want evicted on mac1", first)
	}
	if last := rec.Attempts[1]; last.Outcome != dispatch.AttemptExited {
		t.Errorf("second attempt = %+v", last)
	}
}

// Evictions are not losses: any number of them leaves MaxAttempts and quarantine untouched.
func TestAttemptLog_EvictedDoesNotCount(t *testing.T) {
	l := attemptLog{record: dispatch.AttemptRecord{}}
	for i := 0; i < 5; i++ {
		at := l.start(orchard.VM{Name: "linux-mac1-0", Worker: "mac1"})
		l.finish(at, exitEvicted, true, "snoozed", false)
	}
	if l.count() != 0 {
		t.Fatalf("count = %d, evicted attempts must not count", l.count())
	}
}

func TestRun_SnoozeDrainModeDoesNotEvict(t *testing.T) {
	vm := newFakeVM()
	vm.runningInspects = 2
	h := retryHarness(t, map[string]*fakeVM{"linux-mac1-0": vm})
	h.runner.PollInterval = 5 * time.Millisecond
	writePauses(t, h, "mac1", snooze.ModeDrain, time.Now().Add(time.Hour))
	if code := h.runner.Run(context.Background(), retryRequest()); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderrString())
	}
	if rec := readRecord(t, h); len(rec.Attempts) != 1 || rec.Attempts[0].Outcome != dispatch.AttemptExited {
		t.Fatalf("record = %+v", rec)
	}
}

func TestRun_AgentJobNotEvictedBySnoozeNow(t *testing.T) {
	vm := newFakeVM()
	vm.runningInspects = 2
	h := retryHarness(t, map[string]*fakeVM{"linux-mac1-0": vm})
	h.runner.PollInterval = 5 * time.Millisecond
	writePauses(t, h, "mac1", snooze.ModeNow, time.Now().Add(time.Hour))
	req := retryRequest()
	req.RetryLost = false // an agent job
	if code := h.runner.Run(context.Background(), req); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderrString())
	}
}

func TestAcquire_WaitsPastNoVMTimeoutWhileSnoozed(t *testing.T) {
	h := retryHarness(t, map[string]*fakeVM{})
	h.runner.SlotWait = 5 * time.Millisecond
	h.orchard.workers = []orchard.Worker{{Name: "mac1", SchedulingPaused: true}}
	writePauses(t, h, "mac1", snooze.ModeDrain, time.Now().Add(time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond) // 8x NoVMTimeout
	defer cancel()
	code := h.runner.Run(ctx, retryRequest())
	if code != ExitCanceled {
		t.Fatalf("Run = %d, want canceled (still waiting); stderr:\n%s", code, h.stderrString())
	}
	if !strings.Contains(h.stderrString(), "all machines snoozed until") {
		t.Errorf("stderr = %s", h.stderrString())
	}
}

func TestAcquire_PausedWithoutSnoozeStillTimesOut(t *testing.T) {
	h := retryHarness(t, map[string]*fakeVM{})
	h.runner.SlotWait = 5 * time.Millisecond
	h.orchard.workers = []orchard.Worker{{Name: "mac1", SchedulingPaused: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if code := h.runner.Run(ctx, retryRequest()); code != ExitLost {
		t.Fatalf("Run = %d, want lost after NoVMTimeout", code)
	}
}
