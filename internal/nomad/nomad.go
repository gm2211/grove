// Package nomad is grove's view of the Nomad cluster: nodes, allocations, dispatch, logs.
//
// The concrete implementation lives in client.go and wraps github.com/hashicorp/nomad/api.
package nomad

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is wrapped into the error returned by ListAllocations/GetAllocation/GetNode/
// JobEvaluations when Nomad responds 404 — e.g. the dispatched job (or its allocation) no longer
// exists because the Nomad server was restarted with a wiped data dir, or the job was purged.
// Callers check for it with errors.Is. The real client (client.go) wraps the API's 404 responses
// as this; a fake nomad.Client used in tests should return it directly (or wrapped) to simulate
// the same condition.
var ErrNotFound = errors.New("nomad: not found")

// Node is a Nomad client node (one per worker VM, plus any bare-metal clients).
type Node struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Status        string            `json:"status"` // ready | down | initializing
	Eligibility   string            `json:"eligibility"`
	Drain         bool              `json:"drain"`
	NodeClass     string            `json:"nodeClass"`
	Datacenter    string            `json:"datacenter"`
	Attributes    map[string]string `json:"attributes"` // os.name, cpu.arch, …
	Meta          map[string]string `json:"meta"`       // pool, host — set by the guest image
	CPUMHz        int               `json:"cpuMHz"`
	MemoryMiB     int               `json:"memoryMiB"`
	RunningAllocs int               `json:"runningAllocs"`
}

// Allocation is a scheduled task group instance.
type Allocation struct {
	ID           string     `json:"id"`
	JobID        string     `json:"jobId"`
	NodeID       string     `json:"nodeId"`
	NodeName     string     `json:"nodeName"`
	ClientStatus string     `json:"clientStatus"` // pending | running | complete | failed | lost
	TaskName     string     `json:"taskName"`
	ExitCode     *int       `json:"exitCode,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	// Signal is the raw Unix signal number (if any) from the allocation's terminal task event, e.g.
	// 15 (SIGTERM) or 9 (SIGKILL). nil when the task didn't end via a caught signal.
	Signal *int `json:"signal,omitempty"`
	// FailureReason is a human-readable infra-fault description (Nomad DriverError/SetupError/
	// DownloadError on the terminal task event), empty for an ordinary script failure (non-zero exit,
	// no infra fault) or a still-running/pending allocation.
	FailureReason string `json:"failureReason,omitempty"`
}

// DispatchResult is what Nomad returns for a parameterized job dispatch.
type DispatchResult struct {
	DispatchedJobID string `json:"dispatchedJobId"`
	EvalID          string `json:"evalId"`
}

// AllocationMetric is the subset of a Nomad evaluation's scheduling metrics that explain why an
// allocation couldn't be placed (a "blocked" evaluation's FailedTGAllocs), one per task group.
type AllocationMetric struct {
	// ConstraintFiltered counts nodes ruled out per unmet job/task constraint (e.g. a `meta.pool`
	// constraint), keyed by a human-readable description of the constraint.
	ConstraintFiltered map[string]int `json:"constraintFiltered,omitempty"`
	// DimensionExhausted counts nodes ruled out per exhausted resource dimension (cpu, memory, …) —
	// i.e. the fleet is full for this pool.
	DimensionExhausted map[string]int `json:"dimensionExhausted,omitempty"`
	// ClassFiltered counts nodes ruled out per node class the job doesn't match.
	ClassFiltered map[string]int `json:"classFiltered,omitempty"`
	// NodesEvaluated is how many nodes the scheduler considered.
	NodesEvaluated int `json:"nodesEvaluated,omitempty"`
	// NodesAvailable is how many nodes were available per datacenter.
	NodesAvailable map[string]int `json:"nodesAvailable,omitempty"`
}

// Evaluation is a Nomad scheduling evaluation for a job, used to explain a still-pending job's
// placement (see AllocationMetric).
type Evaluation struct {
	Status            string `json:"status"`
	StatusDescription string `json:"statusDescription,omitempty"`
	// FailedTGAllocs is keyed by task group name; non-empty only for a "blocked" evaluation that
	// couldn't place every requested allocation.
	FailedTGAllocs map[string]AllocationMetric `json:"failedTGAllocs,omitempty"`
}

// Client is the seam between grove and Nomad.
type Client interface {
	ListNodes(ctx context.Context) ([]Node, error)
	// GetNode fetches one Nomad client node by ID — used to resolve an allocation's placement
	// (node meta.vm / meta.host) without listing every node.
	GetNode(ctx context.Context, id string) (*Node, error)
	// DrainNode enables/disables drain; deadline applies when enabling.
	DrainNode(ctx context.Context, nodeID string, enable bool, deadline time.Duration) error

	Dispatch(ctx context.Context, parentJobID string, meta map[string]string, payload []byte) (*DispatchResult, error)
	ListAllocations(ctx context.Context, jobID string) ([]Allocation, error)
	GetAllocation(ctx context.Context, allocID string) (*Allocation, error)
	// JobEvaluations returns jobID's recorded evaluations (most recent first, per Nomad's own
	// ordering), used to surface why a still-pending job hasn't been placed — see Evaluation.
	JobEvaluations(ctx context.Context, jobID string) ([]Evaluation, error)
	// Logs streams a task's stdout or stderr ("stdout" | "stderr"); follow keeps the stream open.
	Logs(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error)
	StopJob(ctx context.Context, jobID string, purge bool) error

	// RegisterJobFile registers (or updates) a job from HCL, used to install grove's parameterized jobs.
	RegisterJobFile(ctx context.Context, hcl string) error

	Ping(ctx context.Context) error
}
