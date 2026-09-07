// Package dispatch turns a self-contained JobRequest into a parameterized Nomad job and tracks it.
package dispatch

import (
	"context"
	"io"
	"time"
)

// Kind selects which parameterized Nomad job a request maps to.
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
	// Pool constrains placement (meta.pool on Nomad nodes), e.g. "linux" | "macos" | "gpu".
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
	// Meta is free-form and round-tripped (Argos puts bead ids here).
	Meta map[string]string `json:"meta,omitempty"`
	// Requester is an opaque label for who submitted (argos, mcp:claude-code, cli).
	Requester string `json:"requester,omitempty"`
	// IdempotencyKey, if set, makes a repeat Submit with the same key return the existing Job
	// instead of dispatching again. Argos convention: "argos:<taskId>:<runId>".
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// Resources optionally hints at this job's desired sizing. Nomad cannot resize a parameterized
	// job's `resources` block per dispatch (there is no dispatch-time equivalent of job update), so
	// this is *not* applied to the running job yet — it is only validated against the target pool's
	// configured job defaults (fleet.yaml pool.jobCPU/jobMemory, see PoolConfig) and Submit rejects
	// a request that exceeds them with a 400. See docs/JOBS.md "Per-job resource sizing" for the
	// full limitation and the plan for real per-dispatch sizing.
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
	ID          string            `json:"id"` // dispatched Nomad job id
	Request     JobRequest        `json:"request"`
	Status      Status            `json:"status"`
	AllocID     string            `json:"allocId,omitempty"`
	Node        string            `json:"node,omitempty"`
	ExitCode    *int              `json:"exitCode,omitempty"`
	SubmittedAt time.Time         `json:"submittedAt"`
	StartedAt   *time.Time        `json:"startedAt,omitempty"`
	FinishedAt  *time.Time        `json:"finishedAt,omitempty"`
	Artifacts   []Artifact        `json:"artifacts,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
	// TimedOut is true when the job's script was killed by run.sh's own `timeout` wrapper (exit code
	// 124 is the POSIX `timeout(1)` convention for "killed for exceeding the deadline" — see
	// nomad/jobs/*.nomad.hcl's run.sh).
	TimedOut bool `json:"timedOut,omitempty"`
	// Signal is a human name ("SIGTERM", "SIGKILL", …) for the Unix signal (if any) that ended the
	// task, nil otherwise.
	Signal *string `json:"signal,omitempty"`
	// FailureReason is set for lost/infra faults (Nomad DriverError/SetupError/DownloadError, or a
	// generic "node lost" message for a StatusLost job) so callers can classify it as transient/retryable
	// rather than a genuine script failure.
	FailureReason *string    `json:"failureReason,omitempty"`
	Placement     *Placement `json:"placement,omitempty"`
	// PendingReason explains why a StatusPending job hasn't been placed yet, derived from Nomad's
	// blocked-evaluation metrics (constraint filtered / dimension exhausted / class filtered / no
	// nodes available — see nomad.Client.JobEvaluations). Empty when the job isn't pending, or
	// Nomad hasn't recorded a placement failure (yet).
	PendingReason string `json:"pendingReason,omitempty"`
}

// Placement is where a job's allocation landed, resolved via the Nomad node's meta (see
// internal/fleet/scripts.go's nomadMetaScript, which writes meta.pool/meta.host/meta.vm).
type Placement struct {
	AllocID  string `json:"allocId,omitempty"`
	NodeID   string `json:"nodeId,omitempty"`
	VMID     string `json:"vmId,omitempty"`
	WorkerID string `json:"workerId,omitempty"`
}

// Artifact is a file a job produced, stored in the artifact bucket.
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
	Time   time.Time // when this line was read from Nomad — best-effort, not the guest's own timestamp
}

// Service is the seam used by the HTTP server, the CLI and the MCP server.
type Service interface {
	// Submit dispatches req and returns the resulting Job. created is false when req.IdempotencyKey
	// matched an existing job — in that case the existing Job is returned unchanged and nothing is
	// (re-)dispatched.
	Submit(ctx context.Context, req JobRequest) (job *Job, created bool, err error)
	Get(ctx context.Context, id string) (*Job, error)
	List(ctx context.Context) ([]Job, error)
	// Logs streams combined stdout+stderr; follow keeps the stream open until the job ends.
	//
	// A job that hasn't been allocated yet (still StatusPending, no Nomad allocation) is not an
	// error by itself: with follow=true, Logs polls for an allocation (roughly once a second) until
	// one appears, the job reaches a terminal status, or ctx is done (the caller — the HTTP handler
	// — bounds this with a deadline; see server docs), then streams normally. With follow=false, a
	// missing allocation is reported as ErrNoAllocationYet without waiting — the caller (the HTTP
	// handler) turns that into a 200 with an empty body rather than an error, since "no output yet"
	// is a legitimate state for a pending job, not a failure.
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	// LogLines streams stdout/stderr as discrete, stream-tagged lines (used for the NDJSON log mode;
	// the plain/SSE modes keep using Logs). The channel is closed when both streams are drained (or,
	// with follow, when ctx is done or the job reaches a terminal status). Line ORDER across the two
	// streams is best-effort (same non-determinism the existing byte-level Logs merge already has —
	// see mergedLogReader) since stdout/stderr are two independent goroutines racing to send. See
	// Logs for the no-allocation-yet / ErrNoAllocationYet semantics, which apply identically here.
	LogLines(ctx context.Context, id string, follow bool) (<-chan LogLine, error)
	Cancel(ctx context.Context, id string) error
	// RunReconciler reconciles every non-terminal job's status against Nomad on each tick of
	// interval (and once immediately), until ctx is done. It exists so a job's status — in
	// particular lost-detection for an orphaned or stuck-pending job — advances even when nothing
	// is actively calling Get/List for it (see Options.PendingTimeout). `grove serve` runs this in
	// the background; a failed reconcile for one job is logged and doesn't stop the sweep. Returns
	// ctx.Err() once ctx is done.
	RunReconciler(ctx context.Context, interval time.Duration) error
}
