package server

import (
	"net/http"
	"strings"
	"testing"
)

// L1: one address can hold only a few pending requests, and bodies are bounded.
func TestJoinRequestsAreCappedPerSourceAndInSize(t *testing.T) {
	h := newJoinHarness(t)
	var first joinHandle
	for i := 0; i < maxPendingJoinsPerSource; i++ {
		j := h.create("mac-a", "100.64.0.20", false)
		if i == 0 {
			first = j
		}
	}
	if rec := h.request("mac-a", "100.64.0.20", false); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request %d from one address: %d, want 429", maxPendingJoinsPerSource+1, rec.Code)
	}
	if rec := h.request("mac-b", "100.64.0.21", false); rec.Code != http.StatusCreated {
		t.Fatalf("another address: %d %s", rec.Code, rec.Body)
	}
	// An approved request no longer counts against its address.
	if rec := h.approve(first.Code, false); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	if rec := h.request("mac-a", "100.64.0.20", false); rec.Code != http.StatusCreated {
		t.Fatalf("after one was approved: %d %s", rec.Code, rec.Body)
	}

	big := `{"name":"mac-c","tailnetIp":"100.64.0.22","pad":"` + strings.Repeat("x", maxJoinRequestBody) + `"}`
	if rec := peerRequest(h.srv, http.MethodPost, "/api/v1/join/requests", big, "100.64.0.22:1", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized join request: %d, want 400", rec.Code)
	}
	if rec := h.request("../etc", "100.64.0.23", false); rec.Code != http.StatusBadRequest {
		t.Fatalf("a name that is no worker name: %d, want 400", rec.Code)
	}
}

func TestJSONBodiesAreBounded(t *testing.T) {
	h := newJoinHarness(t)
	pad := func(n int) string { return strings.Repeat("x", n) }
	for name, tc := range map[string]struct{ method, path, body string }{
		"POST /jobs":              {http.MethodPost, "/api/v1/jobs", `{"kind":"build","script":"` + pad(maxJobBody) + `"}`},
		"PUT /jobs/x/credentials": {http.MethodPut, "/api/v1/jobs/x/credentials", `{"credentials":[{"env":"A","token":"` + pad(maxCredentialsBody) + `"}]}`},
		"POST /access/devices":    {http.MethodPost, "/api/v1/access/devices", `{"name":"` + pad(maxSmallBody) + `","scopes":["read"]}`},
	} {
		rec := peerRequest(h.srv, tc.method, tc.path, tc.body, "127.0.0.1:1", "operator")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "too large") {
			t.Errorf("%s with an oversized body: %d %.80s, want 400 request body too large", name, rec.Code, rec.Body)
		}
	}
	if len(h.ds.submitCalls) != 0 || len(h.access.List()) != 0 {
		t.Fatal("an oversized request was acted on")
	}
}
