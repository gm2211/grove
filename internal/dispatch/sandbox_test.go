package dispatch

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/sandbox"
)

// vmlessOrchard has no VMs at all, and records deletes.
type vmlessOrchard struct {
	orchard.Client
	mu      sync.Mutex
	deleted []string
}

func (o *vmlessOrchard) ListVMs(context.Context) ([]orchard.VM, error) { return nil, nil }

func (o *vmlessOrchard) DeleteVM(_ context.Context, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deleted = append(o.deleted, name)
	return nil
}

func TestSandboxExecutor_SlotsPerVMAndExitCodes(t *testing.T) {
	for _, c := range []struct {
		pool  PoolConfig
		perVM int
	}{
		{PoolConfig{Name: "linux", Slots: 3, OS: sandbox.OSLinux}, 3},
		{PoolConfig{Name: "macos", Slots: 3, OS: sandbox.OSDarwin}, 1},
	} {
		t.Run(c.pool.Name, func(t *testing.T) {
			var tuned *sandbox.Runner
			e := &SandboxExecutor{Orchard: &vmlessOrchard{}, SlotDir: t.TempDir(), Tune: func(r *sandbox.Runner) {
				tuned = r
				r.SlotWait = time.Millisecond
				r.NoVMTimeout = 20 * time.Millisecond
			}}
			var stderr bytes.Buffer
			run := &Run{ID: "0123456789abcdef", Pool: c.pool, Request: JobRequest{Kind: KindShell, Pool: c.pool.Name, Script: "true"}, Stdout: &bytes.Buffer{}, Stderr: &stderr}
			if code := e.Execute(context.Background(), run); code != ExitLost {
				t.Fatalf("Execute with no VM = %d, want %d; log:\n%s", code, ExitLost, stderr.String())
			}
			if tuned.Slots.PerVM != c.perVM {
				t.Fatalf("PerVM = %d, want %d", tuned.Slots.PerVM, c.perVM)
			}
			if !strings.Contains(stderr.String(), "no sealed VM of pool "+c.pool.Name) {
				t.Fatalf("the job's log doesn't say why: %q", stderr.String())
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			e.Tune = func(r *sandbox.Runner) { r.SlotWait = time.Millisecond }
			if code := e.Execute(ctx, run); code != ExitCanceled {
				t.Fatalf("canceled Execute = %d, want %d", code, ExitCanceled)
			}
		})
	}
}

func TestSandboxExecutor_AbandonDeletesTheVM(t *testing.T) {
	o := &vmlessOrchard{}
	e := &SandboxExecutor{Orchard: o}
	if err := e.Abandon(context.Background(), Placement{}); err != nil || len(o.deleted) != 0 {
		t.Fatalf("Abandon without a VM = %v, deleted %v", err, o.deleted)
	}
	if err := e.Abandon(context.Background(), placementOf("linux-mac1-0")); err != nil || len(o.deleted) != 1 || o.deleted[0] != "linux-mac1-0" {
		t.Fatalf("Abandon = %v, deleted %v", err, o.deleted)
	}
}
