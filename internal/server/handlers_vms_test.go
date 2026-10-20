package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// Recycling a VM deletes it through Orchard (its shutdown script drains it) and returns 202 right
// away; the fleet reconciler recreates it.
func TestRecycleVM_DeletesTheVM(t *testing.T) {
	oc := &fakeOrchard{vms: []orchard.VM{{Name: "linux-mac1-0", Status: "running", Softnet: true, SoftnetBlock: []string{"out @host"}}}}
	srv := newTestServer(oc, nil, Options{})
	done := make(chan string, 1)
	srv.recycleDone = done

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/vms/linux-mac1-0/recycle", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		DrainStarted bool `json:"drainStarted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.DrainStarted {
		t.Fatalf("body = %s (%v), want drainStarted", rec.Body.String(), err)
	}
	select {
	case vm := <-done:
		if vm != "linux-mac1-0" {
			t.Errorf("deleted vm = %q", vm)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the delete")
	}
	if len(oc.deleteCalls) != 1 || oc.deleteCalls[0] != "linux-mac1-0" {
		t.Errorf("deleteCalls = %v", oc.deleteCalls)
	}
}

func TestRecycleVM_UnknownVM(t *testing.T) {
	srv := newTestServer(nil, nil, Options{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/vms/does-not-exist/recycle", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
