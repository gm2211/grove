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
	access, err := NewAccessStore(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(oc, nil, nil, Options{Token: "operator-secret", AccessStore: access, ControllerURL: "http://100.64.0.1:6120", ServerURL: "http://100.64.0.1:6130", IssueWorkerBootstrap: func(context.Context, string) (string, error) { return "worker-bootstrap", nil }})
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
	if !bytes.Contains(got.Body.Bytes(), []byte(`"clientToken":`)) || !bytes.Contains(got.Body.Bytes(), []byte(`"deviceId":`)) {
		t.Fatalf("missing device credential: %s", got.Body.String())
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

func TestPendingJoinsAreOperatorOnlyAndClearOnApproval(t *testing.T) {
	access, err := NewAccessStore(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(&fakeOrchard{}, nil, nil, Options{Token: "operator-secret", AccessStore: access, ControllerURL: "http://100.64.0.1:6120", ServerURL: "http://100.64.0.1:6130", IssueWorkerBootstrap: func(context.Context, string) (string, error) { return "worker-bootstrap", nil }})

	create := func(name, ip string) string {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/join/requests", bytes.NewBufferString(`{"name":"`+name+`","tailnetIp":"`+ip+`"}`))
		req.RemoteAddr = ip + ":40000"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
		var start struct{ Code string }
		if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
			t.Fatal(err)
		}
		return start.Code
	}
	pending := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/join/pending", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	firstCode := create("mac-one", "192.0.2.1")
	create("mac-two", "192.0.2.2")

	// Unauthenticated callers must not be able to read a code: holding one is equivalent to
	// being able to approve the Mac that owns it.
	if got := pending(""); got.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous pending status=%d, want 401", got.Code)
	}
	// Nor may a worker's own device credential, which carries build/agent scope but not operator.
	deviceToken, _, err := access.Issue("mac-one", []string{ScopeRead, ScopeBuild, ScopeAgent})
	if err != nil {
		t.Fatal(err)
	}
	if got := pending(deviceToken); got.Code != http.StatusForbidden {
		t.Fatalf("device pending status=%d, want 403", got.Code)
	}

	listed := func() []pendingJoin {
		rec := pending("operator-secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("operator pending status=%d body=%s", rec.Code, rec.Body.String())
		}
		var out []pendingJoin
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	joins := listed()
	if len(joins) != 2 {
		t.Fatalf("pending=%d, want 2: %+v", len(joins), joins)
	}
	for _, jr := range joins {
		if jr.Name == "" || jr.TailnetIP == "" || jr.Code == "" || jr.ExpiresAt.IsZero() {
			t.Fatalf("pending entry is missing what an operator needs to recognise it: %+v", jr)
		}
	}

	approve := httptest.NewRequest(http.MethodPost, "/api/v1/join/approve/"+firstCode, nil)
	approve.Header.Set("Authorization", "Bearer operator-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, approve)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", rec.Code, rec.Body.String())
	}

	// An approved request is no longer pending — it is now waiting on the Mac itself to pick up
	// its credential, which is not something an operator can act on.
	joins = listed()
	if len(joins) != 1 || joins[0].Name != "mac-two" {
		t.Fatalf("after approval pending=%+v, want only mac-two", joins)
	}
}
