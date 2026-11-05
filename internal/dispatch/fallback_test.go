package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

func fallbackService(t *testing.T, nc *fakeNomad) *service {
	t.Helper()
	svc := newTestServiceWithOpts(t, nc, Options{
		Pools: []PoolConfig{
			{Name: "linux", Fallback: "macos"},
			{Name: "macos"},
		},
		FallbackNoNodesAfter: time.Nanosecond,
		FallbackAfter:        time.Hour,
	})
	return svc.(*service)
}

// noLinuxNodes is the blocked evaluation Nomad records when no node has meta.pool = linux.
func noLinuxNodes() []nomad.Evaluation {
	return []nomad.Evaluation{{Status: "blocked", FailedTGAllocs: map[string]nomad.AllocationMetric{
		"main": {ConstraintFiltered: map[string]int{"${meta.pool} = linux": 1}, NodesEvaluated: 1},
	}}}
}

func TestFallback_NoLinuxNodes_MovesToMacOS(t *testing.T) {
	nc := fakeWithReadyPools()
	svc := fallbackService(t, nc)
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/argos", Ref: "abc", Script: "bash scripts/lint-all.sh", Env: map[string]string{"CI": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	original := nc.dispatchCalls[0]
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-build-linux/dispatch-1": noLinuxNodes()}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fallback == nil || got.Fallback.From != "linux" || got.Fallback.To != "macos" || got.Status != StatusPending {
		t.Fatalf("want a pending job moved linux -> macos, got status=%s fallback=%+v", got.Status, got.Fallback)
	}
	if len(nc.dispatchCalls) != 2 || nc.dispatchCalls[1].jobName != "grove-build-macos" {
		t.Fatalf("dispatch calls = %+v, want a second one to grove-build-macos", nc.dispatchCalls)
	}
	moved := nc.dispatchCalls[1]
	if fmt.Sprint(moved.meta) != fmt.Sprint(original.meta) || !bytes.Equal(moved.payload, original.payload) {
		t.Errorf("the macOS dispatch must carry the original meta and payload unchanged")
	}
	if len(nc.stopCalls) != 1 || nc.stopCalls[0].jobID != "grove-build-linux/dispatch-1" || !nc.stopCalls[0].purge {
		t.Errorf("stop calls = %+v, want the Linux copy stopped and purged", nc.stopCalls)
	}
	if rec, _ := svc.store.get(job.ID); rec.NomadJobID != "grove-build-macos/dispatch-1" {
		t.Errorf("record still tracks %s, want the macOS job", rec.NomadJobID)
	}

	// Moved once only.
	svc.cache = map[string]cachedStatus{}
	if _, err := svc.Get(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if len(nc.dispatchCalls) != 2 {
		t.Fatalf("a job falls back at most once, got %d dispatches", len(nc.dispatchCalls))
	}
}

func TestFallback_BusyPoolWaitsForFallbackAfter(t *testing.T) {
	nc := fakeWithReadyPools()
	svc := fallbackService(t, nc)
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatal(err)
	}
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-linux/dispatch-1": {{Status: "blocked", FailedTGAllocs: map[string]nomad.AllocationMetric{
		"main": {DimensionExhausted: map[string]int{"memory": 1}, NodesEvaluated: 1},
	}}}}
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fallback != nil || len(nc.dispatchCalls) != 1 {
		t.Fatalf("a job waiting on busy Linux nodes must wait FallbackAfter first, got %+v", got.Fallback)
	}
}

func TestFallback_PinPoolStays(t *testing.T) {
	for _, req := range []JobRequest{
		{Kind: KindShell, Pool: "linux", Script: "docker build .", PinPool: true},
	} {
		nc := fakeWithReadyPools()
		svc := fallbackService(t, nc)
		job, _, err := svc.Submit(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-shell-linux/dispatch-1": noLinuxNodes()}
		got, err := svc.Get(context.Background(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Fallback != nil {
			t.Fatalf("%+v must stay on linux", req)
		}
	}
}

func TestFallback_StopsAfterRepeatedFailuresThere(t *testing.T) {
	nc := fakeWithReadyPools()
	svc := fallbackService(t, nc)
	req := JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/argos", Script: "apt-get install -y foo"}
	now := time.Now().UTC()
	for i := 0; i < fallbackStrikes; i++ {
		finished := now.Add(-time.Duration(i+1) * time.Hour)
		rec := &record{Job: Job{ID: fmt.Sprintf("old%d", i), Request: req, Status: StatusFailed, FinishedAt: &finished,
			Fallback: &Fallback{From: "linux", To: "macos"}}}
		if err := svc.store.put(rec); err != nil {
			t.Fatal(err)
		}
	}

	job, _, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-build-linux/dispatch-1": noLinuxNodes()}
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fallback != nil {
		t.Fatal("a job whose last runs on macOS all failed must wait for Linux instead")
	}
	if !strings.Contains(got.PendingReason, "won't fall back until") {
		t.Errorf("PendingReason = %q, want it to say why the job isn't falling back", got.PendingReason)
	}

	// One success among the recent runs keeps the fallback on.
	ok := now.Add(-time.Minute)
	if err := svc.store.put(&record{Job: Job{ID: "good", Request: req, Status: StatusSuccess, FinishedAt: &ok, Fallback: &Fallback{From: "linux", To: "macos"}}}); err != nil {
		t.Fatal(err)
	}
	if _, blocked := svc.fallbackBlockedUntil(req, now); blocked {
		t.Fatal("a recent success on macOS must keep the fallback on")
	}
}

// fakeWithReadyPools is a fake Nomad with one ready, eligible node in each pool these tests name,
// so a due fallback is never held back for want of a node in its target.
func fakeWithReadyPools() *fakeNomad {
	nc := &fakeNomad{}
	for _, pool := range []string{"linux", "macos", "sealed"} {
		nc.nodes = append(nc.nodes, nomad.Node{ID: "n-" + pool, Name: "n-" + pool, Status: "ready", Eligibility: "eligible", Meta: map[string]string{"pool": pool}})
	}
	return nc
}

func TestFallback_SkipsAPoolWithNoReadyNode(t *testing.T) {
	for name, macos := range map[string]*nomad.Node{
		"none":     nil,
		"down":     {Status: "down", Eligibility: "eligible"},
		"draining": {Status: "ready", Eligibility: "eligible", Drain: true},
		"inelig":   {Status: "ready", Eligibility: "ineligible"},
	} {
		t.Run(name, func(t *testing.T) {
			nc := &fakeNomad{}
			if macos != nil {
				macos.Meta = map[string]string{"pool": "macos"}
				nc.nodes = []nomad.Node{*macos}
			}
			svc := fallbackService(t, nc)
			job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/argos", Ref: "abc", Script: "true"})
			if err != nil {
				t.Fatal(err)
			}
			nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-build-linux/dispatch-1": noLinuxNodes()}
			got, err := svc.Get(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Fallback != nil || len(nc.dispatchCalls) != 1 {
				t.Fatalf("moved to a pool with no ready node: %+v", got.Fallback)
			}
			if !strings.Contains(got.PendingReason, "not moving to macos: it has no ready node") {
				t.Fatalf("pending reason %q doesn't say why it stayed", got.PendingReason)
			}
		})
	}
}
