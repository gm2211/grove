// Package dispatch queues self-contained JobRequests and runs them itself, inside `grove serve` on
// the control plane: a persistent per-pool FIFO queue, an Executor that drives each job in one of
// its pool's sealed VMs (SandboxExecutor, over internal/sandbox), and per-job log files on the
// control plane's disk. There is no external scheduler.
package dispatch

import (
	"context"
	"io"
	"time"
)

// Kind selects what a job does with its request.
type Kind string

const (
	// KindBuild clones repo@ref, runs Script, uploads ./artifacts/** to the artifact store.
	KindBuild Kind = "build"
	// KindAgent runs a long-lived coding-agent session (Claude Code / Codex) against a checkout.
	KindAgent Kind = "agent"
	// KindShell runs an arbitrary command; used by `grove exec` and the MCP grove_run tool.
	KindShell Kind = "shell"
)

// JobRequest is everything needed to run a unit of work somewhere in the fleet.
type JobRequest struct {
	Kind Kind `json:"kind"`
	// Pool is the fleet.yaml pool whose sealed VMs run the job, e.g. "linux" | "macos".
	Pool string `json:"pool"`
	Repo string `json:"repo,omitempty"` // git URL
	Ref  string `json:"ref,omitempty"`  // commit SHA, branch or tag
	// Script is run with bash -eo pipefail in the checkout (build/agent) or cwd (shell).
	Script string `json:"script"`
	// Env is injected as environment variables. Secrets should reference names resolved
	// server-side from the artifact/secret store rather than be sent inline.
	Env     map[string]string `json:"env,omitempty"`
	Secrets []string          `json:"secrets,omitempty"`
	// Timeout is a duration string ("30m", "2h", "90s") or a JSON number of SECONDS on the wire —
	// never raw nanoseconds. See Duration's doc comment.
	Timeout Duration `json:"timeout,omitempty"`
	// Meta is free-form and round-tripped (an orchestrator can put its own task ids here).
	Meta map[string]string `json:"meta,omitempty"`
	// Requester is an opaque label for who submitted (orchestrator, mcp:claude-code, cli).
	Requester string `json:"requester,omitempty"`
	// SubmittedBy is set by Grove from the authenticated device identity. Client input is ignored.
	// It lets a dispatcher cancel its own jobs without granting fleet-wide operator power.
	SubmittedBy string `json:"submittedBy,omitempty"`
	// IdempotencyKey, if set, makes a repeat Submit with the same key return the existing Job
	// instead of dispatching again. Orchestrator convention: "<orchestrator>:<taskId>:<runId>".
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// Resources optionally hints at this job's desired sizing. It is *not* applied to the running
	// job — every job of a pool runs at the pool's job sizing (fleet.yaml pool.jobCPU/jobMemory,
	// see PoolConfig), which is also what the per-VM slot count is computed from — it is only
	// validated against those defaults, and Submit rejects a request that exceeds them with a 400.
	// See docs/JOBS.md "Per-job resource sizing".
	Resources *ResourceHint `json:"resources,omitempty"`
}

// ResourceHint is JobRequest.Resources: a CPU/memory sizing hint, in the same units as
// PoolConfig/fleet.Pool's job defaults (MHz / MiB).
type ResourceHint struct {
	CPU    int `json:"cpu,omitempty"`    // MHz
	Memory int `json:"memory,omitempty"` // MiB
}

// Status of a Job.
type Status string

const (
	StatusPending  Status = "pending"
	StatusRunning  Status = "running"
	StatusSuccess  Status = "success"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
	StatusLost     Status = "lost"
)

// Job is a submitted request plus what happened to it.
type Job struct {
	ID      string     `json:"id"`
	Request JobRequest `json:"request"`
	Status  Status     `json:"status"`
	// Deprecated: AllocID was the Nomad allocation id. Jobs no longer have one; it is kept only so
	// history written by older versions still loads.
	AllocID string `json:"allocId,omitempty"`
	// Node is the sealed VM the job ran in (same as Placement.VMID).
	Node        string     `json:"node,omitempty"`
	ExitCode    *int       `json:"exitCode,omitempty"`
	SubmittedAt time.Time  `json:"submittedAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Artifacts   []Artifact `json:"artifacts,omitempty"`
	// ArtifactsExpiredAt is when grove deleted this job's artifacts under its retention policy
	// (see config.ArtifactRetention); Artifacts is empty from then on.
	ArtifactsExpiredAt *time.Time        `json:"artifactsExpiredAt,omitempty"`
	Meta               map[string]string `json:"meta,omitempty"`
	// LogsExpiredAt is when grove deleted this job's log file under the same retention policy as
	// its artifacts; Logs/LogLines return an empty stream from then on.
	LogsExpiredAt *time.Time `json:"logsExpiredAt,omitempty"`
	// TimedOut is true when the job ran past its timeout and was stopped (exit code 124, the POSIX
	// `timeout(1)` convention — see internal/sandbox ExitTimedOut).
	TimedOut bool `json:"timedOut,omitempty"`
	// Signal is a human name ("SIGTERM", "SIGKILL", …) for the Unix signal (if any) that ended the
	// task, nil otherwise.
	Signal *string `json:"signal,omitempty"`
	// FailureReason is set for lost/infra faults (the sealed VM stopped answering, grove serve
	// restarted under a running job, no sealed VM came up) so callers can classify it as
	// transient/retryable rather than a genuine script failure.
	FailureReason *string    `json:"failureReason,omitempty"`
	Placement     *Placement `json:"placement,omitempty"`
	// RepoCredentialUsed records that this job's clone was given the control plane's armed
	// private-repo credential (see internal/gitauth and Options.RepoAuth). The credential itself
	// is never stored on the Job or returned by the API — this is only the audit bit saying one
	// was in play.
	RepoCredentialUsed bool `json:"repoCredentialUsed,omitempty"`
	// PendingReason explains why a StatusPending job hasn't started yet: how many jobs are ahead of
	// it in its pool's queue, or that it is waiting for a free slot in one of the pool's sealed VMs.
	// Empty when the job isn't pending.
	PendingReason string `json:"pendingReason,omitempty"`
}

// Placement is the sealed VM a job runs in and the worker (host Mac) that VM was named for.
type Placement struct {
	// Deprecated: AllocID and NodeID were Nomad's; history written by older versions keeps them.
	AllocID  string `json:"allocId,omitempty"`
	NodeID   string `json:"nodeId,omitempty"`
	VMID     string `json:"vmId,omitempty"`
	WorkerID string `json:"workerId,omitempty"`
}

// Artifact is a file a job produced, stored in the control plane's artifact store.
type Artifact struct {
	Path        string `json:"path"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType,omitempty"`
}

// LogLine is one stream-tagged line of job output.
type LogLine struct {
	Stream string    // "stdout" | "stderr"
	Line   string    // one line, no trailing newline
	Time   time.Time // when the control plane received this line — not the guest's own timestamp
}

// RepoAuth resolves the credential a job's `git clone` should use, if the control plane has one
// armed for that repository. It is the seam internal/gitauth's Store satisfies; a nil RepoAuth
// (the default) means grove never supplies clone credentials and only public repositories can be
// sourced.
type RepoAuth interface {
	// TokenFor returns the token the control plane's clone of repoURL should use, and ok=false
	// when private-repo sourcing is off or repoURL is out of the armed credential's scope.
	TokenFor(repoURL string) (token string, ok bool)
}

// Service is the seam used by the HTTP server, the CLI and the MCP server.
type Service interface {
	// Submit queues req and returns the resulting Job. created is false when req.IdempotencyKey
	// matched an existing job — in that case the existing Job is returned unchanged and nothing is
	// (re-)queued.
	Submit(ctx context.Context, req JobRequest) (job *Job, created bool, err error)
	Get(ctx context.Context, id string) (*Job, error)
	List(ctx context.Context) ([]Job, error)
	// Logs streams combined stdout+stderr; follow keeps the stream open until the job ends.
	//
	// A job still waiting in its pool's queue has no output yet, which is not an error by itself:
	// with follow=true, Logs waits until it starts, reaches a terminal status, or ctx is done (the
	// caller — the HTTP handler — bounds this with a deadline; see server docs), then streams
	// normally. With follow=false it is reported as ErrNoAllocationYet without waiting — the HTTP
	// handler turns that into a 200 with an empty body, since "no output yet" is a legitimate state
	// for a pending job, not a failure. A finished job whose log has expired streams nothing.
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	// LogLines streams stdout/stderr as discrete, stream-tagged lines (used for the NDJSON log mode;
	// the plain/SSE modes keep using Logs), in the order the control plane received them. The
	// channel is closed once the log is drained (with follow, once the job is terminal and its log
	// drained, or ctx is done). See Logs for the ErrNoAllocationYet semantics, which apply
	// identically here.
	LogLines(ctx context.Context, id string, follow bool) (<-chan LogLine, error)
	Cancel(ctx context.Context, id string) error
	// RunReconciler runs housekeeping on each tick of interval (and once immediately) until ctx is
	// done: it expires finished jobs' artifacts and log files under Options.ArtifactRetention.
	// `grove serve` runs this in the background; a failure is logged and doesn't stop the sweep.
	// Returns ctx.Err() once ctx is done.
	RunReconciler(ctx context.Context, interval time.Duration) error
}
