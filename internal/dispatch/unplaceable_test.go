package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

type fakePoolHealth map[string]string

func (f fakePoolHealth) UnplaceableReason(pool string) string { return f[pool] }

func unplaceableSetup(t *testing.T, health PoolHealth, pools ...PoolConfig) (*fakeNomad, Service, Job) {
	t.Helper()
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{PoolHealth: health, UnplaceableGrace: time.Millisecond, Pools: pools})
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	return nc, svc, *job
}

func TestGet_NoNodeAndEnrolmentFailingFailsFast(t *testing.T) {
	reason := "pool linux has no ready node: enrollment failing: enrol linux-a-0: nomad: read node pool linux: 403"
	nc, svc, job := unplaceableSetup(t, fakePoolHealth{"linux": reason})
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-linux/dispatch-1": noLinuxNodes()}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("status = %v, want failed", got.Status)
	}
	if got.FailureReason == nil || *got.FailureReason != reason {
		t.Errorf("FailureReason = %v, want %q", got.FailureReason, reason)
	}
	if len(nc.stopCalls) != 1 || !nc.stopCalls[0].purge {
		t.Errorf("stop calls = %+v, want the Nomad job stopped and purged once", nc.stopCalls)
	}
}

func TestGet_UnplaceableWaitsOutGraceAndNeedsNoNodeEvidence(t *testing.T) {
	health := fakePoolHealth{"linux": "pool linux VMs boot-looping"}

	// Inside the grace: stays pending.
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{PoolHealth: health, UnplaceableGrace: time.Hour})
	job, _, _ := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-linux/dispatch-1": noLinuxNodes()}
	if got, _ := svc.Get(context.Background(), job.ID); got.Status != StatusPending {
		t.Errorf("inside grace: status = %v, want pending", got.Status)
	}

	// Past the grace but the pool has nodes (resources exhausted): a busy fleet is not a fault.
	nc, svc, job2 := unplaceableSetup(t, health)
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-linux/dispatch-1": {{Status: "blocked",
		FailedTGAllocs: map[string]nomad.AllocationMetric{"main": {DimensionExhausted: map[string]int{"memory": 1}, NodesEvaluated: 1}}}}}
	if got, _ := svc.Get(context.Background(), job2.ID); got.Status != StatusPending {
		t.Errorf("busy fleet: status = %v, want pending", got.Status)
	}
}

func TestGet_UnplaceableSkippedWhenNothingKnownOrIsolated(t *testing.T) {
	nc, svc, job := unplaceableSetup(t, fakePoolHealth{})
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-linux/dispatch-1": noLinuxNodes()}
	if got, _ := svc.Get(context.Background(), job.ID); got.Status != StatusPending {
		t.Errorf("no known cause: status = %v, want pending", got.Status)
	}

	nc, svc, job = unplaceableSetup(t, fakePoolHealth{"linux": "boom"}, PoolConfig{Name: "linux", Isolated: true})
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-linux/dispatch-1": noLinuxNodes()}
	got, _ := svc.Get(context.Background(), job.ID)
	if got.Status != StatusPending {
		t.Errorf("isolated pool: status = %v, want pending", got.Status)
	}
	if got.FailureReason != nil && strings.Contains(*got.FailureReason, "boom") {
		t.Errorf("isolated pool failed with %q", *got.FailureReason)
	}
}
