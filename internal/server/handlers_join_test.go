package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

func TestJoinRequiresOperatorApprovalAndDeliversCredentialOnce(t *testing.T) {
	oc := &fakeOrchard{}
	srv := newTestServer(oc, nil, nil, Options{Token: "operator-secret", ControllerURL: "http://100.64.0.1:6120", ServerURL: "http://100.64.0.1:6130", IssueWorkerBootstrap: func(context.Context, string) (string, error) { return "worker-bootstrap", nil }})
	create := httptest.NewRequest(http.MethodPost, "/api/v1/join/requests", bytes.NewBufferString(`{"name":"new-mac","tailnetIp":"192.0.2.1"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var start struct{ ID, Secret, Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}

	poll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/join/requests/"+start.ID, nil)
		req.Header.Set("X-Grove-Join-Secret", start.Secret)
		out := httptest.NewRecorder()
		srv.ServeHTTP(out, req)
		return out
	}
	pollVerify := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/join/requests/"+start.ID, nil)
		req.Header.Set("X-Grove-Join-Secret", start.Secret)
		req.Header.Set("X-Grove-Join-Phase", "verify")
		out := httptest.NewRecorder()
		srv.ServeHTTP(out, req)
		return out
	}
	if got := poll(); got.Code != http.StatusAccepted {
		t.Fatalf("pending status=%d", got.Code)
	}

	approve := httptest.NewRequest(http.MethodPost, "/api/v1/join/approve/"+start.Code, nil)
	approve.Header.Set("Authorization", "Bearer operator-secret")
	approved := httptest.NewRecorder()
	srv.ServeHTTP(approved, approve)
	if approved.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", approved.Code, approved.Body.String())
	}
	got := poll()
	if got.Code != http.StatusOK {
		t.Fatalf("approved poll status=%d body=%s", got.Code, got.Body.String())
	}
	if !bytes.Contains(got.Body.Bytes(), []byte(`"bootstrapToken":"worker-bootstrap"`)) {
		t.Fatalf("missing bootstrap token: %s", got.Body.String())
	}
	if again := poll(); again.Code != http.StatusOK || !bytes.Contains(again.Body.Bytes(), []byte(`"bootstrapToken":"worker-bootstrap"`)) {
		t.Fatalf("credential delivery was not retryable: %d %s", again.Code, again.Body.String())
	}
	oc.workers = []orchard.Worker{{Name: "new-mac", Offline: false, LastSeen: time.Now().Add(time.Second)}}
	if online := pollVerify(); online.Code != http.StatusOK || !bytes.Contains(online.Body.Bytes(), []byte(`"workerOnline":true`)) {
		t.Fatalf("online status=%d %s", online.Code, online.Body.String())
	}
	if gone := poll(); gone.Code != http.StatusNotFound {
		t.Fatalf("completed request retained: %d", gone.Code)
	}
}

func TestJoinRejectsSpoofedTailnetIP(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/join/requests", bytes.NewBufferString(`{"name":"fake","tailnetIp":"100.64.0.99"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rec.Code)
	}
}

func TestJoinApprovalRequiresOperatorToken(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{Token: "operator-secret"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/join/approve/ABCDEFGH", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}
