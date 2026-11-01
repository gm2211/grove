package snooze

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

type fakeOrchard struct {
	paused map[string]bool
	calls  []string
}

func newFake(paused ...string) *fakeOrchard {
	f := &fakeOrchard{paused: map[string]bool{"mac": false, "other": false}}
	for _, p := range paused {
		f.paused[p] = true
	}
	return f
}

func (f *fakeOrchard) ListWorkers(context.Context) ([]orchard.Worker, error) {
	var out []orchard.Worker
	for n, p := range f.paused {
		out = append(out, orchard.Worker{Name: n, SchedulingPaused: p})
	}
	return out, nil
}
func (f *fakeOrchard) PauseWorker(_ context.Context, n string) error {
	f.paused[n] = true
	f.calls = append(f.calls, "pause "+n)
	return nil
}
func (f *fakeOrchard) ResumeWorker(_ context.Context, n string) error {
	f.paused[n] = false
	f.calls = append(f.calls, "resume "+n)
	return nil
}

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func TestStoreRoundTripAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pauses.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if err := st.SetSnooze("mac", Snooze{Until: t0.Add(time.Hour), Mode: ModeNow, By: "me", At: t0, Note: "zoom"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetHealth("mac", t0, 3); err != nil {
		t.Fatal(err)
	}
	// restart keeps state
	again, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	e := again.Get("mac")
	if e.Snooze == nil || e.Snooze.Mode != ModeNow || e.Snooze.Note != "zoom" || e.Health == nil || e.Health.Jobs != 3 {
		t.Fatalf("lost state: %+v", e)
	}
	if got := again.Reasons("mac", t0); len(got) != 2 || got[0] != ReasonSnooze || got[1] != ReasonHealth {
		t.Fatalf("reasons = %v", got)
	}
	if got := again.Reasons("mac", t0.Add(2*time.Hour)); len(got) != 1 || got[0] != ReasonHealth {
		t.Fatalf("expired snooze still a reason: %v", got)
	}
	ro, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if ro.Get("mac").Snooze == nil {
		t.Fatal("Load lost state")
	}
	if ro.SetOperator("mac", "x", t0) == nil {
		t.Fatal("read-only store accepted a write")
	}
	if err := st.SetSnooze("mac", Snooze{Until: t0, Mode: "bogus"}); err == nil {
		t.Fatal("bogus mode accepted")
	}
}

func TestStoreRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "pauses.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(link); err == nil {
		t.Fatal("NewStore followed a symlink")
	}
	if _, err := Load(link); err == nil {
		t.Fatal("Load followed a symlink")
	}
}

func TestMissingFileLoadsEmpty(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || len(st.List()) != 0 {
		t.Fatalf("%v %v", st, err)
	}
	var nilStore *Store
	if nilStore.Get("x").Snooze != nil || len(nilStore.List()) != 0 {
		t.Fatal("nil store not empty")
	}
}

func TestExpiryResumesOnlyWithNoOtherReason(t *testing.T) {
	ctx := context.Background()
	st, _ := NewStore(filepath.Join(t.TempDir(), "p.json"))
	fo := newFake()
	if err := Engage(ctx, fo, st, "mac", Snooze{Until: t0.Add(time.Hour), At: t0}, t0); err != nil {
		t.Fatal(err)
	}
	if !fo.paused["mac"] {
		t.Fatal("not paused")
	}
	if err := st.SetHealth("mac", t0, 3); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSnooze("other", Snooze{Until: t0.Add(time.Minute), At: t0}); err != nil {
		t.Fatal(err)
	}
	fo.paused["other"] = true
	ex := &Expirer{Store: st, Orchard: fo, Now: func() time.Time { return t0.Add(2 * time.Hour) }}
	if err := ex.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !fo.paused["mac"] {
		t.Fatal("snooze expiry resumed a health-paused worker")
	}
	if st.Get("mac").Snooze != nil {
		t.Fatal("expired snooze kept")
	}
	if fo.paused["other"] {
		t.Fatal("snooze-only worker was not resumed")
	}
	if len(st.List()) != 1 {
		t.Fatalf("empty entries kept: %v", st.List())
	}
}

func TestExternalPauseNeverResumed(t *testing.T) {
	ctx := context.Background()
	st, _ := NewStore(filepath.Join(t.TempDir(), "p.json"))
	fo := newFake("mac")
	if err := Engage(ctx, fo, st, "mac", Snooze{Until: t0.Add(time.Hour), At: t0}, t0); err != nil {
		t.Fatal(err)
	}
	if e := st.Get("mac"); e.Operator == nil || e.Operator.By != ExternalBy {
		t.Fatalf("external pause not recorded: %+v", e)
	}
	resumed, still, err := Wake(ctx, fo, st, "mac", t0)
	if err != nil || resumed || len(still) != 1 || still[0] != ReasonOperator {
		t.Fatalf("wake = %v %v %v", resumed, still, err)
	}
	if !fo.paused["mac"] {
		t.Fatal("wake resumed an external pause")
	}
}

func TestPauseReapplied(t *testing.T) {
	ctx := context.Background()
	st, _ := NewStore(filepath.Join(t.TempDir(), "p.json"))
	fo := newFake()
	_ = Engage(ctx, fo, st, "mac", Snooze{Until: t0.Add(time.Hour), At: t0}, t0)
	fo.paused["mac"] = false // someone clicked resume in Orchard
	ex := &Expirer{Store: st, Orchard: fo, Now: func() time.Time { return t0.Add(time.Minute) }}
	if err := ex.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !fo.paused["mac"] {
		t.Fatal("pause not re-applied")
	}
}

func TestWakeResumesWhenNothingElse(t *testing.T) {
	ctx := context.Background()
	st, _ := NewStore(filepath.Join(t.TempDir(), "p.json"))
	fo := newFake()
	_ = Engage(ctx, fo, st, "mac", Snooze{Until: t0.Add(time.Hour), At: t0}, t0)
	resumed, still, err := Wake(ctx, fo, st, "mac", t0)
	if err != nil || !resumed || len(still) != 0 || fo.paused["mac"] {
		t.Fatalf("%v %v %v", resumed, still, err)
	}
	if err := Engage(ctx, fo, st, "ghost", Snooze{Until: t0.Add(time.Hour)}, t0); err == nil {
		t.Fatal("unknown worker accepted")
	}
}
