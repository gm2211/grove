package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/snooze"
)

type snoozeEnv struct {
	srv    *Server
	oc     *fakeOrchard
	nc     *fakeNomad
	ds     *fakeDispatch
	pauses *snooze.Store
	owner  string // token holding worker:snooze:mac1
	other  string // token holding worker:snooze:mac2
}

func newSnoozeEnv(t *testing.T) *snoozeEnv {
	t.Helper()
	pauses, err := snooze.NewStore(filepath.Join(t.TempDir(), "pauses.json"))
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewAccessStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := access.Issue("mac1-owner", []string{ScopeRead, SnoozeScope("mac1")})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := access.Issue("mac2-owner", []string{ScopeRead, SnoozeScope("mac2")})
	if err != nil {
		t.Fatal(err)
	}
	oc := &fakeOrchard{workers: []orchard.Worker{{Name: "mac1"}, {Name: "mac2"}}}
	nc := &fakeNomad{nodes: []nomad.Node{
		{ID: "n1", Status: "ready", Meta: map[string]string{"host": "mac1"}},
		{ID: "n2", Status: "ready", Meta: map[string]string{"host": "mac2"}},
		{ID: "n3", Status: "ready", Drain: true, Meta: map[string]string{"host": "mac1"}},
	}}
	ds := &fakeDispatch{}
	srv := newTestServer(oc, nc, ds, Options{Token: "operator", AccessStore: access, Pauses: pauses})
	return &snoozeEnv{srv, oc, nc, ds, pauses, owner, other}
}

func (e *snoozeEnv) do(method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

func TestSnooze_ScopeHolderCanSnoozeOwnWorkerOnly(t *testing.T) {
	e := newSnoozeEnv(t)
	rec := e.do(http.MethodPost, "/workers/mac1/snooze", `{"for":"1h","note":"zoom"}`, e.owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("snooze own worker = %d %s", rec.Code, rec.Body)
	}
	var resp SnoozeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Worker != "mac1" || resp.Mode != "drain" || len(resp.Reasons) != 1 || resp.Reasons[0] != "snooze" ||
		resp.Requeued == nil || resp.LeftRunning == nil {
		t.Fatalf("resp = %+v", resp)
	}
	if d := time.Until(resp.Until); d < 59*time.Minute || d > time.Hour {
		t.Errorf("until in %v", d)
	}
	if got := e.oc.pauseCalls; len(got) != 1 || got[0] != "mac1" {
		t.Errorf("pauseCalls = %v", got)
	}
	// Only mac1's ready, undrained node is drained, with a real deadline, immediately.
	if len(e.nc.drainCalls) != 1 || e.nc.drainCalls[0].nodeID != "n1" || !e.nc.drainCalls[0].enable || e.nc.drainCalls[0].deadline != 2*time.Hour {
		t.Errorf("drainCalls = %+v", e.nc.drainCalls)
	}
	if len(e.ds.evictCalls) != 0 {
		t.Errorf("drain mode evicted: %v", e.ds.evictCalls)
	}

	for _, c := range []struct{ method, path, token string }{
		{http.MethodPost, "/workers/mac2/snooze", e.owner},
		{http.MethodPost, "/workers/mac2/wake", e.owner},
		{http.MethodPost, "/workers/mac1/pause", e.owner},
		{http.MethodPost, "/workers/mac1/resume", e.owner},
		{http.MethodPost, "/workers/mac1/snooze", e.other},
		{http.MethodPost, "/vms/x/recycle", e.owner},
	} {
		if rec := e.do(c.method, c.path, "{}", c.token); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

func TestSnooze_OperatorMayDoAnything(t *testing.T) {
	e := newSnoozeEnv(t)
	if rec := e.do(http.MethodPost, "/workers/mac2/snooze", `{}`, "operator"); rec.Code != http.StatusOK {
		t.Fatalf("operator snooze = %d %s", rec.Code, rec.Body)
	}
	// Default window is two hours.
	if e := e.pauses.Get("mac2"); e.Snooze == nil || time.Until(e.Snooze.Until) < 119*time.Minute {
		t.Errorf("default window: %+v", e.Snooze)
	}
	if rec := e.do(http.MethodPost, "/workers/mac2/wake", "", "operator"); rec.Code != http.StatusOK {
		t.Fatalf("operator wake = %d %s", rec.Code, rec.Body)
	}
}

func TestSnooze_ValidatesWindow(t *testing.T) {
	e := newSnoozeEnv(t)
	for name, body := range map[string]string{
		"both":     `{"for":"1h","until":"2099-01-01T00:00:00Z"}`,
		"too long": `{"for":"25h"}`,
		"past":     `{"until":"2001-01-01T00:00:00Z"}`,
		"garbage":  `{"for":"soon"}`,
		"negative": `{"for":"-1h"}`,
	} {
		if rec := e.do(http.MethodPost, "/workers/mac1/snooze", body, "operator"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
	if rec := e.do(http.MethodPost, "/workers/ghost/snooze", `{}`, "operator"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown worker = %d", rec.Code)
	}
	if len(e.oc.pauseCalls) != 0 {
		t.Errorf("a rejected snooze paused something: %v", e.oc.pauseCalls)
	}
}

func TestSnooze_NowEvictsAndReports(t *testing.T) {
	e := newSnoozeEnv(t)
	e.ds.evictRequeued = []string{"job-b"}
	e.ds.evictLeft = []string{"job-agent"}
	rec := e.do(http.MethodPost, "/workers/mac1/snooze", `{"for":"30m","now":true}`, e.owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var resp SnoozeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Mode != "now" || len(resp.Requeued) != 1 || resp.Requeued[0] != "job-b" || len(resp.LeftRunning) != 1 || resp.LeftRunning[0] != "job-agent" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(e.ds.evictCalls) != 1 || e.ds.evictCalls[0] != "mac1" {
		t.Errorf("evictCalls = %v", e.ds.evictCalls)
	}
}

func TestSnooze_ResumeDoesNotEndASnoozeAndHealthPauseSurvivesWake(t *testing.T) {
	e := newSnoozeEnv(t)
	e.do(http.MethodPost, "/workers/mac1/snooze", `{"for":"1h"}`, "operator")
	rec := e.do(http.MethodPost, "/workers/mac1/resume", "", "operator")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "grove worker wake") {
		t.Fatalf("resume under snooze = %d %s", rec.Code, rec.Body)
	}
	if len(e.oc.resumeCalls) != 0 {
		t.Fatalf("resumed a snoozed worker: %v", e.oc.resumeCalls)
	}

	// Health also paused it: the owner's wake must not clear that.
	if err := e.pauses.SetHealth("mac1", time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	rec = e.do(http.MethodPost, "/workers/mac1/wake", "", e.owner)
	var wake WakeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &wake)
	if rec.Code != http.StatusOK || wake.Resumed || len(wake.StillPausedBecause) != 1 || wake.StillPausedBecause[0] != "health" {
		t.Fatalf("wake = %d %+v", rec.Code, wake)
	}
	if len(e.oc.resumeCalls) != 0 {
		t.Fatalf("wake resumed a health-paused worker")
	}
	// The operator's resume now clears the health reason.
	if rec := e.do(http.MethodPost, "/workers/mac1/resume", "", "operator"); rec.Code != http.StatusNoContent {
		t.Fatalf("resume = %d %s", rec.Code, rec.Body)
	}
	if len(e.pauses.Reasons("mac1", time.Now())) != 0 || len(e.oc.resumeCalls) != 1 {
		t.Errorf("reasons=%v resumes=%v", e.pauses.Reasons("mac1", time.Now()), e.oc.resumeCalls)
	}
}

func TestSnooze_WakeResumesAndRefusesWhenNotSnoozed(t *testing.T) {
	e := newSnoozeEnv(t)
	if rec := e.do(http.MethodPost, "/workers/mac1/wake", "", e.owner); rec.Code != http.StatusConflict {
		t.Fatalf("wake of an unsnoozed worker = %d", rec.Code)
	}
	e.do(http.MethodPost, "/workers/mac1/snooze", `{"for":"1h"}`, e.owner)
	rec := e.do(http.MethodPost, "/workers/mac1/wake", "", e.owner)
	var wake WakeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &wake)
	if rec.Code != http.StatusOK || !wake.Resumed || len(e.oc.resumeCalls) != 1 {
		t.Fatalf("wake = %d %+v resumes=%v", rec.Code, wake, e.oc.resumeCalls)
	}
}

func TestSnooze_OperatorPauseIsRecordedAndGetReportsIt(t *testing.T) {
	e := newSnoozeEnv(t)
	e.do(http.MethodPost, "/workers/mac1/pause", "", "operator")
	if e.pauses.Get("mac1").Operator == nil {
		t.Fatal("operator pause not recorded")
	}
	e.do(http.MethodPost, "/workers/mac1/snooze", `{"for":"1h","note":"call"}`, e.owner)
	// wake leaves it paused for the operator reason
	e.do(http.MethodPost, "/workers/mac1/wake", "", e.owner)
	if len(e.oc.resumeCalls) != 0 {
		t.Fatal("snooze+wake undid an operator pause")
	}
	e.do(http.MethodPost, "/workers/mac1/snooze", `{"for":"1h","note":"call"}`, e.owner)
	rec := e.do(http.MethodGet, "/workers/mac1/snooze", "", e.owner)
	var st SnoozeStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if rec.Code != 200 || !st.Snoozed || st.Note != "call" || len(st.Reasons) != 2 {
		t.Fatalf("status = %d %+v", rec.Code, st)
	}
	if rec := e.do(http.MethodGet, "/workers/mac2/snooze", "", e.owner); rec.Code != http.StatusForbidden {
		t.Errorf("reading another worker's snooze = %d", rec.Code)
	}
}

func TestSnooze_FleetShowsSnoozedWorker(t *testing.T) {
	e := newSnoozeEnv(t)
	e.oc.workers[0].SchedulingPaused = true
	e.do(http.MethodPost, "/workers/mac1/snooze", `{"for":"1h"}`, "operator")
	rec := e.do(http.MethodGet, "/fleet", "", "operator")
	var resp FleetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	w := resp.Workers[0]
	if w.Status != "snoozed" || w.SnoozedUntil == nil || w.SnoozeMode != "drain" || len(w.PauseReasons) == 0 || resp.Totals.WorkersSnoozed != 1 {
		t.Fatalf("worker = %+v totals=%+v", w, resp.Totals)
	}
	if resp.Workers[1].Status == "snoozed" {
		t.Errorf("mac2 shown snoozed")
	}
}

func TestSnooze_NotConfigured(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/mac1/snooze", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("= %d", rec.Code)
	}
}

func TestSnoozeScope_ValidationAndJoinGrant(t *testing.T) {
	access, _ := NewAccessStore(filepath.Join(t.TempDir(), "d.json"))
	if _, p, err := access.Issue("x", []string{SnoozeScope("g-macbook-m5.local")}); err != nil || p.Scopes[0] != "worker:snooze:g-macbook-m5.local" {
		t.Fatalf("%v %v", p, err)
	}
	for _, bad := range []string{"worker:snooze:", "worker:snooze:a/b", "worker:snooze:a b", "worker:snooze"} {
		if _, _, err := access.Issue("x", []string{bad}); err == nil {
			t.Errorf("accepted scope %q", bad)
		}
	}
	// A worker host's Tailscale identity is still refused, and Tailscale never grants snooze scopes.
	peers := fakePeers{"100.64.0.30:1": {ID: "tailscale:n", Name: "mac", Node: "mac1.tail1.ts.net", Scopes: []string{SnoozeScope("mac1"), ScopeRead}}}
	oc := &fakeOrchard{workers: []orchard.Worker{{Name: "mac1.local"}}}
	srv := newTestServer(oc, nil, nil, Options{Token: "operator", Tailscale: peers})
	if rec := peerRequest(srv, http.MethodGet, "/api/v1/whoami", "", "100.64.0.30:1", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("worker host identity = %d, want 401", rec.Code)
	}
}
