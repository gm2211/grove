package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/nomad"
)

// blockingFollowReader mimics what internal/nomad/client.go's real Logs implementation does for
// follow=true against an allocation whose task has already exited: Nomad's AllocFS Logs API keeps
// the connection open past task completion, so the reader hands back whatever was already
// captured and then blocks — never EOFing — until ctx is canceled, at which point Read returns
// ctx.Err() (mirroring the real client's ctx.Done() select case). Used to reproduce/verify the fix
// for the "curl .../logs?follow=1 hangs forever on a finished job" defect.
type blockingFollowReader struct {
	ctx  context.Context
	data []byte

	mu   sync.Mutex
	sent bool
}

func (r *blockingFollowReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if !r.sent {
		r.sent = true
		n := copy(p, r.data)
		r.mu.Unlock()
		if n > 0 {
			return n, nil
		}
	} else {
		r.mu.Unlock()
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *blockingFollowReader) Close() error { return nil }

func ptrInt(n int) *int { return &n }

func ptrTime(t time.Time) *time.Time { return &t }

func newTestService(t *testing.T, nc *fakeNomad) Service {
	t.Helper()
	svc, err := New(nc, Options{StorePath: filepath.Join(t.TempDir(), "jobs.json"), StatusTTL: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func TestSubmit_Validation(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	cases := []struct {
		name string
		req  JobRequest
	}{
		{"missing kind", JobRequest{Pool: "linux", Script: "true"}},
		{"unknown kind", JobRequest{Kind: "bogus", Pool: "linux", Script: "true"}},
		{"missing pool", JobRequest{Kind: KindShell, Script: "true"}},
		{"missing script", JobRequest{Kind: KindShell, Pool: "linux"}},
		{"missing repo for build", JobRequest{Kind: KindBuild, Pool: "linux", Script: "true"}},
		{"missing repo for agent", JobRequest{Kind: KindAgent, Pool: "linux", Script: "true"}},
		{"sub-second timeout", JobRequest{Kind: KindShell, Pool: "linux", Script: "true", Timeout: Duration(500 * time.Millisecond)}},
		{"sub-second timeout just under 1s", JobRequest{Kind: KindShell, Pool: "linux", Script: "true", Timeout: Duration(999 * time.Millisecond)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := svc.Submit(context.Background(), c.req); err == nil {
				t.Fatal("expected a validation error, got nil")
			}
		})
	}
	if len(nc.dispatchCalls) != 0 {
		t.Fatalf("Dispatch should not have been called, got %d calls", len(nc.dispatchCalls))
	}
}

// TestSubmit_RejectsSubSecondTimeout pins the exact error message a 0 < timeout < 1s request gets,
// and confirms a zero timeout (no timeout requested) and a timeout of exactly 1s are both fine —
// only the open sub-second gap is rejected.
func TestSubmit_RejectsSubSecondTimeout(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	_, _, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindShell, Pool: "linux", Script: "true", Timeout: Duration(100 * time.Millisecond),
	})
	if err == nil {
		t.Fatal("expected an error for a sub-second timeout")
	}
	wantMsg := `timeout must be at least 1s (send a duration string like "30m" or a number of seconds)`
	if !strings.Contains(err.Error(), wantMsg) {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), wantMsg)
	}

	if _, _, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindShell, Pool: "linux", Script: "true", Timeout: Duration(0),
	}); err != nil {
		t.Fatalf("zero timeout (no timeout) should be accepted: %v", err)
	}
	if _, _, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindShell, Pool: "linux", Script: "true", Timeout: Duration(time.Second),
	}); err != nil {
		t.Fatalf("exactly 1s timeout should be accepted: %v", err)
	}
}

func TestSubmit_Success(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	req := JobRequest{
		Kind:      KindBuild,
		Pool:      "linux",
		Repo:      "https://example.com/repo.git",
		Ref:       "main",
		Script:    "make test",
		Env:       map[string]string{"FOO": "bar"},
		Timeout:   Duration(30 * time.Second),
		Meta:      map[string]string{"bead": "123", "image": "golang:1.27"},
		Requester: "cli",
	}
	job, _, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job.Status != StatusPending {
		t.Errorf("status = %v, want pending", job.Status)
	}
	if job.ID == "" {
		t.Fatal("expected a non-empty job id")
	}

	if len(nc.dispatchCalls) != 1 {
		t.Fatalf("expected 1 Dispatch call, got %d", len(nc.dispatchCalls))
	}
	call := nc.dispatchCalls[0]
	if call.jobName != "grove-build-linux" {
		t.Errorf("job name = %q, want grove-build-linux", call.jobName)
	}
	if string(call.payload) != "make test" {
		t.Errorf("payload = %q, want %q", call.payload, "make test")
	}
	if call.meta["repo"] != req.Repo {
		t.Errorf("meta[repo] = %q", call.meta["repo"])
	}
	if call.meta["ref"] != "main" {
		t.Errorf("meta[ref] = %q", call.meta["ref"])
	}
	if call.meta["requester"] != "cli" {
		t.Errorf("meta[requester] = %q", call.meta["requester"])
	}
	if call.meta["timeout_seconds"] != "30" {
		t.Errorf("meta[timeout_seconds] = %q, want 30", call.meta["timeout_seconds"])
	}
	if call.meta["image"] != "golang:1.27" {
		t.Errorf("meta[image] = %q", call.meta["image"])
	}
	if !strings.HasPrefix(call.meta["artifact_prefix"], "jobs/") || !strings.HasSuffix(call.meta["artifact_prefix"], "/") {
		t.Errorf("artifact_prefix = %q, want jobs/<id>/", call.meta["artifact_prefix"])
	}

	var env map[string]string
	if err := json.Unmarshal([]byte(call.meta["env_json"]), &env); err != nil {
		t.Fatalf("env_json: %v", err)
	}
	if env["FOO"] != "bar" {
		t.Errorf("env_json[FOO] = %q", env["FOO"])
	}

	var meta map[string]string
	if err := json.Unmarshal([]byte(call.meta["grove_meta_json"]), &meta); err != nil {
		t.Fatalf("grove_meta_json: %v", err)
	}
	if meta["bead"] != "123" {
		t.Errorf("grove_meta_json[bead] = %q", meta["bead"])
	}
}

func TestSubmit_ShellDoesNotRequireRepo(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)
	if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "echo hi"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

func TestSubmit_IdempotencyKey_ReturnsExistingWithoutRedispatch(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	first, created1, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindShell, Pool: "linux", Script: "true", IdempotencyKey: "argos:t1:r1",
	})
	if err != nil {
		t.Fatalf("Submit (first): %v", err)
	}
	if !created1 {
		t.Errorf("created (first) = false, want true")
	}

	second, created2, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindShell, Pool: "linux", Script: "false", IdempotencyKey: "argos:t1:r1",
	})
	if err != nil {
		t.Fatalf("Submit (second): %v", err)
	}
	if created2 {
		t.Errorf("created (second) = true, want false")
	}
	if second.ID != first.ID {
		t.Errorf("second.ID = %q, want %q (same job)", second.ID, first.ID)
	}
	if len(nc.dispatchCalls) != 1 {
		t.Fatalf("expected 1 Dispatch call (no re-dispatch), got %d", len(nc.dispatchCalls))
	}
}

func TestSubmit_DifferentOrEmptyIdempotencyKeys_BothDispatch(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	if _, created, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true", IdempotencyKey: "key-a"}); err != nil || !created {
		t.Fatalf("Submit (key-a): created=%v err=%v", created, err)
	}
	if _, created, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true", IdempotencyKey: "key-b"}); err != nil || !created {
		t.Fatalf("Submit (key-b): created=%v err=%v", created, err)
	}
	if _, created, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"}); err != nil || !created {
		t.Fatalf("Submit (no key, 1st): created=%v err=%v", created, err)
	}
	if _, created, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"}); err != nil || !created {
		t.Fatalf("Submit (no key, 2nd): created=%v err=%v", created, err)
	}
	if len(nc.dispatchCalls) != 4 {
		t.Fatalf("expected 4 Dispatch calls, got %d", len(nc.dispatchCalls))
	}
}

func TestGet_NotFound(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)
	_, err := svc.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGet_ReconcilesFromAllocations(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	exit := 0
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {
			{ID: "alloc-1", JobID: dispatchedJobID, ClientStatus: "complete", ExitCode: &exit, NodeName: "node-1", CreatedAt: time.Now()},
		},
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusSuccess {
		t.Errorf("status = %v, want success", got.Status)
	}
	if got.AllocID != "alloc-1" {
		t.Errorf("allocID = %q, want alloc-1", got.AllocID)
	}
	if got.Node != "node-1" {
		t.Errorf("node = %q, want node-1", got.Node)
	}
}

func TestGet_FailedExitCodeMapsToFailed(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "false"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	exit := 1
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "complete", ExitCode: &exit, CreatedAt: time.Now()}},
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %v, want failed", got.Status)
	}
}

func TestGet_ExitCode124MapsToTimedOut(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "sleep 999"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	exit := 124
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "complete", ExitCode: &exit, CreatedAt: time.Now()}},
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.TimedOut {
		t.Errorf("TimedOut = false, want true for exit code 124")
	}
}

func TestGet_SignalPopulatesHumanName(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	exit := 137
	sig := 9
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "complete", ExitCode: &exit, Signal: &sig, CreatedAt: time.Now()}},
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Signal == nil || *got.Signal != "SIGKILL" {
		t.Errorf("Signal = %v, want SIGKILL", got.Signal)
	}
}

func TestGet_LostAllocationSetsFailureReason(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "lost", CreatedAt: time.Now()}},
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusLost {
		t.Fatalf("status = %v, want lost", got.Status)
	}
	if got.FailureReason == nil || *got.FailureReason == "" {
		t.Errorf("FailureReason = %v, want non-empty for a lost allocation", got.FailureReason)
	}
}

func TestGet_InfraFaultSetsFailureReasonFromNomadEvent(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "failed", FailureReason: "failed to pull image", CreatedAt: time.Now()}},
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FailureReason == nil || *got.FailureReason != "failed to pull image" {
		t.Errorf("FailureReason = %v, want %q", got.FailureReason, "failed to pull image")
	}
}

func TestGet_PopulatesPlacementFromNode(t *testing.T) {
	nc := &fakeNomad{
		nodesByID: map[string]nomad.Node{
			"node-1": {ID: "node-1", Name: "linux-mac1-0", Meta: map[string]string{"vm": "linux-mac1-0", "host": "mac1"}},
		},
	}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	exit := 0
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", NodeID: "node-1", ClientStatus: "complete", ExitCode: &exit, CreatedAt: time.Now()}},
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Placement == nil {
		t.Fatal("Placement = nil, want populated")
	}
	if got.Placement.AllocID != "alloc-1" || got.Placement.NodeID != "node-1" {
		t.Errorf("Placement = %+v, want AllocID=alloc-1 NodeID=node-1", got.Placement)
	}
	if got.Placement.VMID != "linux-mac1-0" || got.Placement.WorkerID != "mac1" {
		t.Errorf("Placement = %+v, want VMID=linux-mac1-0 WorkerID=mac1", got.Placement)
	}
}

func TestList_ReturnsAllJobs(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)
	for i := 0; i < 3; i++ {
		if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	jobs, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("len(jobs) = %d, want 3", len(jobs))
	}
}

func TestCancel_MarksCanceledAndStopsQuerying(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := svc.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(nc.stopCalls) != 1 {
		t.Fatalf("expected 1 StopJob call, got %d", len(nc.stopCalls))
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCanceled {
		t.Errorf("status = %v, want canceled", got.Status)
	}

	// A terminal (canceled) job must never be re-queried against Nomad.
	nc.allocationsErr = errors.New("must not be called for a terminal job")
	if _, err := svc.Get(context.Background(), job.ID); err != nil {
		t.Fatalf("Get after cancel unexpectedly queried nomad: %v", err)
	}
}

func TestCancel_NotFound(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)
	if err := svc.Cancel(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStore_PersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	nc := &fakeNomad{}
	svc1, err := New(nc, Options{StorePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	job, _, err := svc1.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	svc2, err := New(nc, Options{StorePath: path})
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	got, err := svc2.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if got.ID != job.ID {
		t.Errorf("id after restart = %q, want %q", got.ID, job.ID)
	}
}

func TestLogs_MergesStdoutAndStderr(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "running", CreatedAt: time.Now()}},
	}
	nc.logsFunc = func(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
		if stream == "stdout" {
			return io.NopCloser(strings.NewReader("out-line\n")), nil
		}
		return io.NopCloser(strings.NewReader("err-line\n")), nil
	}

	rc, err := svc.Logs(context.Background(), job.ID, false)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	combined := string(data)
	if !strings.Contains(combined, "out-line") || !strings.Contains(combined, "err-line") {
		t.Errorf("combined logs missing content: %q", combined)
	}
}

func TestLogs_NoAllocationYet(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := svc.Logs(context.Background(), job.ID, false); !errors.Is(err, ErrNoAllocationYet) {
		t.Fatalf("Logs err = %v, want ErrNoAllocationYet", err)
	}
}

func TestLogLines_TagsStreamPerLine(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "running", CreatedAt: time.Now()}},
	}
	nc.logsFunc = func(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
		if stream == "stdout" {
			return io.NopCloser(strings.NewReader("out-1\nout-2\n")), nil
		}
		return io.NopCloser(strings.NewReader("err-1\n")), nil
	}

	lines, err := svc.LogLines(context.Background(), job.ID, false)
	if err != nil {
		t.Fatalf("LogLines: %v", err)
	}

	var stdoutLines, stderrLines []string
	for ln := range lines {
		switch ln.Stream {
		case "stdout":
			stdoutLines = append(stdoutLines, ln.Line)
		case "stderr":
			stderrLines = append(stderrLines, ln.Line)
		default:
			t.Errorf("unexpected stream %q for line %q", ln.Stream, ln.Line)
		}
		if ln.Time.IsZero() {
			t.Errorf("line %q has zero Time", ln.Line)
		}
	}

	if len(stdoutLines) != 2 || stdoutLines[0] != "out-1" || stdoutLines[1] != "out-2" {
		t.Errorf("stdoutLines = %v, want [out-1 out-2]", stdoutLines)
	}
	if len(stderrLines) != 1 || stderrLines[0] != "err-1" {
		t.Errorf("stderrLines = %v, want [err-1]", stderrLines)
	}
}

func TestGet_PopulatesArtifactsOnTerminalStatus(t *testing.T) {
	nc := &fakeNomad{}
	fa := &fakeArtifacts{
		objectsByPrefix: map[string][]artifacts.Object{},
	}
	svc, err := New(nc, Options{
		StorePath: filepath.Join(t.TempDir(), "jobs.json"),
		StatusTTL: time.Millisecond,
		Artifacts: fa,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://example.com/repo.git", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	prefix := artifactPrefix(job.ID)
	fa.objectsByPrefix[prefix] = []artifacts.Object{
		{Path: "out.bin", Size: 42, ContentType: "application/octet-stream"},
	}

	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	exit := 0
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "complete", ExitCode: &exit, CreatedAt: time.Now()}},
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want 1 entry", got.Artifacts)
	}
	art := got.Artifacts[0]
	if art.Path != "out.bin" || art.Size != 42 || art.ContentType != "application/octet-stream" {
		t.Errorf("artifact = %+v", art)
	}
	wantURL := "/api/v1/jobs/" + job.ID + "/artifacts/out.bin"
	if art.URL != wantURL {
		t.Errorf("artifact URL = %q, want %q", art.URL, wantURL)
	}
	if len(fa.listCalls) != 1 || fa.listCalls[0] != prefix {
		t.Errorf("listCalls = %v, want [%s]", fa.listCalls, prefix)
	}

	// A second Get shouldn't re-list — Artifacts are already populated.
	if _, err := svc.Get(context.Background(), job.ID); err != nil {
		t.Fatalf("Get (second): %v", err)
	}
	if len(fa.listCalls) != 1 {
		t.Errorf("listCalls after second Get = %d, want still 1 (terminal job, no re-list)", len(fa.listCalls))
	}
}

func TestLogLines_NoAllocationYet(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := svc.LogLines(context.Background(), job.ID, false); !errors.Is(err, ErrNoAllocationYet) {
		t.Fatalf("LogLines err = %v, want ErrNoAllocationYet", err)
	}
}

// newTestServiceWithOpts is like newTestService but lets the caller tweak Options (e.g.
// AllocationPollInterval, Pools) before New is called.
func newTestServiceWithOpts(t *testing.T, nc *fakeNomad, opts Options) Service {
	t.Helper()
	opts.StorePath = filepath.Join(t.TempDir(), "jobs.json")
	if opts.StatusTTL <= 0 {
		opts.StatusTTL = time.Millisecond
	}
	svc, err := New(nc, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// TestLogs_FollowWaitsForAllocation reproduces the live-run defect: `grove dispatch --follow`
// used to 502 immediately ("job has no allocation yet") because Logs/LogLines never waited for
// Nomad to actually place the job. With follow=true, Logs must instead poll until an allocation
// shows up (here, ListAllocations returns empty for the first 3 calls, simulating a job that
// takes a few polls to get placed) and then stream normally.
func TestLogs_FollowWaitsForAllocation(t *testing.T) {
	nc := &fakeNomad{allocationsEmptyCalls: 3}
	svc := newTestServiceWithOpts(t, nc, Options{AllocationPollInterval: 5 * time.Millisecond})

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "running", CreatedAt: time.Now()}},
	}
	nc.logsFunc = func(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
		if stream == "stdout" {
			return io.NopCloser(strings.NewReader("out-line\n")), nil
		}
		return io.NopCloser(strings.NewReader("")), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rc, err := svc.Logs(ctx, job.ID, true)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(data), "out-line") {
		t.Errorf("logs = %q, want to contain out-line", string(data))
	}
	if nc.allocationsCallCount < 4 {
		t.Errorf("allocationsCallCount = %d, want >= 4 (3 empty + 1 hit)", nc.allocationsCallCount)
	}
}

// TestLogs_FollowGivesUpAtContextDeadline asserts that a follow=true request bounded by a short
// ctx deadline (mirroring the HTTP handler's `?wait=` timeout) gives up with ErrNoAllocationYet
// instead of blocking forever when an allocation never shows up.
func TestLogs_FollowGivesUpAtContextDeadline(t *testing.T) {
	nc := &fakeNomad{} // ListAllocations always returns empty — no allocation ever appears.
	svc := newTestServiceWithOpts(t, nc, Options{AllocationPollInterval: 5 * time.Millisecond})

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = svc.Logs(ctx, job.ID, true)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrNoAllocationYet) {
		t.Fatalf("Logs err = %v, want ErrNoAllocationYet", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Logs took %s to give up, want well under its bounding context's budget", elapsed)
	}
}

// TestLogs_FollowOnAlreadyTerminalJobDoesNotHang is the direct regression test for the live-run
// defect: `curl .../jobs/<id>/logs?follow=1` hung forever (no response headers for 60s+) against a
// job that was already `failed` with an allocation, because Nomad's AllocFS Logs API keeps a
// follow=true stream open past task completion. Logs must recognize the job is already terminal
// and fetch the captured log the same way a one-shot request would (nomad.Client.Logs called with
// follow=false), never handing the already-terminal case to a follow=true nomad call at all.
func TestLogs_FollowOnAlreadyTerminalJobDoesNotHang(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{
		LogTerminalPollInterval: 10 * time.Millisecond,
		LogStreamGracePeriod:    10 * time.Millisecond,
	})

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{
			ID: "alloc-1", ClientStatus: "failed", ExitCode: ptrInt(127),
			CreatedAt: time.Now(), FinishedAt: ptrTime(time.Now()),
		}},
	}

	var sawFollow bool
	nc.logsFunc = func(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
		if follow {
			sawFollow = true
			// If Logs actually used this (instead of downgrading to a one-shot fetch), the test
			// would hang until its own context deadline — exactly the live defect.
			return &blockingFollowReader{ctx: ctx}, nil
		}
		if stream == "stdout" {
			return io.NopCloser(strings.NewReader("boom\n")), nil
		}
		return io.NopCloser(strings.NewReader("")), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	rc, err := svc.Logs(ctx, job.ID, true)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Logs on an already-terminal job took %s, want near-instant (no follow wait)", elapsed)
	}
	if sawFollow {
		t.Error("Logs called nomad.Logs with follow=true for an already-terminal job — want it downgraded to a one-shot fetch")
	}
	if !strings.Contains(string(data), "boom") {
		t.Errorf("logs = %q, want to contain boom", string(data))
	}
}

// TestLogLines_FollowAndNoFollowAgreeOnTerminalJob is the regression test for the reported
// "follow=1 may return only stdout" defect: for an already-terminal job with both stdout and
// stderr non-empty, follow=true (which streamContext/isTerminal downgrades to a one-shot,
// follow=false fetch against Nomad — see TestLogs_FollowOnAlreadyTerminalJobDoesNotHang) and
// follow=false must return the exact same merged set of lines. Verified live against a real grove
// server that the actual root cause was one layer deeper (internal/nomad/client.go's Logs — see
// TestStreamLogs_FramesDrainedBeforeConcurrentErrSignal): both follow=true and follow=false
// nondeterministically dropped lines there, not just follow=true relative to follow=false. This
// test guards the dispatch-layer contract (both modes agree) regardless of which layer a future
// regression reintroduces a discrepancy in.
func TestLogLines_FollowAndNoFollowAgreeOnTerminalJob(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{
		LogTerminalPollInterval: 10 * time.Millisecond,
		LogStreamGracePeriod:    10 * time.Millisecond,
	})

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{
			ID: "alloc-1", ClientStatus: "failed", ExitCode: ptrInt(1),
			CreatedAt: time.Now(), FinishedAt: ptrTime(time.Now()),
		}},
	}
	nc.logsFunc = func(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
		if stream == "stdout" {
			return io.NopCloser(strings.NewReader("out1\nout2\n")), nil
		}
		return io.NopCloser(strings.NewReader("err1\nerr2\n")), nil
	}

	collect := func(follow bool) []string {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		lines, err := svc.LogLines(ctx, job.ID, follow)
		if err != nil {
			t.Fatalf("LogLines(follow=%v): %v", follow, err)
		}
		var got []string
		for ln := range lines {
			got = append(got, ln.Stream+":"+ln.Line)
		}
		sort.Strings(got) // merge order across the two streams is best-effort; compare as sets
		return got
	}

	want := []string{"stderr:err1", "stderr:err2", "stdout:out1", "stdout:out2"}
	if got := collect(false); !slicesEqual(got, want) {
		t.Fatalf("follow=false lines = %v, want %v", got, want)
	}
	if got := collect(true); !slicesEqual(got, want) {
		t.Fatalf("follow=true lines = %v, want %v", got, want)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLogs_FollowEndsPromptlyWhenJobBecomesTerminal covers the other half of the same defect: a
// job that's still running when the follow request starts but finishes while the stream is open.
// nomad.Logs is (correctly) called with follow=true here and returns a reader that never EOFs —
// Logs must notice (via watchForTerminal polling job status) once the allocation flips to a
// terminal status and sever the stream within a few seconds, not hang for the rest of the
// request's context budget.
func TestLogs_FollowEndsPromptlyWhenJobBecomesTerminal(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{
		AllocationPollInterval:  5 * time.Millisecond,
		LogTerminalPollInterval: 10 * time.Millisecond,
		LogStreamGracePeriod:    20 * time.Millisecond,
	})

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "running", CreatedAt: time.Now()}},
	}

	nc.logsFunc = func(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
		if !follow {
			return io.NopCloser(strings.NewReader("")), nil
		}
		var data []byte
		if stream == "stdout" {
			data = []byte("out-line\n")
		}
		return &blockingFollowReader{ctx: ctx, data: data}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rc, err := svc.Logs(ctx, job.ID, true)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer rc.Close()

	// Flip the allocation to terminal shortly after the stream opens, mimicking the job finishing
	// mid-follow.
	go func() {
		time.Sleep(30 * time.Millisecond)
		nc.mu.Lock()
		nc.allocationsByJob[dispatchedJobID] = []nomad.Allocation{{
			ID: "alloc-1", ClientStatus: "complete", ExitCode: ptrInt(0),
			CreatedAt: time.Now(), FinishedAt: ptrTime(time.Now()),
		}}
		nc.mu.Unlock()
	}()

	start := time.Now()
	data, err := io.ReadAll(rc)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(data), "out-line") {
		t.Errorf("logs = %q, want to contain out-line", string(data))
	}
	if elapsed > 3*time.Second {
		t.Errorf("Logs took %s to end after its job went terminal, want well under a few seconds", elapsed)
	}
}

// TestLogLines_FollowEndsPromptlyWhenJobBecomesTerminal is the NDJSON-mode (LogLines) counterpart
// of TestLogs_FollowEndsPromptlyWhenJobBecomesTerminal.
func TestLogLines_FollowEndsPromptlyWhenJobBecomesTerminal(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{
		AllocationPollInterval:  5 * time.Millisecond,
		LogTerminalPollInterval: 10 * time.Millisecond,
		LogStreamGracePeriod:    20 * time.Millisecond,
	})

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "running", CreatedAt: time.Now()}},
	}

	nc.logsFunc = func(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
		if !follow {
			return io.NopCloser(strings.NewReader("")), nil
		}
		var data []byte
		if stream == "stdout" {
			data = []byte("out-line\n")
		}
		return &blockingFollowReader{ctx: ctx, data: data}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, err := svc.LogLines(ctx, job.ID, true)
	if err != nil {
		t.Fatalf("LogLines: %v", err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		nc.mu.Lock()
		nc.allocationsByJob[dispatchedJobID] = []nomad.Allocation{{
			ID: "alloc-1", ClientStatus: "complete", ExitCode: ptrInt(0),
			CreatedAt: time.Now(), FinishedAt: ptrTime(time.Now()),
		}}
		nc.mu.Unlock()
	}()

	start := time.Now()
	var got []LogLine
	done := make(chan struct{})
	go func() {
		for ln := range lines {
			got = append(got, ln)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("LogLines channel did not close within 3s of its job going terminal")
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Errorf("LogLines took %s to end after its job went terminal, want well under a few seconds", elapsed)
	}
	found := false
	for _, ln := range got {
		if ln.Stream == "stdout" && ln.Line == "out-line" {
			found = true
		}
	}
	if !found {
		t.Errorf("lines = %+v, want a stdout \"out-line\" entry", got)
	}
}

func TestSubmit_RejectsResourceHintExceedingPoolDefaults(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{Pools: []PoolConfig{{Name: "macos", CPU: 500, Memory: 1024}}})

	_, _, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindShell, Pool: "macos", Script: "true",
		Resources: &ResourceHint{CPU: 4000},
	})
	if err == nil {
		t.Fatal("expected an error for a resources hint exceeding the pool's job CPU default")
	}
	if len(nc.dispatchCalls) != 0 {
		t.Errorf("dispatchCalls = %d, want 0 — an over-budget request should never be dispatched", len(nc.dispatchCalls))
	}
}

func TestSubmit_AcceptsResourceHintWithinPoolDefaults(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{Pools: []PoolConfig{{Name: "macos", CPU: 500, Memory: 1024}}})

	job, _, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindShell, Pool: "macos", Script: "true",
		Resources: &ResourceHint{CPU: 250, Memory: 512},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job == nil {
		t.Fatal("expected a job")
	}
}

// TestGet_OrphanedNomadJobMarksLost reproduces the live defect: job b1a0ea873e6e3ad3 sat pending
// forever because the dev Nomad agent it was dispatched against restarted with its data dir wiped
// — its dispatched Nomad job no longer exists, so ListAllocations 404s. Get must recognize that
// (via errors.Is(err, nomad.ErrNotFound)) and mark the job StatusLost with an explanatory
// FailureReason + FinishedAt, rather than surfacing the 404 as an opaque error or leaving the job
// stuck pending.
func TestGet_OrphanedNomadJobMarksLost(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	nc.allocationsErr = fmt.Errorf("job not found: %w", nomad.ErrNotFound)

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusLost {
		t.Fatalf("status = %v, want lost", got.Status)
	}
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "not found") {
		t.Errorf("FailureReason = %v, want it to mention the job wasn't found", got.FailureReason)
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt = nil, want set once a job is marked lost")
	}

	// The job is now terminal — a further Get must never re-query Nomad for it.
	nc.allocationsErr = errors.New("must not be called for a terminal (lost) job")
	if _, err := svc.Get(context.Background(), job.ID); err != nil {
		t.Fatalf("Get after orphan-detection unexpectedly re-queried nomad: %v", err)
	}
}

// TestGet_StuckPendingPastTimeoutMarksLost covers a job Nomad still knows about but never placed
// (fleet full for the pool, or an unmatchable constraint) — it must stop reporting pending forever
// once Options.PendingTimeout elapses.
func TestGet_StuckPendingPastTimeoutMarksLost(t *testing.T) {
	nc := &fakeNomad{} // ListAllocations always returns empty — no allocation ever appears.
	svc := newTestServiceWithOpts(t, nc, Options{PendingTimeout: time.Millisecond})

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusLost {
		t.Fatalf("status = %v, want lost", got.Status)
	}
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "no allocation within") {
		t.Errorf("FailureReason = %v, want it to mention the pending timeout", got.FailureReason)
	}
}

// TestGet_PendingWithinTimeoutStaysPending guards against a false-positive: a freshly submitted job
// that just hasn't been placed yet (well within PendingTimeout) must stay pending, not lost.
func TestGet_PendingWithinTimeoutStaysPending(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{PendingTimeout: time.Hour})

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %v, want pending", got.Status)
	}
}

// TestGet_PendingReasonFromBlockedEvaluation covers the pending-placement diagnostics: a pending
// job's Get should surface Nomad's blocked-evaluation metrics (constraint filtered / dimension
// exhausted / class filtered / nodes available) as a human-readable Job.PendingReason.
func TestGet_PendingReasonFromBlockedEvaluation(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "macos", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.evaluationsByJob = map[string][]nomad.Evaluation{
		dispatchedJobID: {
			{
				Status:            "blocked",
				StatusDescription: "queued allocations still pending",
				FailedTGAllocs: map[string]nomad.AllocationMetric{
					"main": {
						NodesEvaluated:     2,
						NodesAvailable:     map[string]int{"dc1": 2},
						DimensionExhausted: map[string]int{"cpu": 2},
					},
				},
			},
		},
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %v, want pending", got.Status)
	}
	if got.PendingReason == "" {
		t.Fatal("PendingReason = \"\", want a non-empty reason derived from the blocked evaluation")
	}
	if !strings.Contains(got.PendingReason, "resources exhausted") || !strings.Contains(got.PendingReason, "cpu=2") {
		t.Errorf("PendingReason = %q, want it to mention the exhausted cpu dimension", got.PendingReason)
	}
}

// TestGet_PendingReasonEmptyWithoutEvaluations covers the common case (no evaluations recorded
// yet, or none blocked) — PendingReason must stay empty rather than reporting something bogus.
func TestGet_PendingReasonEmptyWithoutEvaluations(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PendingReason != "" {
		t.Errorf("PendingReason = %q, want empty with no recorded evaluations", got.PendingReason)
	}
}

// TestGet_PendingReasonClearedOnceRunning ensures a stale PendingReason from an earlier pending
// poll doesn't linger once the job actually starts running.
func TestGet_PendingReasonClearedOnceRunning(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "macos", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	dispatchedJobID := nc.dispatchCalls[0].jobName + "/dispatch-1"
	nc.evaluationsByJob = map[string][]nomad.Evaluation{
		dispatchedJobID: {{
			Status: "blocked",
			FailedTGAllocs: map[string]nomad.AllocationMetric{
				"main": {NodesEvaluated: 1, DimensionExhausted: map[string]int{"cpu": 1}},
			},
		}},
	}
	if got, err := svc.Get(context.Background(), job.ID); err != nil || got.PendingReason == "" {
		t.Fatalf("Get (pending): got=%+v err=%v, want a non-empty PendingReason", got, err)
	}

	nc.allocationsByJob = map[string][]nomad.Allocation{
		dispatchedJobID: {{ID: "alloc-1", ClientStatus: "running", CreatedAt: time.Now()}},
	}
	time.Sleep(2 * time.Millisecond) // let the newTestService StatusTTL cache entry from the pending Get expire
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get (running): %v", err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("status = %v, want running", got.Status)
	}
	if got.PendingReason != "" {
		t.Errorf("PendingReason = %q, want cleared once running", got.PendingReason)
	}
}

// TestRunReconciler_MarksOrphanedJobLostWithoutAnyGetCall is the periodic-sweep counterpart to
// TestGet_OrphanedNomadJobMarksLost: nothing calls Get/List for the job — RunReconciler's own tick
// must reconcile it (and thus detect the 404 and mark it lost) on its own.
func TestRunReconciler_MarksOrphanedJobLostWithoutAnyGetCall(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)

	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	nc.allocationsErr = nomad.ErrNotFound

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.RunReconciler(ctx, time.Millisecond) }()

	deadline := time.After(2 * time.Second)
	for {
		rec, ok := svc.(*service).store.get(job.ID)
		if ok && rec.Status == StatusLost {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for RunReconciler to mark the orphaned job lost")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestSubmit_ResourceHintIgnoredForUnknownPool(t *testing.T) {
	nc := &fakeNomad{}
	// No Options.Pools configured at all — the service has nothing to validate against, so any
	// hint is accepted rather than rejecting every request with resources set.
	svc := newTestService(t, nc)

	_, _, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindShell, Pool: "linux", Script: "true",
		Resources: &ResourceHint{CPU: 999999},
	})
	if err != nil {
		t.Fatalf("Submit: %v, want no error when the service has no pool config to validate against", err)
	}
}
