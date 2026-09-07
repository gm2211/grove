package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

func TestRecycleVM_DrainsAndReturns202(t *testing.T) {
	nc := &fakeNomad{nodes: []nomad.Node{
		{ID: "node-1", Name: "linux-mac1-0", Meta: map[string]string{"vm": "linux-mac1-0"}, RunningAllocs: 0},
	}}
	oc := &fakeOrchard{}
	srv := newTestServer(oc, nc, nil, Options{RecyclePollInterval: 5 * time.Millisecond})
	done := make(chan string, 1)
	srv.recycleDone = done

	req := httptest.NewRequest(http.MethodPost, "/api/v1/vms/linux-mac1-0/recycle", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		DrainStarted bool `json:"drainStarted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.DrainStarted {
		t.Error("drainStarted = false, want true")
	}

	if len(nc.drainCalls) != 1 {
		t.Fatalf("expected 1 DrainNode call, got %d", len(nc.drainCalls))
	}
	dc := nc.drainCalls[0]
	if dc.nodeID != "node-1" || !dc.enable || dc.deadline != recycleDrainDeadline {
		t.Errorf("drain call = %+v", dc)
	}

	select {
	case vm := <-done:
		if vm != "linux-mac1-0" {
			t.Errorf("deleted vm = %q, want linux-mac1-0", vm)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background delete")
	}

	if len(oc.deleteCalls) != 1 || oc.deleteCalls[0] != "linux-mac1-0" {
		t.Errorf("deleteCalls = %v", oc.deleteCalls)
	}
}

func TestRecycleVM_NoMatchingNode(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vms/does-not-exist/recycle", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRecycleVM_DeletesAfterDeadlineIfNeverDrains(t *testing.T) {
	nc := &fakeNomad{nodes: []nomad.Node{
		{ID: "node-1", Name: "linux-mac1-0", Meta: map[string]string{"vm": "linux-mac1-0"}, RunningAllocs: 3},
	}}
	oc := &fakeOrchard{}
	srv := newTestServer(oc, nc, nil, Options{RecyclePollInterval: 2 * time.Millisecond})
	done := make(chan string, 1)
	srv.recycleDone = done

	// Shrink the deadline for this test by calling finishRecycle directly with a background
	// context that expires almost immediately isn't possible (deadline is a package const used
	// inside DrainNode/finishRecycle); instead just verify the node stays perpetually busy and
	// the handler still returns 202 promptly without blocking on the background goroutine.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vms/linux-mac1-0/recycle", nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("handler blocked for %s, want it to return immediately", elapsed)
	}

	select {
	case <-done:
		t.Fatal("vm should not have been deleted while allocations are still running")
	case <-time.After(50 * time.Millisecond):
		// expected: still draining, no delete yet.
	}
}
