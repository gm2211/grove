package dispatch

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeExec is an Executor the test drives step by step: every Execute registers a fakeRun and then
// does only what the test tells it to through that run's channels.
type fakeExec struct {
	mu        sync.Mutex
	runs      []*fakeRun
	abandoned []Placement
	notify    chan *fakeRun
	// onCancel is what Execute returns once its ctx is done (default ExitCanceled).
	onCancel int
}

type fakeRun struct {
	run     *Run
	ctx     context.Context
	place   chan string
	exit    chan int
	writeFn chan func(*Run)
	done    chan struct{}
}

func newFakeExec() *fakeExec {
	return &fakeExec{notify: make(chan *fakeRun, 64), onCancel: ExitCanceled}
}

func (f *fakeExec) Execute(ctx context.Context, run *Run) int {
	fr := &fakeRun{run: run, ctx: ctx, place: make(chan string, 1), exit: make(chan int, 1), writeFn: make(chan func(*Run), 16), done: make(chan struct{})}
	defer close(fr.done)
	f.mu.Lock()
	f.runs = append(f.runs, fr)
	f.mu.Unlock()
	f.notify <- fr
	for {
		select {
		case vm := <-fr.place:
			run.Started(placementOf(vm))
		case fn := <-fr.writeFn:
			fn(run)
		case code := <-fr.exit:
			return code
		case <-ctx.Done():
			return f.onCancel
		}
	}
}

func (f *fakeExec) Abandon(_ context.Context, p Placement) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abandoned = append(f.abandoned, p)
	return nil
}

// next waits for the next Execute call.
func (f *fakeExec) next(t *testing.T) *fakeRun {
	t.Helper()
	select {
	case fr := <-f.notify:
		return fr
	case <-time.After(5 * time.Second):
		t.Fatal("no job reached the executor")
		return nil
	}
}

// none asserts no further Execute call arrives for a little while.
func (f *fakeExec) none(t *testing.T) {
	t.Helper()
	select {
	case fr := <-f.notify:
		t.Fatalf("job %s reached the executor, want it still queued", fr.run.ID)
	case <-time.After(50 * time.Millisecond):
	}
}

// write runs fn on the executor's goroutine (as a real executor writes output) and waits for it.
func (fr *fakeRun) write(t *testing.T, fn func(*Run)) {
	t.Helper()
	done := make(chan struct{})
	fr.writeFn <- func(r *Run) { fn(r); close(done) }
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not write")
	}
}

func (fr *fakeRun) finish(t *testing.T, code int) {
	t.Helper()
	fr.exit <- code
	select {
	case <-fr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not return")
	}
}

func testPools() []PoolConfig {
	return []PoolConfig{
		{Name: "linux", RunnerImage: DefaultRunnerImage, CPU: 2000, Memory: 4096, Slots: 2, OS: "linux"},
		{Name: "macos", CPU: 500, Memory: 1024, Slots: 1, OS: "darwin"},
	}
}

func newTestServiceWith(t *testing.T, exec Executor, opts Options) *service {
	t.Helper()
	if opts.StorePath == "" {
		opts.StorePath = filepath.Join(t.TempDir(), "jobs.json")
	}
	if opts.Pools == nil {
		opts.Pools = testPools()
	}
	if opts.LogPollInterval == 0 {
		opts.LogPollInterval = 5 * time.Millisecond
	}
	svc, err := New(exec, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := svc.(*service)
	t.Cleanup(s.Close)
	return s
}

func newTestService(t *testing.T, exec *fakeExec) *service {
	t.Helper()
	return newTestServiceWith(t, exec, Options{})
}

// waitStatus polls job id until it has status want.
func waitStatus(t *testing.T, svc Service, id string, want Status) *Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := svc.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if job.Status == want {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s status = %s, want %s", id, job.Status, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func shellJob(pool string) JobRequest {
	return JobRequest{Kind: KindShell, Pool: pool, Script: "true", Requester: "test"}
}

func submit(t *testing.T, svc Service, req JobRequest) *Job {
	t.Helper()
	job, created, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !created {
		t.Fatal("Submit: created = false")
	}
	return job
}

func ptrTime(t time.Time) *time.Time { return &t }

func mustf(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
}
