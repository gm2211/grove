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

	"github.com/gm2211/grove/internal/artifacts"
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
			if _, _, err := svc.Submit(context.Background(), c.req); err == nil {
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
	if _, err := svc.Logs(context.Background(), job.ID, false); err == nil {
		t.Fatal("expected an error when no allocation exists yet")
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
	if _, err := svc.LogLines(context.Background(), job.ID, false); err == nil {
		t.Fatal("expected an error when no allocation exists yet")
	}
}
