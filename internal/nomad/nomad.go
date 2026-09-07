// Package nomad is grove's view of the Nomad cluster: nodes, allocations, dispatch, logs.
//
// The concrete implementation lives in client.go and wraps github.com/hashicorp/nomad/api.
package nomad

import (
	"context"
	"io"
	"time"
)

// Node is a Nomad client node (one per worker VM, plus any bare-metal clients).
type Node struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Status      string            `json:"status"` // ready | down | initializing
	Eligibility string            `json:"eligibility"`
	Drain       bool              `json:"drain"`
	NodeClass   string            `json:"nodeClass"`
	Datacenter  string            `json:"datacenter"`
	Attributes  map[string]string `json:"attributes"` // os.name, cpu.arch, …
	Meta        map[string]string `json:"meta"`       // pool, host — set by the guest image
	CPUMHz      int               `json:"cpuMHz"`
	MemoryMiB   int               `json:"memoryMiB"`
	RunningAllocs int             `json:"runningAllocs"`
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
}

// DispatchResult is what Nomad returns for a parameterized job dispatch.
type DispatchResult struct {
	DispatchedJobID string `json:"dispatchedJobId"`
	EvalID          string `json:"evalId"`
}

// Client is the seam between grove and Nomad.
type Client interface {
	ListNodes(ctx context.Context) ([]Node, error)
	// DrainNode enables/disables drain; deadline applies when enabling.
	DrainNode(ctx context.Context, nodeID string, enable bool, deadline time.Duration) error

	Dispatch(ctx context.Context, parentJobID string, meta map[string]string, payload []byte) (*DispatchResult, error)
	ListAllocations(ctx context.Context, jobID string) ([]Allocation, error)
	GetAllocation(ctx context.Context, allocID string) (*Allocation, error)
	// Logs streams a task's stdout or stderr ("stdout" | "stderr"); follow keeps the stream open.
	Logs(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error)
	StopJob(ctx context.Context, jobID string, purge bool) error

	// RegisterJobFile registers (or updates) a job from HCL, used to install grove's parameterized jobs.
	RegisterJobFile(ctx context.Context, hcl string) error

	Ping(ctx context.Context) error
}
