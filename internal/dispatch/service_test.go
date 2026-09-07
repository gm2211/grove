package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := svc.Submit(context.Background(), c.req); err == nil {
				t.Fatal("expected a validation error, got nil")
			}
		})
	}
	if len(nc.dispatchCalls) != 0 {
		t.Fatalf("Dispatch should not have been called, got %d calls", len(nc.dispatchCalls))
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
		Timeout:   30 * time.Second,
		Meta:      map[string]string{"bead": "123", "image": "golang:1.27"},
		Requester: "cli",
	}
	job, err := svc.Submit(context.Background(), req)
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
	if _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "echo hi"}); err != nil {
		t.Fatalf("Submit: %v", err)
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

	job, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
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

	job, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "false"})
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

func TestList_ReturnsAllJobs(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestService(t, nc)
	for i := 0; i < 3; i++ {
		if _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"}); err != nil {
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

	job, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
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
	job, err := svc1.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
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

	job, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
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
	job, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := svc.Logs(context.Background(), job.ID, false); err == nil {
		t.Fatal("expected an error when no allocation exists yet")
	}
}
