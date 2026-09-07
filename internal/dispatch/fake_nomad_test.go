package dispatch

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

// fakeNomad is a minimal in-memory nomad.Client used by dispatch package tests.
type fakeNomad struct {
	mu sync.Mutex

	dispatchCalls  []dispatchCall
	dispatchResult *nomad.DispatchResult
	dispatchErr    error

	allocationsByJob map[string][]nomad.Allocation
	allocationsErr   error

	stopCalls []stopCall
	stopErr   error

	registerCalls []string
	registerErr   error

	logsFunc func(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error)

	pingErr error
	nodes   []nomad.Node

	nodesByID  map[string]nomad.Node
	getNodeErr error
}

type dispatchCall struct {
	jobName string
	meta    map[string]string
	payload []byte
}

type stopCall struct {
	jobID string
	purge bool
}

func (f *fakeNomad) ListNodes(ctx context.Context) ([]nomad.Node, error) {
	return f.nodes, nil
}

func (f *fakeNomad) GetNode(ctx context.Context, id string) (*nomad.Node, error) {
	if f.getNodeErr != nil {
		return nil, f.getNodeErr
	}
	if n, ok := f.nodesByID[id]; ok {
		cp := n
		return &cp, nil
	}
	return nil, fmt.Errorf("fakeNomad: node %s not found", id)
}

func (f *fakeNomad) DrainNode(ctx context.Context, nodeID string, enable bool, deadline time.Duration) error {
	return nil
}

func (f *fakeNomad) Dispatch(ctx context.Context, parentJobID string, meta map[string]string, payload []byte) (*nomad.DispatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]string, len(meta))
	for k, v := range meta {
		cp[k] = v
	}
	f.dispatchCalls = append(f.dispatchCalls, dispatchCall{
		jobName: parentJobID,
		meta:    cp,
		payload: append([]byte(nil), payload...),
	})
	if f.dispatchErr != nil {
		return nil, f.dispatchErr
	}
	if f.dispatchResult != nil {
		return f.dispatchResult, nil
	}
	return &nomad.DispatchResult{DispatchedJobID: parentJobID + "/dispatch-1", EvalID: "eval-1"}, nil
}

func (f *fakeNomad) ListAllocations(ctx context.Context, jobID string) ([]nomad.Allocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allocationsErr != nil {
		return nil, f.allocationsErr
	}
	return f.allocationsByJob[jobID], nil
}

func (f *fakeNomad) GetAllocation(ctx context.Context, allocID string) (*nomad.Allocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, allocs := range f.allocationsByJob {
		for _, a := range allocs {
			if a.ID == allocID {
				cp := a
				return &cp, nil
			}
		}
	}
	return nil, fmt.Errorf("fakeNomad: alloc %s not found", allocID)
}

func (f *fakeNomad) Logs(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
	if f.logsFunc != nil {
		return f.logsFunc(ctx, allocID, task, stream, follow)
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeNomad) StopJob(ctx context.Context, jobID string, purge bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, stopCall{jobID: jobID, purge: purge})
	return f.stopErr
}

func (f *fakeNomad) RegisterJobFile(ctx context.Context, hcl string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerCalls = append(f.registerCalls, hcl)
	return f.registerErr
}

func (f *fakeNomad) Ping(ctx context.Context) error { return f.pingErr }
