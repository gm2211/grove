package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

func TestSubmit_RetryLostMeta(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name     string
		isolated bool
		req      JobRequest
		want     string // "" = no retry_lost meta
	}{
		{"build on a sealed pool", true, JobRequest{Kind: KindBuild, Repo: "https://github.com/o/r"}, "true"},
		{"agent on a sealed pool", true, JobRequest{Kind: KindAgent, Repo: "https://github.com/o/r"}, "false"},
		{"build that opted out", true, JobRequest{Kind: KindBuild, Repo: "https://github.com/o/r", RetryLost: &no}, "false"},
		{"shared pool", false, JobRequest{Kind: KindShell}, ""},
	} {
		nc := &fakeNomad{}
		svc := newTestServiceWithOpts(t, nc, Options{Pools: []PoolConfig{{Name: "linux", Isolated: tc.isolated}}})
		tc.req.Pool, tc.req.Script = "linux", "true"
		if _, _, err := svc.Submit(context.Background(), tc.req); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got, ok := nc.dispatchCalls[0].meta["retry_lost"]
		if (tc.want == "" && ok) || got != tc.want {
			t.Errorf("%s: retry_lost = %q (set %v), want %q", tc.name, got, ok, tc.want)
		}
	}
}

func TestGet_AttemptsAndQuarantine(t *testing.T) {
	dir := t.TempDir()
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{AttemptsDir: dir, Pools: []PoolConfig{{Name: "linux", Isolated: true}}})
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := JobIDFromArtifactPrefix(nc.dispatchCalls[0].meta["artifact_prefix"]); !ok || id != job.ID {
		t.Fatalf("job id from artifact prefix = %q, %v", id, ok)
	}
	allocs := func(status string, exit *int) {
		nc.allocationsByJob = map[string][]nomad.Allocation{
			nc.dispatchCalls[0].jobName + "/dispatch-1": {{ID: "alloc-1", ClientStatus: status, ExitCode: exit, CreatedAt: time.Now()}},
		}
	}
	start := time.Now().UTC()
	end := start.Add(time.Minute)
	rec := AttemptRecord{JobID: job.ID, Attempts: []Attempt{
		{VM: "linux-mac1-0", Worker: "mac1", StartedAt: start, FinishedAt: &end, Outcome: AttemptLost, JobStarted: true, Reason: "lost the job"},
		{VM: "linux-mac2-0", Worker: "mac2", StartedAt: end, Outcome: AttemptRunning},
	}}
	if err := WriteAttempts(dir, rec); err != nil {
		t.Fatal(err)
	}

	allocs("running", nil)
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 2 || got.Placement == nil || got.Placement.VMID != "linux-mac2-0" || got.Placement.WorkerID != "mac2" {
		t.Fatalf("running job: attempts %+v placement %+v", got.Attempts, got.Placement)
	}

	rec.Attempts[1].Outcome, rec.Attempts[1].FinishedAt, rec.Attempts[1].JobStarted = AttemptLost, &end, true
	rec.Quarantined, rec.QuarantineReason = true, "lost on 2 different machines (mac1, mac2)"
	if err := WriteAttempts(dir, rec); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // past the status cache
	exit := 125
	allocs("complete", &exit)
	got, err = svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || !got.Quarantined || got.FailureReason == nil || !strings.Contains(*got.FailureReason, "quarantined: lost on 2") {
		t.Fatalf("quarantined job: status %s quarantined %v reason %v", got.Status, got.Quarantined, got.FailureReason)
	}
	// The record outlives the job by a while, then goes; the history keeps the attempts.
	impl := svc.(*service)
	impl.expireAttempts(time.Now())
	if _, ok, _ := ReadAttempts(dir, job.ID); !ok {
		t.Fatal("the attempt record was removed as soon as the job ended")
	}
	impl.expireAttempts(time.Now().Add(2 * attemptRecordGrace))
	if _, ok, _ := ReadAttempts(dir, job.ID); ok {
		t.Fatal("the attempt record outlived its grace period")
	}
	again, _ := svc.Get(context.Background(), job.ID)
	if len(again.Attempts) != 2 || !again.Quarantined || again.Status != StatusFailed {
		t.Fatalf("after the record was removed: %+v", again)
	}
}

func TestAttemptsPathRefusesNonIDs(t *testing.T) {
	for _, bad := range []string{"", "../x", "0123456789ABCDEF", "short"} {
		if err := WriteAttempts(t.TempDir(), AttemptRecord{JobID: bad}); err == nil {
			t.Errorf("wrote a record for job id %q", bad)
		}
	}
	for _, bad := range []string{"jobs/../x/", "jobs/0123456789abcdef", "x/0123456789abcdef/"} {
		if _, ok := JobIDFromArtifactPrefix(bad); ok {
			t.Errorf("parsed a job id from %q", bad)
		}
	}
}
