package apiclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestJobLogs_RetriesPendingUntilAllocated is the client-side regression test for the live-run
// defect: a non-follow `grove logs <id>` request against a job that hasn't been allocated yet used
// to surface the server's "pending" response immediately, forcing the caller to poll by hand.
// JobLogs should instead retry a few times with a short backoff before giving up.
func TestJobLogs_RetriesPendingUntilAllocated(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.Header().Set("X-Grove-Job-Status", "pending")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello logs\n"))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	rc, err := c.JobLogs(context.Background(), "job-1", false)
	if err != nil {
		t.Fatalf("JobLogs: %v", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello logs\n" {
		t.Errorf("body = %q, want %q", string(data), "hello logs\n")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server saw %d requests, want 3 (2 pending retries + 1 success)", got)
	}
}

// TestJobLogs_GivesUpAfterMaxRetries asserts JobLogs doesn't retry forever — once
// maxPendingLogRetries is exhausted it returns the last (still-empty, still-pending) response
// rather than blocking indefinitely.
func TestJobLogs_GivesUpAfterMaxRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("X-Grove-Job-Status", "pending")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	rc, err := c.JobLogs(context.Background(), "job-1", false)
	if err != nil {
		t.Fatalf("JobLogs: %v", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("body = %q, want empty", string(data))
	}
	if got := atomic.LoadInt32(&calls); got != maxPendingLogRetries+1 {
		t.Errorf("server saw %d requests, want %d (initial + %d retries)", got, maxPendingLogRetries+1, maxPendingLogRetries)
	}
}

// TestJobLogs_Follow_NeverRetriesPending: with follow=true the server itself already waits for an
// allocation (see internal/server/handlers_jobs.go), so a "pending" response is the server's final
// answer (its own wait deadline elapsed) rather than something the client should retry.
func TestJobLogs_Follow_NeverRetriesPending(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("X-Grove-Job-Status", "pending")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	rc, err := c.JobLogs(context.Background(), "job-1", true)
	if err != nil {
		t.Fatalf("JobLogs: %v", err)
	}
	rc.Close()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server saw %d requests, want exactly 1 (no client-side retry for follow=true)", got)
	}
}

// TestJobLogs_ErrorStatusReturnsError asserts a genuine error status (not the pending marker)
// still surfaces as an error rather than being silently retried/swallowed.
func TestJobLogs_ErrorStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "job not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.JobLogs(context.Background(), "job-1", false)
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want it to mention the 404 status", err)
	}
}
