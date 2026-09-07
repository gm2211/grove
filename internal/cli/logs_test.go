package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/dispatch"
)

// ndjsonRec mirrors apiclient.LogLine's wire format, for building fake server responses.
type ndjsonRec struct {
	Offset int64  `json:"offset"`
	Ts     string `json:"ts"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

func writeNDJSON(t *testing.T, w http.ResponseWriter, lines ...ndjsonRec) {
	t.Helper()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, ln := range lines {
		if err := enc.Encode(ln); err != nil {
			t.Fatalf("encode ndjson line: %v", err)
		}
	}
}

// TestStreamLogsToStdout_FollowStreamsAndExitsWithJobStatus is the CLI-level regression test for
// the live-run defect: `grove dispatch --follow`/`grove logs --follow` used to print the job id
// and exit in ~30ms without waiting for the job or streaming anything. With follow=true,
// streamLogsToStdout must block until GET /jobs/{id} reports a terminal status, route stdout/
// stderr-tagged NDJSON lines to the matching stream, and return an *ExitError carrying the job's
// exit code plus a one-line failure summary on stderr.
func TestStreamLogsToStdout_FollowStreamsAndExitsWithJobStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/logs", func(w http.ResponseWriter, r *http.Request) {
		writeNDJSON(t, w,
			ndjsonRec{Offset: 0, Ts: "2024-01-01T00:00:00Z", Stream: "stdout", Line: "hello"},
			ndjsonRec{Offset: 6, Ts: "2024-01-01T00:00:00Z", Stream: "stderr", Line: "uh oh"},
		)
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		exitCode := 127
		job := dispatch.Job{ID: "job-1", Status: dispatch.StatusFailed, ExitCode: &exitCode, Node: "mac-mini-1"}
		if err := json.NewEncoder(w).Encode(job); err != nil {
			t.Fatalf("encode job: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := apiclient.New(srv.URL, "")
	cmd := &cobra.Command{}
	var stdout, stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())

	err := streamLogsToStdout(cmd, client, "job-1", true)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v (%T), want *ExitError", err, err)
	}
	if exitErr.Code != 127 {
		t.Errorf("exit code = %d, want 127", exitErr.Code)
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Errorf("stdout = %q, want to contain the stdout-tagged line", stdout.String())
	}
	if strings.Contains(stdout.String(), "uh oh") {
		t.Errorf("stdout = %q, want the stderr-tagged line NOT on stdout", stdout.String())
	}
	if !strings.Contains(stderr.String(), "uh oh") {
		t.Errorf("stderr = %q, want to contain the stderr-tagged line", stderr.String())
	}
	if !strings.Contains(stderr.String(), "job job-1 failed (exit 127) on mac-mini-1") {
		t.Errorf("stderr = %q, want the one-line failure summary", stderr.String())
	}
}

// TestStreamLogsToStdout_FollowSuccessExitsZero mirrors the same flow for a job that succeeds:
// streamLogsToStdout must return nil (not an *ExitError) with no failure summary printed.
func TestStreamLogsToStdout_FollowSuccessExitsZero(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/logs", func(w http.ResponseWriter, r *http.Request) {
		writeNDJSON(t, w, ndjsonRec{Offset: 0, Ts: "2024-01-01T00:00:00Z", Stream: "stdout", Line: "all good"})
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		exitCode := 0
		job := dispatch.Job{ID: "job-1", Status: dispatch.StatusSuccess, ExitCode: &exitCode, Node: "mac-mini-1"}
		if err := json.NewEncoder(w).Encode(job); err != nil {
			t.Fatalf("encode job: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := apiclient.New(srv.URL, "")
	cmd := &cobra.Command{}
	var stdout, stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())

	if err := streamLogsToStdout(cmd, client, "job-1", true); err != nil {
		t.Fatalf("streamLogsToStdout: %v", err)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty for a successful job", stderr.String())
	}
	if !strings.Contains(stdout.String(), "all good") {
		t.Errorf("stdout = %q, want to contain the logged line", stdout.String())
	}
}

// TestStreamLogsToStdout_NonFollowIsOneShotSnapshot asserts follow=false preserves the pre-existing
// one-shot `grove logs <id>` contract: a single fetch, no wait for the job, no GetJob call at all.
func TestStreamLogsToStdout_NonFollowIsOneShotSnapshot(t *testing.T) {
	var jobCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/logs", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("follow"); got != "" {
			t.Errorf("follow query = %q, want empty for a non-follow request", got)
		}
		writeNDJSON(t, w, ndjsonRec{Offset: 0, Ts: "2024-01-01T00:00:00Z", Stream: "stdout", Line: "snapshot"})
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		jobCalls++
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := apiclient.New(srv.URL, "")
	cmd := &cobra.Command{}
	var stdout strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&strings.Builder{})
	cmd.SetContext(context.Background())

	if err := streamLogsToStdout(cmd, client, "job-1", false); err != nil {
		t.Fatalf("streamLogsToStdout: %v", err)
	}
	if !strings.Contains(stdout.String(), "snapshot") {
		t.Errorf("stdout = %q, want to contain the logged line", stdout.String())
	}
	if jobCalls != 0 {
		t.Errorf("GET /jobs/job-1 called %d times, want 0 for a non-follow request", jobCalls)
	}
}
