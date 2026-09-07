// Package orchard is grove's view of the Orchard controller: the subset of the Orchard API grove
// needs, expressed in grove's own types so the rest of the code never imports Orchard directly.
//
// The concrete implementation lives in client.go and wraps github.com/cirruslabs/orchard/pkg/client
// (our fork, github.com/gm2211/orchard, which adds ShutdownScript and TTL).
package orchard

import (
	"context"
	"io"
	"time"
)

// Worker is an Orchard worker (one per Mac).
type Worker struct {
	Name             string            `json:"name"`
	MachineID        string            `json:"machineId"`
	Arch             string            `json:"arch"`
	Runtime          string            `json:"runtime"` // tart | vetu
	Labels           map[string]string `json:"labels"`
	Resources        map[string]uint64 `json:"resources"` // org.cirruslabs.tart-vms, logical-cores, memory-mib
	LastSeen         time.Time         `json:"lastSeen"`
	Offline          bool              `json:"offline"`
	SchedulingPaused bool              `json:"schedulingPaused"`
}

// VM is an Orchard VM as observed.
type VM struct {
	Name          string            `json:"name"`
	UID           string            `json:"uid"`
	Image         string            `json:"image"`
	Status        string            `json:"status"` // pending | running | failed
	StatusMessage string            `json:"statusMessage,omitempty"`
	Worker        string            `json:"worker"`
	CPU           uint64            `json:"cpu"`
	Memory        uint64            `json:"memory"` // MiB
	Labels        map[string]string `json:"labels"`
	Resources     map[string]uint64 `json:"resources"`
	RestartPolicy string            `json:"restartPolicy"`
	RestartCount  uint64            `json:"restartCount"`
	CreatedAt     time.Time         `json:"createdAt"`
	StartedAt     time.Time         `json:"startedAt,omitempty"`
	TTL           time.Duration     `json:"ttl,omitempty"`
}

// VMSpec is what grove asks Orchard to create.
type VMSpec struct {
	Name            string
	Image           string
	CPU             uint64
	Memory          uint64 // MiB
	DiskSize        uint64 // GiB, 0 = image default
	Labels          map[string]string
	Resources       map[string]uint64
	RestartPolicy   string // Never | OnFailure
	Headless        bool
	Username        string
	Password        string
	StartupScript   string
	ShutdownScript  string
	ShutdownTimeout time.Duration
	TTL             time.Duration
}

// ExecOptions controls a remote command.
type ExecOptions struct {
	// Session lets a caller reattach to a still-running exec after a disconnect.
	Session string
	Stdin   io.Reader
	TTY     bool
	// Wait is how long to wait for the VM to become ready before failing.
	Wait time.Duration
}

// ExecSession is a running command inside a VM.
type ExecSession interface {
	// Output streams combined stdout/stderr until the command exits.
	Output() io.Reader
	// Wait blocks until exit and returns the exit code.
	Wait(ctx context.Context) (int, error)
	Close() error
}

// Client is the seam between grove and Orchard.
type Client interface {
	ListWorkers(ctx context.Context) ([]Worker, error)
	PauseWorker(ctx context.Context, name string) error
	ResumeWorker(ctx context.Context, name string) error

	ListVMs(ctx context.Context) ([]VM, error)
	GetVM(ctx context.Context, name string) (*VM, error)
	CreateVM(ctx context.Context, spec VMSpec) (*VM, error)
	DeleteVM(ctx context.Context, name string) error

	Exec(ctx context.Context, vm string, command []string, opts ExecOptions) (ExecSession, error)

	// Ping verifies the controller is reachable and the token is valid.
	Ping(ctx context.Context) error
}
