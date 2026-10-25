package cli

import (
	"testing"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
)

func TestWaitingByPool(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)
	jobs := []dispatch.Job{
		{Status: dispatch.StatusPending, SubmittedAt: old, Request: dispatch.JobRequest{Pool: "linux"}},
		{Status: dispatch.StatusPending, SubmittedAt: old, Request: dispatch.JobRequest{Pool: "linux"}, Fallback: &dispatch.Fallback{From: "linux", To: "macos"}},
		{Status: dispatch.StatusPending, SubmittedAt: now.Add(-5 * time.Second), Request: dispatch.JobRequest{Pool: "linux"}},
		{Status: dispatch.StatusPending, SubmittedAt: old, AllocID: "a1", Request: dispatch.JobRequest{Pool: "linux"}},
		{Status: dispatch.StatusRunning, SubmittedAt: old, Request: dispatch.JobRequest{Pool: "macos"}},
	}
	got := waitingByPool(jobs, now)
	if got["linux"] != 1 || got["macos"] != 1 || len(got) != 2 {
		t.Fatalf("waiting = %v, want linux 1 (the fresh and the allocated ones don't count) and macos 1 (the moved one)", got)
	}
}
