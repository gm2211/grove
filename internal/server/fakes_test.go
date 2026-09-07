package server

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

// fakeOrchard is a minimal in-memory orchard.Client for server tests.
type fakeOrchard struct {
	mu sync.Mutex

	workers []orchard.Worker
	vms     []orchard.VM

	pauseCalls  []string
	resumeCalls []string
	deleteCalls []string

	pingErr   error
	listErr   error
	pauseErr  error
	resumeErr error
	deleteErr error
}

func (f *fakeOrchard) ListWorkers(ctx context.Context) ([]orchard.Worker, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.workers, nil
}

func (f *fakeOrchard) PauseWorker(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCalls = append(f.pauseCalls, name)
	return f.pauseErr
}

func (f *fakeOrchard) ResumeWorker(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls = append(f.resumeCalls, name)
	return f.resumeErr
}

func (f *fakeOrchard) ListVMs(ctx context.Context) ([]orchard.VM, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.vms, nil
}

func (f *fakeOrchard) GetVM(ctx context.Context, name string) (*orchard.VM, error) {
	for _, v := range f.vms {
		if v.Name == name {
			cp := v
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeOrchard) CreateVM(ctx context.Context, spec orchard.VMSpec) (*orchard.VM, error) {
	return nil, nil
}

func (f *fakeOrchard) DeleteVM(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls = append(f.deleteCalls, name)
	return f.deleteErr
}

func (f *fakeOrchard) Exec(ctx context.Context, vm string, command []string, opts orchard.ExecOptions) (orchard.ExecSession, error) {
	return nil, nil
}

func (f *fakeOrchard) Ping(ctx context.Context) error { return f.pingErr }

// fakeNomad is a minimal in-memory nomad.Client for server tests.
type fakeNomad struct {
	mu sync.Mutex

	nodes []nomad.Node

	drainCalls []drainCall
	drainErr   error

	pingErr error
}

type drainCall struct {
	nodeID   string
	enable   bool
	deadline time.Duration
}

func (f *fakeNomad) ListNodes(ctx context.Context) ([]nomad.Node, error) { return f.nodes, nil }

func (f *fakeNomad) DrainNode(ctx context.Context, nodeID string, enable bool, deadline time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drainCalls = append(f.drainCalls, drainCall{nodeID: nodeID, enable: enable, deadline: deadline})
	return f.drainErr
}

func (f *fakeNomad) Dispatch(ctx context.Context, parentJobID string, meta map[string]string, payload []byte) (*nomad.DispatchResult, error) {
	return nil, nil
}
func (f *fakeNomad) ListAllocations(ctx context.Context, jobID string) ([]nomad.Allocation, error) {
	return nil, nil
}
func (f *fakeNomad) GetAllocation(ctx context.Context, allocID string) (*nomad.Allocation, error) {
	return nil, nil
}
func (f *fakeNomad) Logs(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeNomad) StopJob(ctx context.Context, jobID string, purge bool) error { return nil }
func (f *fakeNomad) RegisterJobFile(ctx context.Context, hcl string) error       { return nil }
func (f *fakeNomad) Ping(ctx context.Context) error                              { return f.pingErr }

// fakeDispatch is a minimal in-memory dispatch.Service for server tests.
type fakeDispatch struct {
	mu sync.Mutex

	submitCalls []dispatch.JobRequest
	submitJob   *dispatch.Job
	submitErr   error

	jobs   map[string]*dispatch.Job
	getErr error

	listErr error

	cancelCalls []string
	cancelErr   error

	logsFunc func(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
}

func (f *fakeDispatch) Submit(ctx context.Context, req dispatch.JobRequest) (*dispatch.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitCalls = append(f.submitCalls, req)
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	if f.submitJob != nil {
		return f.submitJob, nil
	}
	return &dispatch.Job{ID: "job-1", Request: req, Status: dispatch.StatusPending}, nil
}

func (f *fakeDispatch) Get(ctx context.Context, id string) (*dispatch.Job, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	job, ok := f.jobs[id]
	if !ok {
		return nil, dispatch.ErrNotFound
	}
	return job, nil
}

func (f *fakeDispatch) List(ctx context.Context) ([]dispatch.Job, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]dispatch.Job, 0, len(f.jobs))
	for _, j := range f.jobs {
		out = append(out, *j)
	}
	return out, nil
}

func (f *fakeDispatch) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	if _, ok := f.jobs[id]; !ok {
		return nil, dispatch.ErrNotFound
	}
	if f.logsFunc != nil {
		return f.logsFunc(ctx, id, follow)
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeDispatch) Cancel(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls = append(f.cancelCalls, id)
	if f.cancelErr != nil {
		return f.cancelErr
	}
	if _, ok := f.jobs[id]; !ok {
		return dispatch.ErrNotFound
	}
	return nil
}
