package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
