package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeGroveAPI is a minimal in-memory implementation of the grove HTTP API (v1), good enough to
// exercise every tool handler's request shape and response parsing without a real grove server.
type fakeGroveAPI struct {
	mu sync.Mutex

	token string

	workers []orchard.Worker
	vms     []orchard.VM
	nodes   []nomad.Node

	jobs      map[string]*dispatch.Job
	nextJobID int

	recycled      []string
	pausedWorkers map[string]bool
	submittedJobs []dispatch.JobRequest
	logsByJobID   map[string]string
}

func newFakeGroveAPI() *fakeGroveAPI {
	return &fakeGroveAPI{
		token:         "test-token",
		jobs:          map[string]*dispatch.Job{},
		pausedWorkers: map[string]bool{},
		logsByJobID:   map[string]string{},
	}
}

func (f *fakeGroveAPI) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(f.handle))
}

func (f *fakeGroveAPI) handle(w http.ResponseWriter, r *http.Request) {
	if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/fleet":
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, http.StatusOK, FleetResponse{Workers: f.workers, VMs: f.vms, Nodes: f.nodes})

	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/jobs":
		var req dispatch.JobRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.nextJobID++
		id := fmt.Sprintf("job-%d", f.nextJobID)
		now := time.Now()
		job := &dispatch.Job{ID: id, Request: req, Status: dispatch.StatusRunning, SubmittedAt: now, StartedAt: &now}
		f.jobs[id] = job
		f.submittedJobs = append(f.submittedJobs, req)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"id": id})

	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/jobs":
		f.mu.Lock()
		defer f.mu.Unlock()
		out := make([]dispatch.Job, 0, len(f.jobs))
		for _, j := range f.jobs {
			out = append(out, *j)
		}
		writeJSON(w, http.StatusOK, out)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/jobs/") && strings.HasSuffix(r.URL.Path, "/logs"):
		id, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/"), "/logs"))
		f.mu.Lock()
		logs := f.logsByJobID[id]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(logs))

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/jobs/"):
		id, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/"))
		f.mu.Lock()
		job, ok := f.jobs[id]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, job)

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/jobs/"):
		id, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/"))
		f.mu.Lock()
		job, ok := f.jobs[id]
		if ok {
			job.Status = dispatch.StatusCanceled
		}
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/recycle"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/vms/"), "/recycle")
		name, _ = url.PathUnescape(name)
		f.mu.Lock()
		f.recycled = append(f.recycled, name)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/pause") || strings.HasSuffix(r.URL.Path, "/resume")):
		paused := strings.HasSuffix(r.URL.Path, "/pause")
		trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/workers/")
		name := strings.TrimSuffix(strings.TrimSuffix(trimmed, "/pause"), "/resume")
		name, _ = url.PathUnescape(name)
		f.mu.Lock()
		f.pausedWorkers[name] = paused
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "no route: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newTestHandlers spins up a fake grove API server and returns handlers wired against it. The
// server is closed automatically via t.Cleanup.
func newTestHandlers(t *testing.T, fake *fakeGroveAPI) *handlers {
	t.Helper()
	srv := fake.server()
	t.Cleanup(srv.Close)
	client, err := newAPIClient(srv.URL, fake.token)
	if err != nil {
		t.Fatalf("newAPIClient: %v", err)
	}
	return &handlers{client: client}
}

func TestFleetTool(t *testing.T) {
	fake := newFakeGroveAPI()
	fake.workers = []orchard.Worker{
		{Name: "mac-1", Offline: false, SchedulingPaused: false},
		{Name: "mac-2", Offline: true, SchedulingPaused: true},
	}
	fake.vms = []orchard.VM{
		{Name: "linux-mac-1-0", Status: "running", Labels: map[string]string{"pool": "linux"}},
		{Name: "linux-mac-1-1", Status: "pending", Labels: map[string]string{"pool": "linux"}},
		{Name: "macos-mac-2-0", Status: "running", Labels: map[string]string{"pool": "macos"}},
	}
	fake.nodes = []nomad.Node{
		{ID: "n1", Status: "ready", RunningAllocs: 2},
		{ID: "n2", Status: "down", RunningAllocs: 0},
	}
	fake.jobs["j1"] = &dispatch.Job{ID: "j1", Status: dispatch.StatusRunning}
	fake.jobs["j2"] = &dispatch.Job{ID: "j2", Status: dispatch.StatusSuccess}

	h := newTestHandlers(t, fake)

	_, summary, err := h.fleet(t.Context(), nil, fleetArgs{})
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	if summary.Workers.Total != 2 || summary.Workers.Online != 1 || summary.Workers.Cordoned != 1 {
		t.Errorf("worker counts = %+v, want total=2 online=1 cordoned=1", summary.Workers)
	}
	if summary.VMs.Total != 3 || summary.VMs.ByPool["linux"] != 2 || summary.VMs.ByPool["macos"] != 1 {
		t.Errorf("vm counts = %+v", summary.VMs)
	}
	if summary.VMs.ByStatus["running"] != 2 || summary.VMs.ByStatus["pending"] != 1 {
		t.Errorf("vm status counts = %+v", summary.VMs.ByStatus)
	}
	if summary.Nodes.Total != 2 || summary.Nodes.Ready != 1 {
		t.Errorf("node counts = %+v", summary.Nodes)
	}
	if summary.RunningJobs != 1 {
		t.Errorf("running jobs = %d, want 1", summary.RunningJobs)
	}
	if len(summary.Hosts) != 2 {
		t.Errorf("hosts = %v, want 2 entries", summary.Hosts)
	}
}

func TestSummarizeFleetFallsBackToAllocCountsWhenJobsUnavailable(t *testing.T) {
	fleet := &FleetResponse{Nodes: []nomad.Node{{ID: "n1", Status: "ready", RunningAllocs: 3}}}
	summary := summarizeFleet(fleet, nil)
	if summary.RunningJobs != 3 {
		t.Errorf("RunningJobs = %d, want 3 (fallback to alloc sum)", summary.RunningJobs)
	}
}

func TestRunToolSubmitsJobRequestWithoutWaiting(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	args := runArgs{
		Pool:   "linux",
		Script: "echo hi",
		Wait:   boolPtr(false),
	}
	_, res, err := h.run(t.Context(), nil, args)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.JobID == "" {
		t.Fatalf("expected a job id, got empty result %+v", res)
	}
	if res.Status != "" {
		t.Errorf("Status = %q, want empty when wait=false", res.Status)
	}

	if len(fake.submittedJobs) != 1 {
		t.Fatalf("expected 1 submitted job, got %d", len(fake.submittedJobs))
	}
	got := fake.submittedJobs[0]
	if got.Pool != "linux" || got.Script != "echo hi" || got.Kind != dispatch.KindShell {
		t.Errorf("submitted job request = %+v, want pool=linux script='echo hi' kind=shell", got)
	}
	if got.Requester != "mcp" {
		t.Errorf("Requester = %q, want %q (no client info on a nil request)", got.Requester, "mcp")
	}
}

func TestRunToolDefaultsKindAndRequesterFromClientInfo(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	req := &mcpsdk.CallToolRequest{
		Params: &mcpsdk.CallToolParamsRaw{},
	}
	// ClientInfo() reads from the session's InitializeParams; without a live session it returns
	// nil, so the requester falls back to "mcp". This is exercised via the nil-request case
	// above; here we just confirm passing a non-nil request doesn't break anything.
	_, res, err := h.run(t.Context(), req, runArgs{Pool: "linux", Script: "true", Wait: boolPtr(false)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.JobID == "" {
		t.Error("expected a job id")
	}
}

func TestRunToolWaitsForTerminalStatus(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	oldInterval := pollInterval
	pollInterval = 5 * time.Millisecond
	defer func() { pollInterval = oldInterval }()

	// Submit a job, then flip it to success shortly after so the poll loop observes a
	// transition from running -> success.
	go func() {
		time.Sleep(20 * time.Millisecond)
		fake.mu.Lock()
		for _, j := range fake.jobs {
			exit := 0
			j.Status = dispatch.StatusSuccess
			j.ExitCode = &exit
			j.Node = "mac-1"
			finished := time.Now()
			j.FinishedAt = &finished
		}
		fake.logsByJobID["job-1"] = "step 1\nstep 2\nOK"
		fake.mu.Unlock()
	}()

	wait := true
	_, res, err := h.run(t.Context(), nil, runArgs{Pool: "linux", Script: "echo hi", Wait: &wait, TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != string(dispatch.StatusSuccess) {
		t.Errorf("Status = %q, want %q", res.Status, dispatch.StatusSuccess)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", res.ExitCode)
	}
	if res.Node != "mac-1" {
		t.Errorf("Node = %q, want mac-1", res.Node)
	}
	if !strings.Contains(res.LogTail, "OK") {
		t.Errorf("LogTail = %q, want it to contain OK", res.LogTail)
	}
	if res.TimedOutWaiting {
		t.Error("TimedOutWaiting = true, want false (job reached success before timeout)")
	}
}

func TestRunToolReportsTimeoutWithoutError(t *testing.T) {
	fake := newFakeGroveAPI() // job stays "running" forever
	h := newTestHandlers(t, fake)

	oldInterval := pollInterval
	pollInterval = 5 * time.Millisecond
	defer func() { pollInterval = oldInterval }()
	oldMaxWait := maxWait
	_ = oldMaxWait

	wait := true
	// TimeoutSeconds must be small and positive so awaitJob's deadline is reached quickly
	// instead of falling back to maxWait.
	_, res, err := h.run(t.Context(), nil, runArgs{Pool: "linux", Script: "sleep 999", Wait: &wait, TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.TimedOutWaiting {
		t.Error("TimedOutWaiting = false, want true (job never reached a terminal state)")
	}
	if res.Status != string(dispatch.StatusRunning) {
		t.Errorf("Status = %q, want running", res.Status)
	}
}

func TestRunToolRequiresPoolAndScript(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	if _, _, err := h.run(t.Context(), nil, runArgs{Script: "x"}); err == nil {
		t.Error("expected error when pool is empty")
	}
	if _, _, err := h.run(t.Context(), nil, runArgs{Pool: "linux"}); err == nil {
		t.Error("expected error when script is empty")
	}
}

func TestJobStatusToolRequiresJobID(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	if _, _, err := h.jobStatus(t.Context(), nil, jobIDArgs{}); err == nil {
		t.Error("expected error for empty job_id")
	}
}

func TestJobStatusToolReturnsJob(t *testing.T) {
	fake := newFakeGroveAPI()
	exit := 7
	fake.jobs["job-9"] = &dispatch.Job{ID: "job-9", Status: dispatch.StatusFailed, ExitCode: &exit}
	h := newTestHandlers(t, fake)

	_, job, err := h.jobStatus(t.Context(), nil, jobIDArgs{JobID: "job-9"})
	if err != nil {
		t.Fatalf("jobStatus: %v", err)
	}
	if job.Status != dispatch.StatusFailed || job.ExitCode == nil || *job.ExitCode != 7 {
		t.Errorf("job = %+v, want status=failed exitCode=7", job)
	}
}

func TestJobStatusToolNotFound(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	if _, _, err := h.jobStatus(t.Context(), nil, jobIDArgs{JobID: "nope"}); err == nil {
		t.Error("expected error for missing job")
	}
}

func TestJobLogsToolTailsOutput(t *testing.T) {
	fake := newFakeGroveAPI()
	var lines []string
	for i := 1; i <= 500; i++ {
		lines = append(lines, fmt.Sprintf("line-%d", i))
	}
	fake.logsByJobID["job-5"] = strings.Join(lines, "\n")
	h := newTestHandlers(t, fake)

	_, res, err := h.jobLogs(t.Context(), nil, jobLogsArgs{JobID: "job-5", TailLines: 3})
	if err != nil {
		t.Fatalf("jobLogs: %v", err)
	}
	want := "line-498\nline-499\nline-500"
	if res.LogTail != want {
		t.Errorf("LogTail = %q, want %q", res.LogTail, want)
	}
}

func TestJobLogsToolDefaultsTailLines(t *testing.T) {
	fake := newFakeGroveAPI()
	var lines []string
	for i := 1; i <= 500; i++ {
		lines = append(lines, fmt.Sprintf("line-%d", i))
	}
	fake.logsByJobID["job-5"] = strings.Join(lines, "\n")
	h := newTestHandlers(t, fake)

	_, res, err := h.jobLogs(t.Context(), nil, jobLogsArgs{JobID: "job-5"})
	if err != nil {
		t.Fatalf("jobLogs: %v", err)
	}
	if got := strings.Count(res.LogTail, "\n") + 1; got != 200 {
		t.Errorf("got %d lines, want default of 200", got)
	}
}

func TestJobCancelTool(t *testing.T) {
	fake := newFakeGroveAPI()
	fake.jobs["job-1"] = &dispatch.Job{ID: "job-1", Status: dispatch.StatusRunning}
	h := newTestHandlers(t, fake)

	_, res, err := h.jobCancel(t.Context(), nil, jobIDArgs{JobID: "job-1"})
	if err != nil {
		t.Fatalf("jobCancel: %v", err)
	}
	if !res.Cancelled {
		t.Errorf("Cancelled = false, want true")
	}
	if fake.jobs["job-1"].Status != dispatch.StatusCanceled {
		t.Errorf("job status = %s, want canceled", fake.jobs["job-1"].Status)
	}
}

func TestRecycleVMTool(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	_, res, err := h.recycleVM(t.Context(), nil, nameArgs{Name: "linux-mac-1-0"})
	if err != nil {
		t.Fatalf("recycleVM: %v", err)
	}
	if !res.Recycled {
		t.Error("Recycled = false, want true")
	}
	if len(fake.recycled) != 1 || fake.recycled[0] != "linux-mac-1-0" {
		t.Errorf("recycled = %v, want [linux-mac-1-0]", fake.recycled)
	}
}

func TestPauseWorkerTool(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	_, res, err := h.pauseWorker(t.Context(), nil, pauseWorkerArgs{Name: "mac-1", Paused: true})
	if err != nil {
		t.Fatalf("pauseWorker: %v", err)
	}
	if !res.Paused {
		t.Error("Paused = false, want true")
	}
	if paused, ok := fake.pausedWorkers["mac-1"]; !ok || !paused {
		t.Errorf("pausedWorkers[mac-1] = %v, %v, want true, true", paused, ok)
	}

	_, res, err = h.pauseWorker(t.Context(), nil, pauseWorkerArgs{Name: "mac-1", Paused: false})
	if err != nil {
		t.Fatalf("pauseWorker (resume): %v", err)
	}
	if res.Paused {
		t.Error("Paused = true after resume, want false")
	}
}

func TestAPIClientReportsUnreachableServer(t *testing.T) {
	client, err := newAPIClient("http://127.0.0.1:1", "") // nothing listens here
	if err != nil {
		t.Fatalf("newAPIClient: %v", err)
	}
	h := &handlers{client: client}
	_, _, err = h.fleet(t.Context(), nil, fleetArgs{})
	if err == nil {
		t.Fatal("expected an error calling an unreachable server")
	}
	if !strings.Contains(err.Error(), "grove serve") {
		t.Errorf("error = %q, want an actionable message mentioning `grove serve`", err.Error())
	}
}

func TestAPIClientReportsAuthFailure(t *testing.T) {
	fake := newFakeGroveAPI()
	fake.token = "correct-token"
	srv := fake.server()
	defer srv.Close()

	client, err := newAPIClient(srv.URL, "wrong-token")
	if err != nil {
		t.Fatalf("newAPIClient: %v", err)
	}
	h := &handlers{client: client}
	_, _, err = h.fleet(t.Context(), nil, fleetArgs{})
	if err == nil {
		t.Fatal("expected an auth error")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q, want it to mention the token", err.Error())
	}
}

func TestNewAPIClientRejectsEmptyURL(t *testing.T) {
	if _, err := newAPIClient("", "tok"); err == nil {
		t.Error("expected error for empty base URL")
	}
}

func TestReadFleetResource(t *testing.T) {
	fake := newFakeGroveAPI()
	fake.workers = []orchard.Worker{{Name: "mac-1"}}
	h := newTestHandlers(t, fake)

	result, err := h.readFleetResource(t.Context(), nil)
	if err != nil {
		t.Fatalf("readFleetResource: %v", err)
	}
	if len(result.Contents) != 1 || result.Contents[0].URI != fleetResourceURI {
		t.Fatalf("unexpected contents: %+v", result.Contents)
	}
	var summary FleetSummary
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &summary); err != nil {
		t.Fatalf("resource body is not valid FleetSummary JSON: %v", err)
	}
	if summary.Workers.Total != 1 {
		t.Errorf("Workers.Total = %d, want 1", summary.Workers.Total)
	}
}

func TestReadRecentJobsResource(t *testing.T) {
	fake := newFakeGroveAPI()
	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	fake.jobs["old"] = &dispatch.Job{ID: "old", SubmittedAt: older}
	fake.jobs["new"] = &dispatch.Job{ID: "new", SubmittedAt: newer}
	h := newTestHandlers(t, fake)

	result, err := h.readRecentJobsResource(t.Context(), nil)
	if err != nil {
		t.Fatalf("readRecentJobsResource: %v", err)
	}
	var body struct {
		Jobs []dispatch.Job `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(body.Jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(body.Jobs))
	}
	if body.Jobs[0].ID != "new" {
		t.Errorf("Jobs[0].ID = %q, want %q (newest first)", body.Jobs[0].ID, "new")
	}
}

func TestCIPromptDefinition(t *testing.T) {
	prompt := ciPrompt()
	if prompt.Name != "grove_ci" {
		t.Fatalf("prompt name = %q, want grove_ci", prompt.Name)
	}
	foundPool := false
	for _, arg := range prompt.Arguments {
		if arg.Name == "pool" {
			foundPool = true
			if !arg.Required {
				t.Error("pool argument should be required")
			}
		}
	}
	if !foundPool {
		t.Error("expected a 'pool' prompt argument")
	}
}

func TestCIPromptRendersToolCallInstructions(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	req := &mcpsdk.GetPromptRequest{Params: &mcpsdk.GetPromptParams{
		Arguments: map[string]string{"pool": "linux", "repo": "git@example.com/x.git"},
	}}
	result, err := h.getCIPrompt(t.Context(), req)
	if err != nil {
		t.Fatalf("getCIPrompt: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(result.Messages))
	}
	text, ok := result.Messages[0].Content.(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("Content is %T, want *mcpsdk.TextContent", result.Messages[0].Content)
	}
	if !strings.Contains(text.Text, "linux") || !strings.Contains(text.Text, "git@example.com/x.git") || !strings.Contains(text.Text, "grove_run") {
		t.Errorf("prompt text = %q, want it to reference pool, repo and grove_run", text.Text)
	}
}

func TestCIPromptRequiresPool(t *testing.T) {
	fake := newFakeGroveAPI()
	h := newTestHandlers(t, fake)

	req := &mcpsdk.GetPromptRequest{Params: &mcpsdk.GetPromptParams{Arguments: map[string]string{}}}
	if _, err := h.getCIPrompt(t.Context(), req); err == nil {
		t.Error("expected error when pool argument is missing")
	}
}

func TestNewServerBuildsWithoutError(t *testing.T) {
	fake := newFakeGroveAPI()
	srv := fake.server()
	defer srv.Close()

	client, err := newAPIClient(srv.URL, fake.token)
	if err != nil {
		t.Fatalf("newAPIClient: %v", err)
	}
	s := newServerWithClient(client, "test-version")
	if s == nil {
		t.Fatal("newServerWithClient returned nil")
	}
}

func boolPtr(b bool) *bool { return &b }
