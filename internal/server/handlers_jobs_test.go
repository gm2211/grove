package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/dispatch"
)

func TestPostJobs_DispatchesAndReturnsID(t *testing.T) {
	ds := &fakeDispatch{
		jobs:      map[string]*dispatch.Job{},
		submitJob: &dispatch.Job{ID: "abc123", Status: dispatch.StatusPending},
	}
	srv := newTestServer(nil, nil, ds, Options{})

	body := `{"kind":"shell","pool":"linux","script":"echo hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "abc123" {
		t.Errorf("id = %q, want abc123", out.ID)
	}

	if len(ds.submitCalls) != 1 {
		t.Fatalf("expected 1 submit call, got %d", len(ds.submitCalls))
	}
	if ds.submitCalls[0].Kind != dispatch.KindShell || ds.submitCalls[0].Pool != "linux" {
		t.Errorf("submit call = %+v", ds.submitCalls[0])
	}
}

func TestEnrolledDeviceDispatchIsLimitedToBuildAndAgentWithoutSecrets(t *testing.T) {
	store, err := NewAccessStore(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Issue("laptop", []string{ScopeRead, ScopeBuild, ScopeAgent})
	if err != nil {
		t.Fatal(err)
	}
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	srv := newTestServer(nil, nil, ds, Options{Token: "operator", AccessStore: store})

	submit := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := submit(`{"kind":"shell","pool":"linux","script":"id"}`); got != http.StatusForbidden {
		t.Fatalf("shell status=%d want 403", got)
	}
	if got := submit(`{"kind":"build","pool":"linux","script":"make test","secrets":["deploy-key"]}`); got != http.StatusForbidden {
		t.Fatalf("secret build status=%d want 403", got)
	}
	if got := submit(`{"kind":"build","pool":"linux","script":"make test"}`); got != http.StatusCreated {
		t.Fatalf("build status=%d want 201", got)
	}
	if got := submit(`{"kind":"agent","pool":"linux","script":"codex exec task"}`); got != http.StatusCreated {
		t.Fatalf("agent status=%d want 201", got)
	}
}

func TestPostJobs_IdempotentReplayReturns200(t *testing.T) {
	notCreated := false
	ds := &fakeDispatch{
		jobs:      map[string]*dispatch.Job{},
		submitJob: &dispatch.Job{ID: "abc123", Status: dispatch.StatusRunning},
		created:   &notCreated,
	}
	srv := newTestServer(nil, nil, ds, Options{})

	body := `{"kind":"shell","pool":"linux","script":"echo hi","idempotencyKey":"orch:t1:r1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "abc123" {
		t.Errorf("id = %q, want abc123", out.ID)
	}
}

func TestPostJobs_InvalidJSON(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestPostJobs_SubmitValidationErrorIs400(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}, submitErr: dispatch.ErrNotFound}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"kind":"shell","pool":"linux","script":"x"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGetJob(t *testing.T) {
	job := &dispatch.Job{ID: "job-1", Status: dispatch.StatusRunning}
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{"job-1": job}}
	srv := newTestServer(nil, nil, ds, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var got dispatch.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "job-1" || got.Status != dispatch.StatusRunning {
		t.Errorf("got = %+v", got)
	}
}

func TestGetJob_NotFound(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/nope", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestListJobs(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{
		"a": {ID: "a", Status: dispatch.StatusPending},
		"b": {ID: "b", Status: dispatch.StatusRunning},
	}}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []dispatch.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestCancelJob(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1"}}}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/jobs/job-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(ds.cancelCalls) != 1 || ds.cancelCalls[0] != "job-1" {
		t.Errorf("cancelCalls = %v", ds.cancelCalls)
	}
}

func TestDispatcherCanCancelOnlyOwnJob(t *testing.T) {
	store, err := NewAccessStore(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatal(err)
	}
	ownerToken, owner, err := store.Issue("owner", []string{ScopeRead, ScopeDispatch})
	if err != nil {
		t.Fatal(err)
	}
	otherToken, _, err := store.Issue("other", []string{ScopeRead, ScopeDispatch})
	if err != nil {
		t.Fatal(err)
	}
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1", Request: dispatch.JobRequest{SubmittedBy: owner.ID}}}}
	srv := newTestServer(nil, nil, ds, Options{Token: "operator", AccessStore: store})

	cancel := func(token string) int {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/jobs/job-1", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := cancel(otherToken); got != http.StatusForbidden {
		t.Fatalf("other device status=%d want 403", got)
	}
	if got := cancel(ownerToken); got != http.StatusNoContent {
		t.Fatalf("owner status=%d want 204", got)
	}
	if got := cancel("operator"); got != http.StatusNoContent {
		t.Fatalf("operator status=%d want 204", got)
	}
}

func TestCancelJob_PostAlias(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1"}}}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-1/cancel", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(ds.cancelCalls) != 1 || ds.cancelCalls[0] != "job-1" {
		t.Errorf("cancelCalls = %v", ds.cancelCalls)
	}
}

func TestCancelJob_PostAlias_NotFound(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/nope/cancel", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCancelJob_NotFound(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/jobs/nope", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestJobLogs_ChunkedByDefault(t *testing.T) {
	ds := &fakeDispatch{
		jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1"}},
		logsFunc: func(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("hello logs\n")), nil
		},
	}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/logs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	if rec.Body.String() != "hello logs\n" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestJobLogs_SSEWhenRequested(t *testing.T) {
	ds := &fakeDispatch{
		jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1"}},
		logsFunc: func(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("line-one\n")), nil
		},
	}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/logs", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: line-one") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestJobLogs_NotFound(t *testing.T) {
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{}}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/nope/logs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestJobLogs_NoAllocationYet_NonFollow_Returns200Pending is the regression test for the
// live-run defect: `grove logs <id>` on a job that hasn't been allocated yet used to 502
// ("job has no allocation yet"). It must now succeed with an empty body and a pending marker
// header instead, since "no output yet" isn't a failure.
func TestJobLogs_NoAllocationYet_NonFollow_Returns200Pending(t *testing.T) {
	ds := &fakeDispatch{
		jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1", Status: dispatch.StatusPending}},
		logsFunc: func(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
			return nil, dispatch.ErrNoAllocationYet
		},
	}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/logs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Grove-Job-Status"); got != "pending" {
		t.Errorf("X-Grove-Job-Status = %q, want %q", got, "pending")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

// TestJobLogs_NoAllocationYet_NDJSON_Returns200Pending is the NDJSON-mode counterpart of the
// above.
func TestJobLogs_NoAllocationYet_NDJSON_Returns200Pending(t *testing.T) {
	ds := &fakeDispatch{
		jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1", Status: dispatch.StatusPending}},
		logLinesFunc: func(ctx context.Context, id string, follow bool) (<-chan dispatch.LogLine, error) {
			return nil, dispatch.ErrNoAllocationYet
		},
	}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/logs", nil)
	req.Header.Set("Accept", "application/x-ndjson")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Grove-Job-Status"); got != "pending" {
		t.Errorf("X-Grove-Job-Status = %q, want %q", got, "pending")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

// TestJobLogs_Follow_BoundsContextWithWaitDeadline asserts that a follow=true request wraps the
// context handed to dispatch.Logs with a deadline (so Logs/LogLines's poll-for-allocation loop is
// actually bounded — see dispatch.Service.Logs), and that `?wait=` overrides the default.
func TestJobLogs_Follow_BoundsContextWithWaitDeadline(t *testing.T) {
	var gotDeadline time.Time
	var gotOK bool
	ds := &fakeDispatch{
		jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1"}},
		logsFunc: func(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
			gotDeadline, gotOK = ctx.Deadline()
			return io.NopCloser(strings.NewReader("")), nil
		},
	}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/logs?follow=1&wait=45s", nil)
	rec := httptest.NewRecorder()
	before := time.Now()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !gotOK {
		t.Fatal("expected the context passed to dispatch.Logs to carry a deadline for follow=1")
	}
	// wait=45s should be honored (within a generous slack for test execution time), not the 10m default.
	if d := gotDeadline.Sub(before); d <= 0 || d > time.Minute {
		t.Errorf("deadline = %s from now, want ~45s (honoring ?wait=45s, not the 10m default)", d)
	}
}

// TestJobLogs_NoFollow_ContextHasNoDeadline asserts the non-follow path doesn't impose any extra
// deadline — it's a single check, not a bounded wait.
func TestJobLogs_NoFollow_ContextHasNoDeadline(t *testing.T) {
	var gotOK bool
	ds := &fakeDispatch{
		jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1"}},
		logsFunc: func(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
			_, gotOK = ctx.Deadline()
			return io.NopCloser(strings.NewReader("")), nil
		},
	}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/logs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotOK {
		t.Error("expected no context deadline to be added for a non-follow request")
	}
}

func linesChanOf(lines ...dispatch.LogLine) <-chan dispatch.LogLine {
	ch := make(chan dispatch.LogLine, len(lines))
	for _, ln := range lines {
		ch <- ln
	}
	close(ch)
	return ch
}

func TestJobLogs_NDJSON(t *testing.T) {
	ds := &fakeDispatch{
		jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1"}},
		logLinesFunc: func(ctx context.Context, id string, follow bool) (<-chan dispatch.LogLine, error) {
			return linesChanOf(
				dispatch.LogLine{Stream: "stdout", Line: "line-one"},
				dispatch.LogLine{Stream: "stderr", Line: "line-two"},
				dispatch.LogLine{Stream: "stdout", Line: "line-three"},
			), nil
		},
	}
	srv := newTestServer(nil, nil, ds, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/logs", nil)
	req.Header.Set("Accept", "application/x-ndjson")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content-type = %q", ct)
	}

	type rec2 struct {
		Offset int64  `json:"offset"`
		Ts     string `json:"ts"`
		Stream string `json:"stream"`
		Line   string `json:"line"`
	}
	var got []rec2
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	for dec.More() {
		var r rec2
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3: %+v", len(got), got)
	}
	if got[0].Stream != "stdout" || got[0].Line != "line-one" || got[0].Offset != 0 {
		t.Errorf("record 0 = %+v", got[0])
	}
	if got[1].Stream != "stderr" || got[1].Line != "line-two" {
		t.Errorf("record 1 = %+v", got[1])
	}
	if got[1].Offset <= got[0].Offset {
		t.Errorf("offsets not strictly increasing: %d, %d", got[0].Offset, got[1].Offset)
	}
	if got[2].Offset <= got[1].Offset {
		t.Errorf("offsets not strictly increasing: %d, %d", got[1].Offset, got[2].Offset)
	}
	for _, r := range got {
		if r.Ts == "" {
			t.Errorf("record %+v has empty ts", r)
		}
	}
}

func TestJobLogs_NDJSON_SinceOffsetSkipsEarlierLines(t *testing.T) {
	ds := &fakeDispatch{
		jobs: map[string]*dispatch.Job{"job-1": {ID: "job-1"}},
		logLinesFunc: func(ctx context.Context, id string, follow bool) (<-chan dispatch.LogLine, error) {
			return linesChanOf(
				dispatch.LogLine{Stream: "stdout", Line: "line-one"},
				dispatch.LogLine{Stream: "stdout", Line: "line-two"},
				dispatch.LogLine{Stream: "stdout", Line: "line-three"},
			), nil
		},
	}
	srv := newTestServer(nil, nil, ds, Options{})

	// First, discover the offset of "line-two" from an unfiltered request.
	req0 := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/logs", nil)
	req0.Header.Set("Accept", "application/x-ndjson")
	rec0 := httptest.NewRecorder()
	srv.ServeHTTP(rec0, req0)

	type rec2 struct {
		Offset int64  `json:"offset"`
		Line   string `json:"line"`
	}
	var all []rec2
	dec := json.NewDecoder(strings.NewReader(rec0.Body.String()))
	for dec.More() {
		var r rec2
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		all = append(all, r)
	}
	if len(all) != 3 {
		t.Fatalf("got %d records, want 3", len(all))
	}
	sinceOffset := all[1].Offset

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/jobs/job-1/logs?sinceOffset=%d", sinceOffset), nil)
	req.Header.Set("Accept", "application/x-ndjson")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var got []rec2
	dec = json.NewDecoder(strings.NewReader(rec.Body.String()))
	for dec.More() {
		var r rec2
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2 (line-one skipped): %+v", len(got), got)
	}
	if got[0].Line != "line-two" || got[1].Line != "line-three" {
		t.Errorf("got = %+v, want [line-two, line-three]", got)
	}
}

func TestGetArtifact_StreamsFromArtifactStore(t *testing.T) {
	fa := &fakeArtifactsClient{
		objects: map[string]struct {
			data []byte
			obj  artifacts.Object
		}{
			"jobs/job-1/out.bin": {
				data: []byte("hello artifact"),
				obj:  artifacts.Object{Path: "jobs/job-1/out.bin", Size: 14, ContentType: "application/octet-stream"},
			},
		},
	}
	srv := New(&fakeOrchard{}, &fakeNomad{}, &fakeDispatch{jobs: map[string]*dispatch.Job{}}, fa, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/artifacts/out.bin", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello artifact" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content-type = %q", ct)
	}
}

func TestGetArtifact_NotFound(t *testing.T) {
	fa := &fakeArtifactsClient{objects: map[string]struct {
		data []byte
		obj  artifacts.Object
	}{}}
	srv := New(&fakeOrchard{}, &fakeNomad{}, &fakeDispatch{jobs: map[string]*dispatch.Job{}}, fa, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/artifacts/missing.bin", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetArtifact_NilClientYields404(t *testing.T) {
	srv := newTestServer(nil, nil, nil, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job-1/artifacts/out.bin", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// jobReadFixture is a server with an access store, one job submitted by "owner" (carrying env
// values) and one by another device, plus an artifact under each. Tokens: operator, owner, other.
func jobReadFixture(t *testing.T) (srv *Server, ownerToken, otherToken string) {
	t.Helper()
	store, err := NewAccessStore(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatal(err)
	}
	ownerToken, owner, err := store.Issue("owner", []string{ScopeRead, ScopeBuild})
	if err != nil {
		t.Fatal(err)
	}
	otherToken, other, err := store.Issue("other", []string{ScopeRead, ScopeBuild})
	if err != nil {
		t.Fatal(err)
	}
	ds := &fakeDispatch{jobs: map[string]*dispatch.Job{
		"job-1": {ID: "job-1", Request: dispatch.JobRequest{SubmittedBy: owner.ID, Env: map[string]string{"API_KEY": "s3cret"}}},
		"job-2": {ID: "job-2", Request: dispatch.JobRequest{SubmittedBy: other.ID}},
	}}
	obj := func(key string) struct {
		data []byte
		obj  artifacts.Object
	} {
		return struct {
			data []byte
			obj  artifacts.Object
		}{data: []byte("x"), obj: artifacts.Object{Path: key, Size: 1, ContentType: "text/html"}}
	}
	fa := &fakeArtifactsClient{objects: map[string]struct {
		data []byte
		obj  artifacts.Object
	}{
		"jobs/job-1/out.html": obj("jobs/job-1/out.html"),
		"jobs/job-2/out.html": obj("jobs/job-2/out.html"),
	}}
	return New(&fakeOrchard{}, &fakeNomad{}, ds, fa, Options{Token: "operator", AccessStore: store}), ownerToken, otherToken
}

func getAs(srv *Server, token, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestJobReads_EnvValuesAreRedactedEvenForOperatorAndOwner(t *testing.T) {
	srv, ownerToken, _ := jobReadFixture(t)
	for _, token := range []string{"operator", ownerToken} {
		rec := getAs(srv, token, "/api/v1/jobs/job-1")
		if rec.Code != http.StatusOK {
			t.Fatalf("get status=%d body=%s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "s3cret") {
			t.Fatalf("env value leaked in GET /jobs/job-1: %s", rec.Body.String())
		}
		var got dispatch.Job
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Request.Env["API_KEY"] != redactedEnvValue {
			t.Errorf("env = %v, want API_KEY kept with a redacted value", got.Request.Env)
		}
		list := getAs(srv, token, "/api/v1/jobs")
		if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "s3cret") {
			t.Fatalf("list status=%d leaked env: %s", list.Code, list.Body.String())
		}
	}
}

func TestJobReads_NonOperatorSeesOnlyOwnJobs(t *testing.T) {
	srv, ownerToken, otherToken := jobReadFixture(t)

	var listed []dispatch.Job
	rec := getAs(srv, ownerToken, "/api/v1/jobs")
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "job-1" {
		t.Fatalf("owner list = %+v, want only job-1", listed)
	}
	rec = getAs(srv, "operator", "/api/v1/jobs")
	listed = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("operator list len = %d, want 2", len(listed))
	}

	for _, path := range []string{"/api/v1/jobs/job-1", "/api/v1/jobs/job-1/logs", "/api/v1/jobs/job-1/artifacts/out.html"} {
		if got := getAs(srv, otherToken, path).Code; got != http.StatusForbidden {
			t.Errorf("other device GET %s = %d, want 403", path, got)
		}
		if got := getAs(srv, ownerToken, path).Code; got != http.StatusOK {
			t.Errorf("owner GET %s = %d, want 200", path, got)
		}
		if got := getAs(srv, "operator", path).Code; got != http.StatusOK {
			t.Errorf("operator GET %s = %d, want 200", path, got)
		}
	}
}

func TestGetArtifact_RejectsTraversalAndSetsDownloadHeaders(t *testing.T) {
	srv, ownerToken, _ := jobReadFixture(t)
	for _, path := range []string{
		"/api/v1/jobs/job-1/artifacts/%2e%2e/job-2/out.html",
		"/api/v1/jobs/job-1/artifacts/%252e%252e/job-2/out.html",
		"/api/v1/jobs/job-1/artifacts/a/%2E%2E/%2E%2E/job-2/out.html",
		"/api/v1/jobs/job-1/artifacts/%2fetc%2fpasswd",
		"/api/v1/jobs/%2e%2e/artifacts/job-2/out.html",
	} {
		for _, token := range []string{"operator", ownerToken} {
			rec := getAs(srv, token, path)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET %s as %s = %d, want 400", path, token, rec.Code)
			}
		}
	}
	rec := getAs(srv, ownerToken, "/api/v1/jobs/job-1/artifacts/out.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename=out.html` {
		t.Errorf("Content-Disposition = %q", got)
	}
}

func TestSafeArtifactPath(t *testing.T) {
	for p, want := range map[string]bool{
		"out.bin": true, "dir/out.bin": true, "": false, "/abs": false, "..": false,
		"a/../b": false, "./a": false, "a//b": false, `a\..\b`: false, "a/": false,
	} {
		if got := safeArtifactPath(p); got != want {
			t.Errorf("safeArtifactPath(%q) = %v, want %v", p, got, want)
		}
	}
}
