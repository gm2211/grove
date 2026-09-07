package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerPauseResume(t *testing.T) {
	oc := &fakeOrchard{}
	srv := newTestServer(oc, nil, nil, Options{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/mac1/pause", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("pause status = %d, body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/workers/mac1/resume", nil)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("resume status = %d, body=%s", rec2.Code, rec2.Body.String())
	}

	if len(oc.pauseCalls) != 1 || oc.pauseCalls[0] != "mac1" {
		t.Errorf("pauseCalls = %v", oc.pauseCalls)
	}
	if len(oc.resumeCalls) != 1 || oc.resumeCalls[0] != "mac1" {
		t.Errorf("resumeCalls = %v", oc.resumeCalls)
	}
}
