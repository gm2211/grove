package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

// evictSetup submits a job of kind and starts it on worker "g-macbook-m5.local".
func evictSetup(t *testing.T, kind Kind) (*fakeNomad, *service, string) {
	t.Helper()
	nc := &fakeNomad{nodesByID: map[string]nomad.Node{
		"n1": {ID: "n1", Meta: map[string]string{"pool": "macos", "host": "g-macbook-m5.local", "vm": "macos-g-0"}},
	}}
	svc := newTestServiceWithOpts(t, nc, Options{Pools: []PoolConfig{{Name: "macos"}}}).(*service)
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: kind, Pool: "macos", Repo: "https://github.com/x/y", Script: "make", Env: map[string]string{"A": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	nc.allocationsByJob = map[string][]nomad.Allocation{"grove-" + string(kind) + "-macos/dispatch-1": {{ID: "a1", NodeID: "n1", NodeName: "vm", ClientStatus: "running", CreatedAt: time.Now()}}}
	if got, err := svc.Get(context.Background(), job.ID); err != nil || got.Status != StatusRunning {
		t.Fatalf("not running: %+v %v", got, err)
	}
	return nc, svc, job.ID
}

func TestEvict_BuildJobRequeuedOnceOldStoppedAndPurged(t *testing.T) {
	nc, svc, id := evictSetup(t, KindBuild)
	original := nc.dispatchCalls[0]
	nc.dispatchResult = &nomad.DispatchResult{DispatchedJobID: "grove-build-macos/dispatch-2", EvalID: "e2"}

	requeued, left, err := svc.Evict(context.Background(), "g-macbook-m5.local", "worker snoozed")
	if err != nil || len(requeued) != 1 || requeued[0] != id || len(left) != 0 {
		t.Fatalf("requeued=%v left=%v err=%v", requeued, left, err)
	}
	if len(nc.dispatchCalls) != 2 {
		t.Fatalf("dispatch calls = %d, want exactly one redispatch", len(nc.dispatchCalls))
	}
	again := nc.dispatchCalls[1]
	if again.jobName != "grove-build-macos" {
		t.Errorf("redispatched to %q, want the same pool's job", again.jobName)
	}
	// Workers are fungible: nothing in the redispatch may pin it to the snoozed host.
	if len(again.meta) != len(original.meta) {
		t.Errorf("meta changed: %v vs %v", again.meta, original.meta)
	}
	for k, v := range again.meta {
		if strings.Contains(strings.ToLower(k), "host") || strings.Contains(strings.ToLower(k), "worker") || strings.Contains(v, "g-macbook-m5") {
			t.Errorf("redispatch meta pins the old worker: %s=%s", k, v)
		}
	}
	if string(again.payload) != string(original.payload) {
		t.Errorf("payload changed")
	}
	if len(nc.stopCalls) != 1 || nc.stopCalls[0].jobID != "grove-build-macos/dispatch-1" || !nc.stopCalls[0].purge {
		t.Errorf("stop calls = %+v, want the old job stopped and purged", nc.stopCalls)
	}
	got, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending || len(got.Evictions) != 1 || got.Evictions[0].Worker != "g-macbook-m5.local" || got.Placement != nil {
		t.Errorf("job after eviction: status=%v evictions=%+v placement=%+v", got.Status, got.Evictions, got.Placement)
	}
	// A second eviction finds nothing left on the worker.
	requeued, left, _ = svc.Evict(context.Background(), "g-macbook-m5.local", "again")
	if len(requeued) != 0 || len(left) != 0 || len(nc.dispatchCalls) != 2 {
		t.Errorf("second evict moved %v / left %v", requeued, left)
	}
}

func TestEvict_AgentJobLeftRunning(t *testing.T) {
	nc, svc, id := evictSetup(t, KindAgent)
	requeued, left, err := svc.Evict(context.Background(), "g-macbook-m5.local", "worker snoozed")
	if err != nil || len(requeued) != 0 || len(left) != 1 || left[0] != id {
		t.Fatalf("requeued=%v left=%v err=%v", requeued, left, err)
	}
	if len(nc.dispatchCalls) != 1 || len(nc.stopCalls) != 0 {
		t.Errorf("agent job was touched: dispatch=%d stop=%+v", len(nc.dispatchCalls), nc.stopCalls)
	}
}

func TestEvict_OtherWorkersUntouched(t *testing.T) {
	nc, svc, _ := evictSetup(t, KindBuild)
	requeued, left, _ := svc.Evict(context.Background(), "some-other-mac", "x")
	if len(requeued) != 0 || len(left) != 0 || len(nc.stopCalls) != 0 {
		t.Fatalf("evicted a job from the wrong worker: %v %v", requeued, left)
	}
}

func TestEvict_FailedRedispatchLeavesOriginal(t *testing.T) {
	nc, svc, id := evictSetup(t, KindBuild)
	nc.dispatchErr = errors.New("nomad down")
	requeued, left, err := svc.Evict(context.Background(), "g-macbook-m5.local", "x")
	if err != nil || len(requeued) != 0 || len(left) != 1 {
		t.Fatalf("requeued=%v left=%v err=%v", requeued, left, err)
	}
	if len(nc.stopCalls) != 0 {
		t.Errorf("original stopped despite failed redispatch: %+v", nc.stopCalls)
	}
	got, _ := svc.Get(context.Background(), id)
	if got.Status != StatusRunning || len(got.Evictions) != 0 {
		t.Errorf("status=%v evictions=%v", got.Status, got.Evictions)
	}
}

func TestAsEvictor(t *testing.T) {
	svc := newTestServiceWithOpts(t, &fakeNomad{}, Options{})
	if _, ok := AsEvictor(svc); !ok {
		t.Fatal("service is not an Evictor")
	}
}
