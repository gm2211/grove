package conformance

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/snooze"
)

// fakePauser is one Orchard worker's pause bit. ListWorkers can be held (block) to stretch a
// writer's critical section, and ResumeWorker can be made to fail once.
type fakePauser struct {
	mu         sync.Mutex
	paused     bool
	block      chan struct{} // when set, ListWorkers waits for it to close
	listing    chan struct{} // closed when ListWorkers is first entered
	failResume int
	calls      []string
}

func (f *fakePauser) ListWorkers(context.Context) ([]orchard.Worker, error) {
	f.mu.Lock()
	block, listing := f.block, f.listing
	f.listing = nil
	f.mu.Unlock()
	if listing != nil {
		close(listing)
	}
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return []orchard.Worker{{Name: "mac1", SchedulingPaused: f.paused}}, nil
}

func (f *fakePauser) PauseWorker(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
	f.calls = append(f.calls, "pause")
	return nil
}

func (f *fakePauser) ResumeWorker(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failResume > 0 {
		f.failResume--
		return errors.New("orchard unavailable")
	}
	f.paused = false
	f.calls = append(f.calls, "resume")
	return nil
}

func newPauseStore(t *testing.T) *snooze.Store {
	t.Helper()
	st, err := snooze.NewStore(filepath.Join(t.TempDir(), "pauses.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// S1 and L1 (spec/quint/snooze.qnt, the counterexample TLC found without the lock): a writer
// that runs while another is between its Orchard read and its last write must wait for it.
// Here Wake arrives while Engage has read Orchard but not yet recorded the snooze.
func TestS1_WritersHoldTheWorkersLock(t *testing.T) {
	st := newPauseStore(t)
	p := &fakePauser{block: make(chan struct{}), listing: make(chan struct{})}
	listing, block := p.listing, p.block
	now := time.Now()

	engaged := make(chan error, 1)
	go func() {
		engaged <- snooze.Engage(context.Background(), p, st, "mac1", snooze.Snooze{Until: now.Add(time.Hour), At: now}, now)
	}()
	<-listing // Engage is inside its critical section

	woke := make(chan struct{})
	go func() {
		_, _, _ = snooze.Wake(context.Background(), p, st, "mac1", now)
		close(woke)
	}()
	select {
	case <-woke:
		t.Fatal("Wake ran while Engage was between reading Orchard and recording the snooze")
	case <-time.After(50 * time.Millisecond):
	}
	p.mu.Lock()
	p.block = nil
	p.mu.Unlock()
	close(block) // let Engage finish
	if err := <-engaged; err != nil {
		t.Fatal(err)
	}
	<-woke
	// Serialized: the snooze was engaged, then ended, and the worker is working again.
	if e := st.Get("mac1"); e.Snooze != nil || p.paused {
		t.Fatalf("after Engage then Wake: snooze %v, paused %v", e.Snooze, p.paused)
	}
}

// L2 (spec/quint/snooze.qnt, snoozeExpiryFailsBefore): an Orchard resume that fails when a snooze
// lapses leaves the snooze recorded, so the next Expirer pass resumes the worker. Before, the
// snooze was cleared first and the failed resume left a pause no reason accounts for, forever.
func TestL2_FailedResumeAfterSnoozeIsRetried(t *testing.T) {
	st := newPauseStore(t)
	p := &fakePauser{paused: true, failResume: 1}
	now := time.Now()
	if err := st.SetSnooze("mac1", snooze.Snooze{Until: now.Add(-time.Minute), At: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	x := &snooze.Expirer{Store: st, Orchard: p, Now: func() time.Time { return now }}
	if err := x.Tick(context.Background()); err == nil {
		t.Fatal("first pass: want the resume error")
	}
	if !p.paused || st.Get("mac1").Snooze == nil {
		t.Fatalf("after a failed resume: paused %v, snooze %v; want still paused with the snooze kept", p.paused, st.Get("mac1").Snooze)
	}
	if err := x.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.paused || st.Get("mac1").Snooze != nil {
		t.Fatalf("after the retry: paused %v, snooze %v; want working with no reason", p.paused, st.Get("mac1").Snooze)
	}
}
