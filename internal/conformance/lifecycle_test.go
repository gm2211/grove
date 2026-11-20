package conformance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/nomad"
)

// S24 (spec/README.md, spec/quint/lifecycle.qnt): one job, one watched copy. Each test below is a
// counterexample TLC found in the model of grove before this change (lifecycleBefore,
// lifecycleBeforeAmbiguous), replayed against the real dispatch service. spec/mutants.py puts each
// bug back and requires the matching test to fail.

// lcNomad is a Nomad that keeps every dispatched copy of a job, so a test can count the copies
// that could still run. It implements nomad.DispatchInputs (redispatch) and nomad.TokenDispatcher.
type lcNomad struct {
	mu   sync.Mutex
	jobs map[string]*lcCopy
	n    int

	// onDispatch, when set, runs before each dispatch outside the lock; it says whether Nomad
	// commits the copy and what the call answers.
	onDispatch func() (commit bool, err error)
	// onDispatchInput, when set, runs once DispatchInput has read the copy back, and onList
	// before ListAllocations, both outside the lock: a test parks a caller there.
	onDispatchInput func()
	onList          func()
	// late is a dispatch that answered with an error and has yet to commit (commitLate).
	late *lcCopy
}

type lcCopy struct {
	id, parent, token string
	meta              map[string]string
	payload           []byte
	state             string // queued | running | lost | exited | stopped | purged
}

func newLCNomad() *lcNomad { return &lcNomad{jobs: map[string]*lcCopy{}} }

func (f *lcNomad) newCopy(parent, token string, meta map[string]string, payload []byte) *lcCopy {
	f.n++
	return &lcCopy{id: fmt.Sprintf("%s/dispatch-%04d", parent, f.n), parent: parent, token: token, meta: meta, payload: payload, state: "queued"}
}

func (f *lcNomad) dispatch(parent string, meta map[string]string, payload []byte, token string) (*nomad.DispatchResult, error) {
	commit, err := true, error(nil)
	if f.onDispatch != nil {
		commit, err = f.onDispatch()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if token != "" {
		for _, c := range f.jobs {
			if c.token == token && (c.state == "queued" || c.state == "running") {
				return &nomad.DispatchResult{DispatchedJobID: c.id, EvalID: "eval"}, err
			}
		}
	}
	c := f.newCopy(parent, token, meta, payload)
	if commit {
		f.jobs[c.id] = c
	} else if err != nil {
		f.late = c
	}
	if err != nil {
		return nil, err
	}
	return &nomad.DispatchResult{DispatchedJobID: c.id, EvalID: "eval"}, nil
}

// commitLate lands the dispatch that answered with an error before Nomad committed it.
func (f *lcNomad) commitLate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.late != nil {
		f.jobs[f.late.id] = f.late
		f.late = nil
	}
}

func (f *lcNomad) set(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id].state = state
}

// live is every copy that could still run, sorted.
func (f *lcNomad) live() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, c := range f.jobs {
		if c.state == "queued" || c.state == "running" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (f *lcNomad) dispatches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func (f *lcNomad) Dispatch(_ context.Context, parent string, meta map[string]string, payload []byte) (*nomad.DispatchResult, error) {
	return f.dispatch(parent, meta, payload, "")
}
func (f *lcNomad) DispatchWithToken(_ context.Context, parent string, meta map[string]string, payload []byte, token string) (*nomad.DispatchResult, error) {
	return f.dispatch(parent, meta, payload, token)
}
func (f *lcNomad) StopDispatched(_ context.Context, parent, token string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.jobs {
		if c.parent == parent && c.token == token && c.state != "purged" {
			c.state = "purged"
			n++
		}
	}
	return n, nil
}
func (f *lcNomad) DispatchInput(_ context.Context, id string) (map[string]string, []byte, error) {
	f.mu.Lock()
	c, ok := f.jobs[id]
	found := ok && c.state != "purged"
	f.mu.Unlock()
	if !found {
		return nil, nil, nomad.ErrNotFound
	}
	if f.onDispatchInput != nil {
		f.onDispatchInput()
	}
	return c.meta, c.payload, nil
}
func (f *lcNomad) ListAllocations(_ context.Context, id string) ([]nomad.Allocation, error) {
	if f.onList != nil {
		f.onList()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.jobs[id]
	if !ok || c.state == "purged" {
		return nil, nomad.ErrNotFound
	}
	a := nomad.Allocation{ID: id + "/alloc", JobID: id, NodeID: "node", NodeName: "vm", CreatedAt: time.Unix(1, 0), Started: true}
	zero, killed := 0, 137
	switch c.state {
	case "queued":
		return nil, nil
	case "running":
		a.ClientStatus = "running"
	case "lost":
		a.ClientStatus = "lost"
	case "exited":
		a.ClientStatus, a.ExitCode = "complete", &zero
	case "stopped":
		a.ClientStatus, a.ExitCode = "complete", &killed
	}
	return []nomad.Allocation{a}, nil
}
func (f *lcNomad) StopJob(_ context.Context, id string, purge bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.jobs[id]
	if !ok || c.state == "purged" {
		return nomad.ErrNotFound
	}
	if purge {
		c.state = "purged"
	} else if c.state == "queued" || c.state == "running" {
		c.state = "stopped"
	}
	return nil
}
func (f *lcNomad) ListNodes(context.Context) ([]nomad.Node, error) { return nil, nil }
func (f *lcNomad) GetNode(context.Context, string) (*nomad.Node, error) {
	return nil, nomad.ErrNotFound
}
func (f *lcNomad) DrainNode(context.Context, string, bool, time.Duration) error { return nil }
func (f *lcNomad) GetAllocation(context.Context, string) (*nomad.Allocation, error) {
	return nil, nomad.ErrNotFound
}
func (f *lcNomad) JobEvaluations(context.Context, string) ([]nomad.Evaluation, error) {
	return nil, nil
}
func (f *lcNomad) Logs(context.Context, string, string, string, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *lcNomad) RegisterJobFile(context.Context, string) error { return nil }
func (f *lcNomad) Ping(context.Context) error                    { return nil }

// rendezvous lets up to n callers wait for each other, for at most d: with grove's per-job lock
// only one caller can get there, so it goes on alone after d.
func rendezvous(n int, d time.Duration) func() {
	var mu sync.Mutex
	arrived := 0
	all := make(chan struct{})
	return func() {
		mu.Lock()
		arrived++
		if arrived == n {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(d):
		}
	}
}

func newLCService(t *testing.T, nc *lcNomad) dispatch.Service {
	t.Helper()
	svc, err := dispatch.New(nc, dispatch.Options{
		StorePath:   filepath.Join(t.TempDir(), "jobs.json"),
		AttemptsDir: t.TempDir(),
		StatusTTL:   time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func submitLC(t *testing.T, svc dispatch.Service, nc *lcNomad) (*dispatch.Job, string) {
	t.Helper()
	job, _, err := svc.Submit(context.Background(), dispatch.JobRequest{Kind: dispatch.KindShell, Pool: "linux", Script: "true"})
	if err != nil {
		t.Fatal(err)
	}
	live := nc.live()
	if len(live) != 1 {
		t.Fatalf("after submit, live copies = %v, want one", live)
	}
	return job, live[0]
}

// lifecycleBefore, oneLiveCopy: the sweep and an API Get both reconcile a job whose allocation was
// lost. Both read the record, both see the loss, both read the copy's dispatch back, and both
// redispatch: two copies run, one of them unwatched.
func TestS24_ConcurrentReconcilesRerunALostJobOnce(t *testing.T) {
	nc := newLCNomad()
	svc := newLCService(t, nc)
	job, first := submitLC(t, svc, nc)
	nc.set(first, "running")
	if _, err := svc.Get(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	nc.set(first, "lost")
	nc.onDispatchInput = rendezvous(2, 300*time.Millisecond)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Get(context.Background(), job.ID)
		}()
	}
	wg.Wait()
	nc.onDispatchInput = nil

	live := nc.live()
	if len(live) != 1 || nc.dispatches() != 2 {
		t.Fatalf("live copies = %v after %d dispatches; want exactly one rerun", live, nc.dispatches())
	}
	// ... and it is the copy grove follows.
	nc.set(live[0], "running")
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != dispatch.StatusRunning || got.AllocID != live[0]+"/alloc" {
		t.Fatalf("job = %s on %q, want running on %s", got.Status, got.AllocID, live[0])
	}
}

// lifecycleBefore, endIsFinal: a reconcile reads the record, Cancel stops the copy and records
// the cancel, and the reconcile writes back what it read: the canceled job is "running" again.
func TestS24_CancelRacingAReconcileStaysCanceled(t *testing.T) {
	nc := newLCNomad()
	svc := newLCService(t, nc)
	job, first := submitLC(t, svc, nc)
	nc.set(first, "running")

	inList := make(chan struct{})
	canceled := make(chan struct{})
	var once sync.Once
	nc.onList = func() {
		once.Do(func() {
			close(inList)
			select {
			case <-canceled:
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
	reconciled := make(chan struct{})
	go func() {
		defer close(reconciled)
		_, _ = svc.Get(context.Background(), job.ID)
	}()
	<-inList
	cancelErr := make(chan error, 1)
	go func() { cancelErr <- svc.Cancel(context.Background(), job.ID) }()
	select {
	case err := <-cancelErr:
		close(canceled)
		if err != nil {
			t.Fatal(err)
		}
		<-reconciled
	case <-reconciled:
		if err := <-cancelErr; err != nil {
			t.Fatal(err)
		}
		close(canceled)
	}
	nc.onList = nil

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != dispatch.StatusCanceled {
		t.Fatalf("status after cancel = %s, want canceled", got.Status)
	}
	if live := nc.live(); len(live) != 0 {
		t.Fatalf("a canceled job still has live copies %v", live)
	}
}

// lifecycleBefore, endIsFinal: Cancel of a job that already finished turned its success into
// "canceled".
func TestS24_CancelOfAFinishedJobKeepsItsOutcome(t *testing.T) {
	nc := newLCNomad()
	svc := newLCService(t, nc)
	job, first := submitLC(t, svc, nc)
	nc.set(first, "exited")
	if got, err := svc.Get(context.Background(), job.ID); err != nil || got.Status != dispatch.StatusSuccess {
		t.Fatalf("Get = %v, %v; want success", got, err)
	}
	if err := svc.Cancel(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Get(context.Background(), job.ID); err != nil || got.Status != dispatch.StatusSuccess {
		t.Fatalf("after cancel, Get = %v, %v; want success", got.Status, err)
	}
}

// lifecycleBeforeAmbiguous, noUnwatchedCopy: Nomad answers a Dispatch with an error but made the
// copy (or makes it later). The caller is told the submit was refused; the copy must not run
// with nobody watching.
func TestS24_RefusedDispatchLeavesNoCopy(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed, then the call failed", true: "commits after the error"}[late], func(t *testing.T) {
			nc := newLCNomad()
			svc := newLCService(t, nc)
			nc.onDispatch = func() (bool, error) { return !late, errors.New("context deadline exceeded") }
			req := dispatch.JobRequest{Kind: dispatch.KindShell, Pool: "linux", Script: "true", IdempotencyKey: "k", SubmittedBy: "argos"}
			if _, _, err := svc.Submit(context.Background(), req); err == nil {
				t.Fatal("Submit succeeded, want the dispatch error")
			}
			nc.onDispatch = nil
			if late {
				nc.commitLate()
			}
			// The reconcile sweep looks again while the commit window is open.
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_ = svc.RunReconciler(ctx, 10*time.Millisecond)
			if live := nc.live(); len(live) != 0 {
				t.Fatalf("a refused submit left live copies %v", live)
			}
			// A retry under the same key is a new submit, not the refused one.
			job, created, err := svc.Submit(context.Background(), req)
			if err != nil || !created || job.Status != dispatch.StatusPending {
				t.Fatalf("retry = %v, created %v, %v; want a new pending job", job, created, err)
			}
		})
	}
}

// Two submits with one idempotency key, at once: both missed the lookup and both dispatched.
func TestS24_ConcurrentSubmitsWithOneKeyDispatchOnce(t *testing.T) {
	nc := newLCNomad()
	svc := newLCService(t, nc)
	meet := rendezvous(2, 300*time.Millisecond)
	nc.onDispatch = func() (bool, error) { meet(); return true, nil }
	req := dispatch.JobRequest{Kind: dispatch.KindShell, Pool: "linux", Script: "true", IdempotencyKey: "k", SubmittedBy: "argos"}
	ids := make([]string, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, _, err := svc.Submit(context.Background(), req)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = job.ID
		}()
	}
	wg.Wait()
	if nc.dispatches() != 1 || ids[0] != ids[1] {
		t.Fatalf("%d dispatches, jobs %v; want one job", nc.dispatches(), ids)
	}
}
