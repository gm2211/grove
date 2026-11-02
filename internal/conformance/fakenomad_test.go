package conformance

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

// fakeNomad is the smallest Nomad that lets a dispatched job sit unplaced: it accepts dispatches,
// never places them, and reports the evaluation the test sets.
type fakeNomad struct {
	mu      sync.Mutex
	evals   []nomad.Evaluation
	stopped []string
	n       int
}

func (f *fakeNomad) ListNodes(context.Context) ([]nomad.Node, error) { return nil, nil }
func (f *fakeNomad) GetNode(context.Context, string) (*nomad.Node, error) {
	return nil, nomad.ErrNotFound
}
func (f *fakeNomad) DrainNode(context.Context, string, bool, time.Duration) error { return nil }
func (f *fakeNomad) Dispatch(_ context.Context, parent string, _ map[string]string, _ []byte) (*nomad.DispatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return &nomad.DispatchResult{DispatchedJobID: parent + "/dispatch-" + strings.Repeat("x", f.n), EvalID: "eval"}, nil
}
func (f *fakeNomad) ListAllocations(context.Context, string) ([]nomad.Allocation, error) {
	return nil, nil
}
func (f *fakeNomad) GetAllocation(context.Context, string) (*nomad.Allocation, error) {
	return nil, nomad.ErrNotFound
}
func (f *fakeNomad) JobEvaluations(context.Context, string) ([]nomad.Evaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evals, nil
}
func (f *fakeNomad) Logs(context.Context, string, string, string, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeNomad) StopJob(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeNomad) RegisterJobFile(context.Context, string) error { return nil }
func (f *fakeNomad) Ping(context.Context) error                    { return nil }

// noNodesInPool is what Nomad reports when no node of the job's pool exists.
func noNodesInPool(pool string) []nomad.Evaluation {
	return []nomad.Evaluation{{Status: "blocked", FailedTGAllocs: map[string]nomad.AllocationMetric{
		"main": {ConstraintFiltered: map[string]int{"${meta.pool} = " + pool: 1}, NodesEvaluated: 1},
	}}}
}
