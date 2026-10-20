package dispatch

import (
	"context"
	"io"
	"sync"
)

// Exit codes the dispatch service gives meaning to, besides 0. They match internal/sandbox's.
const (
	// ExitTimedOut means the job ran past its timeout and was stopped: Job.TimedOut.
	ExitTimedOut = 124
	// ExitLost means the sealed VM running the job stopped answering, or none came up: StatusLost.
	ExitLost = 125
	// ExitCanceled is 128+SIGTERM, what a job stopped by Cancel exits with.
	ExitCanceled = 143
	// ExitTempFail (EX_TEMPFAIL) returned by an Executor before it called Run.Started means the
	// job never reached a VM and should simply be tried again: the service puts it back at the
	// head of its pool's queue (up to maxTempFailRetries times). After Started it is the job's own
	// exit code, like any other.
	ExitTempFail = 75
)

// maxTempFailRetries bounds how often one job is requeued after ExitTempFail.
const maxTempFailRetries = 3

// Executor runs jobs for the dispatch service. SandboxExecutor is the real one; tests use fakes.
type Executor interface {
	// Execute runs one job to completion and returns its exit code. It must call run.Started once
	// the job has a place to run (until then the job stays StatusPending), write the job's output
	// to run.Stdout/run.Stderr, and return promptly-ish (after stopping the job) once ctx is done,
	// with ExitCanceled. Run.Request.Env holds the real values; run.CloneToken is the private-repo
	// credential the control plane's clone may use and must go nowhere else.
	Execute(ctx context.Context, run *Run) int
	// Abandon cleans up after a job that was running when `grove serve` last stopped: grove lost
	// track of it, so whatever it left at placement must not keep running unsupervised.
	Abandon(ctx context.Context, placement Placement) error
}

// Run is one execution of a queued job, handed to Executor.Execute.
type Run struct {
	ID string
	// Request is the job's request with its real env values (the stored Job's are redacted).
	Request JobRequest
	Pool    PoolConfig
	// CloneToken is the armed private-repo credential for Request.Repo, or "". It is resolved from
	// Options.RepoAuth when the job starts and is never persisted.
	CloneToken string
	// ArtifactPrefix is where a build job's ./artifacts/** go in the artifact store.
	ArtifactPrefix string
	Stdout         io.Writer
	Stderr         io.Writer

	startOnce sync.Once
	onStart   func(Placement)
}

// Started reports that the job now runs at p. Only the first call counts.
func (r *Run) Started(p Placement) {
	r.startOnce.Do(func() {
		if r.onStart != nil {
			r.onStart(p)
		}
	})
}
