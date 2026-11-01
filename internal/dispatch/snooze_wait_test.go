package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

type fakeWaiter struct {
	fakePoolHealth
	reason    string
	until     time.Time
	exclusive bool
}

func (f fakeWaiter) WaitReason(string, time.Time) (string, time.Time, bool) {
	return f.reason, f.until, f.exclusive
}

func busyEvals() []nomad.Evaluation {
	return []nomad.Evaluation{{Status: "blocked", FailedTGAllocs: map[string]nomad.AllocationMetric{
		"main": {DimensionExhausted: map[string]int{"memory": 1}, NodesEvaluated: 1},
	}}}
}

func waitSetup(t *testing.T, w PoolHealth) (*fakeNomad, Service, Job) {
	t.Helper()
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{PoolHealth: w, UnplaceableGrace: time.Millisecond, PendingTimeout: time.Millisecond})
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "macos", Script: "true"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	return nc, svc, *job
}

// The only matching worker is snoozed: the job is accepted and queued, never failed as
// unplaceable, and never lost to the pending timeout while the snooze lasts.
func TestSnooze_OnlyWorkerSnoozedQueuesInsteadOfFailing(t *testing.T) {
	until := time.Now().Add(time.Hour)
	w := fakeWaiter{
		fakePoolHealth: fakePoolHealth{"macos": "every worker for pool macos is paused"},
		reason:         "queued: workers for pool macos snoozed (g-macbook-m5.local snoozed until 15:04)",
		until:          until, exclusive: true,
	}
	nc, svc, job := waitSetup(t, w)
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-macos/dispatch-1": noLinuxNodes()}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %v (%v), want pending", got.Status, got.FailureReason)
	}
	if got.PendingReason != w.reason {
		t.Errorf("PendingReason = %q", got.PendingReason)
	}
	if got.QueuedUntil == nil || !got.QueuedUntil.Equal(until) {
		t.Errorf("QueuedUntil = %v, want %v", got.QueuedUntil, until)
	}
	if len(nc.stopCalls) != 0 {
		t.Errorf("a queued job's Nomad job was stopped: %+v", nc.stopCalls)
	}
}

// Other Macs exist but are busy: the snooze must still keep the job from being lost to the 30m
// pending timeout, and the reason must not claim it waits for the laptop specifically.
func TestSnooze_BusyPoolPushesPendingTimeout(t *testing.T) {
	until := time.Now().Add(time.Hour)
	w := fakeWaiter{
		reason: "waiting for a free macos worker (g-macbook-m5.local snoozed until 15:04)",
		until:  until,
	}
	nc, svc, job := waitSetup(t, w)
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-macos/dispatch-1": busyEvals()}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %v (%v), want pending", got.Status, got.FailureReason)
	}
	if !strings.HasPrefix(got.PendingReason, "waiting for a free macos worker") || got.QueuedUntil == nil {
		t.Errorf("reason=%q queuedUntil=%v", got.PendingReason, got.QueuedUntil)
	}
}

// Without a snooze the old behaviour holds: a stuck job is lost after the pending timeout.
func TestSnooze_NoSnoozeStillTimesOut(t *testing.T) {
	nc, svc, job := waitSetup(t, fakeWaiter{})
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-macos/dispatch-1": busyEvals()}
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusLost {
		t.Fatalf("status = %v, want lost", got.Status)
	}
}

// Once the snooze ends the timeout counts from its end: after that plus PendingTimeout the job is
// lost as before.
func TestSnooze_PastSnoozeDoesNotMoveClockBack(t *testing.T) {
	until := time.Now().Add(-10 * time.Millisecond) // already over, but later than SubmittedAt
	w := fakeWaiter{reason: "waiting for a free macos worker (x snoozed until 15:04)", until: until}
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{PoolHealth: w, PendingTimeout: time.Hour})
	job, _, _ := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "macos", Script: "true"})
	// SubmittedAt is after `until` here, so PendingSince must not move the clock backwards.
	got, _ := svc.Get(context.Background(), job.ID)
	if got.Status != StatusPending {
		t.Fatalf("status = %v", got.Status)
	}
}
