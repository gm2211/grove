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
	Timeout time.Duration     `json:"timeout,omitempty"`
	// Meta is free-form and round-tripped (Argos puts bead ids here).
	Meta map[string]string `json:"meta,omitempty"`
	// Requester is an opaque label for who submitted (argos, mcp:claude-code, cli).
	Requester string `json:"requester,omitempty"`
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
}

// Artifact is a file a job produced, stored in the artifact bucket.
type Artifact struct {
	Path string `json:"path"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// Service is the seam used by the HTTP server, the CLI and the MCP server.
type Service interface {
	Submit(ctx context.Context, req JobRequest) (*Job, error)
	Get(ctx context.Context, id string) (*Job, error)
	List(ctx context.Context) ([]Job, error)
	// Logs streams combined stdout+stderr; follow keeps the stream open until the job ends.
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	Cancel(ctx context.Context, id string) error
}
