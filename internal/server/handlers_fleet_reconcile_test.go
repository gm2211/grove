package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/orchard"
)

func TestHandleFleetReconcile_Disabled(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/reconcile", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp FleetReconcileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Enabled {
		t.Errorf("Enabled = true, want false when nothing has published status")
	}
	if len(resp.LastPlan.Creates) != 0 || len(resp.LastPlan.Deletes) != 0 {
		t.Errorf("LastPlan = %+v, want empty", resp.LastPlan)
	}
}

func TestHandleFleetReconcile_ReportsLatestTick(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})

	srv.FleetReconcile().SetEnabled(true, 30*time.Second)
	at := time.Now().UTC().Truncate(time.Second)
	plan := fleet.Plan{
		Creates: []fleet.PlannedCreate{{Spec: orchard.VMSpec{Name: "synth-mac1-0"}}},
		Deletes: []fleet.PlannedDelete{{Name: "synth-mac1-1"}},
	}
	srv.FleetReconcile().Report(plan, nil, at)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/reconcile", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp FleetReconcileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Enabled {
		t.Errorf("Enabled = false, want true")
	}
	if resp.Interval != "30s" {
		t.Errorf("Interval = %q, want 30s", resp.Interval)
	}
	if !resp.LastRunAt.Equal(at) {
		t.Errorf("LastRunAt = %v, want %v", resp.LastRunAt, at)
	}
	if len(resp.LastPlan.Creates) != 1 || resp.LastPlan.Creates[0] != "synth-mac1-0" {
		t.Errorf("LastPlan.Creates = %+v, want [synth-mac1-0]", resp.LastPlan.Creates)
	}
	if len(resp.LastPlan.Deletes) != 1 || resp.LastPlan.Deletes[0] != "synth-mac1-1" {
		t.Errorf("LastPlan.Deletes = %+v, want [synth-mac1-1]", resp.LastPlan.Deletes)
	}
	if resp.LastError != "" {
		t.Errorf("LastError = %q, want empty", resp.LastError)
	}
}

func TestHandleFleetReconcile_ReportsLastError(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})

	srv.FleetReconcile().SetEnabled(true, 30*time.Second)
	srv.FleetReconcile().Report(fleet.Plan{}, errors.New("read fleet.yaml: no such file"), time.Now())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/reconcile", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var resp FleetReconcileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LastError == "" {
		t.Errorf("LastError = empty, want the reported error message")
	}
}

func TestHandleFleet_IncludesReconcileSummary(t *testing.T) {
	oc := &fakeOrchard{}
	srv := newTestServer(oc, nil, nil, Options{})

	at := time.Now().UTC().Truncate(time.Second)
	srv.FleetReconcile().SetEnabled(true, 30*time.Second)
	srv.FleetReconcile().Report(fleet.Plan{}, nil, at)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp FleetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Reconcile.Enabled {
		t.Errorf("Reconcile.Enabled = false, want true")
	}
	if !resp.Reconcile.LastRunAt.Equal(at) {
		t.Errorf("Reconcile.LastRunAt = %v, want %v", resp.Reconcile.LastRunAt, at)
	}
}
