package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/orchard"
)

const testJobID = "0123456789abcdef"

// lostVM is a fake VM that stops answering once the job is running in it.
func lostVM() *fakeVM {
	vm := newFakeVM()
	vm.runningInspects = 1 << 30
	vm.failInspects = 1 << 30
	return vm
}

// retryHarness runs jobs in pool linux, whose sealed VMs are named by vms (pool-machine-n) and
// answered by the fakes given for them.
func retryHarness(t *testing.T, vms map[string]*fakeVM) *harness {
	t.Helper()
	h := newHarness(t)
	h.orchard.vms = nil
	h.orchard.docker = map[string]*fakeVM{}
	for name, vm := range vms {
		v := sealedVM(name)
		v.UID = "uid-" + name
		h.orchard.vms = append(h.orchard.vms, v)
		h.orchard.docker[name] = vm
	}
	h.runner.MissedPolls = 2
	h.runner.Slots.PerVM = 1
	h.runner.AttemptsDir = t.TempDir()
	h.runner.Queue = Queue{Dir: t.TempDir()}
	h.runner.NoVMTimeout = 50 * time.Millisecond
	return h
}

func retryRequest() Request {
	req := shellRequest()
	req.JobID = testJobID
	req.RetryLost = true
	return req
}

func readRecord(t *testing.T, h *harness) dispatch.AttemptRecord {
	t.Helper()
	rec, ok, err := dispatch.ReadAttempts(h.runner.AttemptsDir, testJobID)
	if err != nil || !ok {
		t.Fatalf("attempt record: ok=%v err=%v", ok, err)
	}
	return rec
}

func TestRun_RerunsALostJobOnAnotherMachine(t *testing.T) {
	good := newFakeVM()
	good.runningInspects = 1
	good.exitCode = 0
	h := retryHarness(t, map[string]*fakeVM{"linux-mac1-0": lostVM(), "linux-mac2-0": good})
	// The first attempt lands in linux-mac1-0: slots tie, and the first VM by name wins.
	if code := h.runner.Run(context.Background(), retryRequest()); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderrString())
	}
	if !good.created {
		t.Fatal("the rerun didn't go to the other machine's VM")
	}
	rec := readRecord(t, h)
	if len(rec.Attempts) != 2 || rec.Quarantined {
		t.Fatalf("record = %+v", rec)
	}
	first, last := rec.Attempts[0], rec.Attempts[1]
	if first.VM != "linux-mac1-0" || first.Worker != "mac1" || first.Outcome != dispatch.AttemptLost || !first.JobStarted ||
		!strings.Contains(first.Reason, "stopped answering") || first.FinishedAt == nil {
		t.Errorf("first attempt = %+v", first)
	}
	if last.VM != "linux-mac2-0" || last.Outcome != dispatch.AttemptExited || last.ExitCode == nil || *last.ExitCode != 0 {
		t.Errorf("second attempt = %+v", last)
	}
	if !strings.Contains(h.stderrString(), "rerunning the job in another VM (attempt 2 of 3)") {
		t.Errorf("stderr = %s", h.stderrString())
	}
}

func TestRun_QuarantinesAJobLostOnTwoMachines(t *testing.T) {
	h := retryHarness(t, map[string]*fakeVM{"linux-mac1-0": lostVM(), "linux-mac2-0": lostVM(), "linux-mac3-0": newFakeVM()})
	if code := h.runner.Run(context.Background(), retryRequest()); code != ExitLost {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderrString())
	}
	rec := readRecord(t, h)
	if !rec.Quarantined || len(rec.Attempts) != 2 || !strings.Contains(rec.QuarantineReason, "mac1, mac2") {
		t.Fatalf("record = %+v", rec)
	}
	if h.orchard.docker["linux-mac3-0"].created {
		t.Fatal("a quarantined job was run a third time")
	}
}

func TestRun_LostTwiceOnOneMachineIsNotQuarantined(t *testing.T) {
	h := retryHarness(t, map[string]*fakeVM{"linux-mac1-0": lostVM(), "linux-mac1-1": lostVM()})
	if code := h.runner.Run(context.Background(), retryRequest()); code != ExitLost {
		t.Fatalf("Run = %d", code)
	}
	rec := readRecord(t, h)
	if rec.Quarantined || len(rec.Attempts) != 2 {
		t.Fatalf("record = %+v; one machine losing a job twice is the machine's problem", rec)
	}
}

func TestRun_SkipsPausedMachines(t *testing.T) {
	good := newFakeVM()
	good.runningInspects = 1
	h := retryHarness(t, map[string]*fakeVM{"linux-mac1-0": newFakeVM(), "linux-mac2-0": good})
	h.orchard.workers = []orchard.Worker{{Name: "mac1", SchedulingPaused: true}, {Name: "mac2"}}
	if code := h.runner.Run(context.Background(), retryRequest()); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderrString())
	}
	if h.orchard.docker["linux-mac1-0"].created || !good.created {
		t.Fatal("a job went to a machine whose scheduling is paused")
	}
}

func TestQueue_FirstComeFirstServed(t *testing.T) {
	q := Queue{Dir: t.TempDir()}
	now := time.Now()
	first, err := q.Join("linux", "a", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.Join("linux", "b", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := q.Join("macos", "c", now.Add(-time.Hour))
	defer other.Leave()

	if n, _ := first.Ahead(); n != 0 {
		t.Errorf("first: %d ahead", n)
	}
	if n, _ := second.Ahead(); n != 1 {
		t.Errorf("second: %d ahead, want 1", n)
	}

	// A runner that died without leaving: its unlocked ticket stops counting and is cleaned up.
	stale := filepath.Join(q.Dir, "linux", "00000000000000000001-dead.wait")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, _ := second.Ahead(); n != 1 {
		t.Errorf("second with a stale ticket: %d ahead, want 1", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale ticket not removed: %v", err)
	}

	first.Leave()
	if n, _ := second.Ahead(); n != 0 {
		t.Errorf("after the first left: %d ahead", n)
	}

	// With one free slot and one job ahead, take waits.
	h := retryHarness(t, map[string]*fakeVM{"linux-mac1-0": newFakeVM()})
	h.runner.Queue = q
	late, _ := q.Join("linux", "late", now.Add(time.Hour))
	defer late.Leave()
	if slot, err := h.runner.take(late, []string{"linux-mac1-0"}); slot != nil || err != nil {
		t.Fatalf("take = %v, %v; a later job must not overtake one waiting", slot, err)
	}
	if slot, err := h.runner.take(second, []string{"linux-mac1-0"}); slot == nil || err != nil {
		t.Fatalf("take for the head of the queue = %v, %v", slot, err)
	} else {
		slot.Release()
	}
	second.Leave()
}
