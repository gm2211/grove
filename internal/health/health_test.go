package health

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/snooze"
)

type fakeJobs struct {
	dispatch.Service
	jobs []dispatch.Job
}

func (f *fakeJobs) List(context.Context) ([]dispatch.Job, error) { return f.jobs, nil }

type fakeOrchard struct {
	orchard.Client
	vms     map[string]string // name -> uid
	workers []orchard.Worker
	deleted []string
	paused  []string
}

func (o *fakeOrchard) GetVM(_ context.Context, name string) (*orchard.VM, error) {
	uid, ok := o.vms[name]
	if !ok {
		return nil, nil
	}
	return &orchard.VM{Name: name, UID: uid}, nil
}
func (o *fakeOrchard) DeleteVM(_ context.Context, name string) error {
	o.deleted = append(o.deleted, name)
	return nil
}
func (o *fakeOrchard) ListWorkers(context.Context) ([]orchard.Worker, error) { return o.workers, nil }
func (o *fakeOrchard) PauseWorker(_ context.Context, name string) error {
	o.paused = append(o.paused, name)
	for i := range o.workers {
		if o.workers[i].Name == name {
			o.workers[i].SchedulingPaused = true
		}
	}
	return nil
}

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func attempt(vm, machine, outcome string, ago time.Duration) dispatch.Attempt {
	end := now.Add(-ago)
	return dispatch.Attempt{VM: vm, VMUID: "uid-" + vm, Worker: machine, StartedAt: end.Add(-time.Minute), FinishedAt: &end, Outcome: outcome}
}

func job(id string, attempts ...dispatch.Attempt) dispatch.Job {
	return dispatch.Job{ID: id, Attempts: attempts}
}

func newMonitor(jobs []dispatch.Job) (*Monitor, *fakeOrchard) {
	o := &fakeOrchard{
		vms:     map[string]string{"linux-mac1-0": "uid-linux-mac1-0", "linux-mac1-1": "uid-linux-mac1-1", "linux-mac2-0": "uid-linux-mac2-0"},
		workers: []orchard.Worker{{Name: "mac1"}, {Name: "mac2"}},
	}
	return &Monitor{
		Jobs: &fakeJobs{jobs: jobs}, Orchard: o,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now },
	}, o
}

func TestRebuildsAVMThatKeepsLosingJobs(t *testing.T) {
	m, o := newMonitor([]dispatch.Job{
		job("a", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 30*time.Minute)),
		job("b", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 20*time.Minute)),
		job("c", attempt("linux-mac2-0", "mac2", dispatch.AttemptLost, 20*time.Minute)),
	})
	m.Tick(context.Background())
	if !slices.Equal(o.deleted, []string{"linux-mac1-0"}) {
		t.Fatalf("deleted = %v, want only the VM that lost two jobs", o.deleted)
	}
	m.Tick(context.Background())
	if len(o.deleted) != 1 {
		t.Fatalf("deleted again: %v", o.deleted)
	}
	if len(o.paused) != 0 {
		t.Fatalf("paused %v: one bad VM is not a bad machine", o.paused)
	}
}

func TestLeavesAReplacedVMAlone(t *testing.T) {
	m, o := newMonitor([]dispatch.Job{
		job("a", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 30*time.Minute)),
		job("b", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 20*time.Minute)),
	})
	o.vms["linux-mac1-0"] = "uid-recreated"
	m.Tick(context.Background())
	if len(o.deleted) != 0 {
		t.Fatalf("deleted the VM that replaced the bad one: %v", o.deleted)
	}
}

func TestAFinishedJobClearsTheSlate(t *testing.T) {
	m, o := newMonitor([]dispatch.Job{
		job("a", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 30*time.Minute)),
		job("ok", attempt("linux-mac1-0", "mac1", dispatch.AttemptExited, 25*time.Minute)),
		job("b", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 20*time.Minute)),
		// Too long ago to count.
		job("old", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 3*time.Hour)),
	})
	m.Tick(context.Background())
	if len(o.deleted) != 0 {
		t.Fatalf("deleted %v though a job finished in the VM between the losses", o.deleted)
	}
}

func TestPausesAMachineWhoseVMsKeepLosingJobs(t *testing.T) {
	m, o := newMonitor([]dispatch.Job{
		job("a", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 40*time.Minute)),
		job("b", attempt("linux-mac1-1", "mac1", dispatch.AttemptLost, 30*time.Minute)),
		job("c", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 20*time.Minute)),
	})
	m.Tick(context.Background())
	if !slices.Equal(o.paused, []string{"mac1"}) {
		t.Fatalf("paused = %v, want mac1", o.paused)
	}
	// An operator resumes it: the same losses don't pause it again.
	o.workers[0].SchedulingPaused = false
	m.Tick(context.Background())
	if len(o.paused) != 1 {
		t.Fatalf("paused again for the same losses: %v", o.paused)
	}
}

func TestQuarantinedJobsSayNothingAboutMachines(t *testing.T) {
	poison := job("poison", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 30*time.Minute), attempt("linux-mac1-1", "mac1", dispatch.AttemptLost, 20*time.Minute))
	poison.Quarantined = true
	m, o := newMonitor([]dispatch.Job{
		poison,
		job("a", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 25*time.Minute)),
	})
	m.Tick(context.Background())
	if len(o.deleted) != 0 || len(o.paused) != 0 {
		t.Fatalf("acted on a quarantined job's losses: deleted %v paused %v", o.deleted, o.paused)
	}
}

func lossyJobs() []dispatch.Job {
	return []dispatch.Job{
		job("a", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 40*time.Minute)),
		job("b", attempt("linux-mac1-1", "mac1", dispatch.AttemptLost, 30*time.Minute)),
		job("c", attempt("linux-mac1-0", "mac1", dispatch.AttemptLost, 20*time.Minute)),
	}
}

func newStore(t *testing.T) *snooze.Store {
	t.Helper()
	st, err := snooze.NewStore(filepath.Join(t.TempDir(), "pauses.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Evictions are grove moving a job off a snoozed Mac, not the Mac losing it.
func TestEvictedAttemptsSayNothingAboutMachines(t *testing.T) {
	m, o := newMonitor([]dispatch.Job{
		job("a", attempt("linux-mac1-0", "mac1", dispatch.AttemptEvicted, 40*time.Minute)),
		job("b", attempt("linux-mac1-1", "mac1", dispatch.AttemptEvicted, 30*time.Minute)),
		job("c", attempt("linux-mac1-0", "mac1", dispatch.AttemptEvicted, 20*time.Minute)),
		job("d", attempt("linux-mac1-1", "mac1", dispatch.AttemptEvicted, 10*time.Minute)),
	})
	m.Tick(context.Background())
	if len(o.paused) != 0 || len(o.deleted) != 0 {
		t.Fatalf("evictions acted on: paused=%v deleted=%v", o.paused, o.deleted)
	}
}

func TestHealthPauseIsRecordedInTheStore(t *testing.T) {
	m, o := newMonitor(lossyJobs())
	st := newStore(t)
	m.Pauses = st
	m.Tick(context.Background())
	if !slices.Equal(o.paused, []string{"mac1"}) {
		t.Fatalf("paused = %v", o.paused)
	}
	if e := st.Get("mac1"); e.Health == nil || e.Health.Jobs != 3 {
		t.Fatalf("entry = %+v", e)
	}
}

// A Mac the owner snoozed still gets its health reason recorded, so the snooze ending does not
// put it back to work.
func TestSnoozedMachineStillRecordsHealthReason(t *testing.T) {
	m, o := newMonitor(lossyJobs())
	o.workers[0].SchedulingPaused = true
	st := newStore(t)
	if err := st.SetSnooze("mac1", snooze.Snooze{Until: now.Add(time.Hour), Mode: snooze.ModeDrain, At: now}); err != nil {
		t.Fatal(err)
	}
	m.Pauses = st
	m.Tick(context.Background())
	if len(o.paused) != 0 {
		t.Fatalf("Orchard was called for an already-paused machine: %v", o.paused)
	}
	if e := st.Get("mac1"); e.Health == nil {
		t.Fatalf("health reason not recorded: %+v", e)
	}
	if got := st.Reasons("mac1", now.Add(2*time.Hour)); !slices.Equal(got, []string{snooze.ReasonHealth}) {
		t.Fatalf("after the snooze: reasons = %v", got)
	}
}
