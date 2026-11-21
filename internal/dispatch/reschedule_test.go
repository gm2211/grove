package dispatch

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

// rescheduleFixture is a shared linux pool on two Macs, w1 and w2, one VM each.
type rescheduleFixture struct {
	t   *testing.T
	nc  *fakeNomad
	svc *service
	n   int // dispatches so far
}

func newRescheduleFixture(t *testing.T) *rescheduleFixture {
	t.Helper()
	nc := &fakeNomad{nodesByID: map[string]nomad.Node{
		"n1": {ID: "n1", Meta: map[string]string{"pool": "linux", "host": "w1", "vm": "linux-w1-0"}},
		"n2": {ID: "n2", Meta: map[string]string{"pool": "linux", "host": "w2", "vm": "linux-w2-0"}},
	}}
	svc := newTestServiceWithOpts(t, nc, Options{Pools: []PoolConfig{{Name: "linux"}}}).(*service)
	return &rescheduleFixture{t: t, nc: nc, svc: svc}
}

// submit dispatches req; its Nomad job is "<name>/dispatch-1".
func (f *rescheduleFixture) submit(req JobRequest) string {
	f.t.Helper()
	f.n++
	f.nc.dispatchResult = &nomad.DispatchResult{DispatchedJobID: jobName(req.Kind, req.Pool) + "/dispatch-" + strconv.Itoa(f.n)}
	job, _, err := f.svc.Submit(context.Background(), req)
	if err != nil {
		f.t.Fatal(err)
	}
	// The next redispatch gets the next id.
	f.nc.dispatchResult = &nomad.DispatchResult{DispatchedJobID: jobName(req.Kind, req.Pool) + "/dispatch-" + strconv.Itoa(f.n+1)}
	return job.ID
}

// nomadJob is the Nomad job rec id currently points at.
func (f *rescheduleFixture) nomadJob(id string) string {
	rec, ok := f.svc.store.get(id)
	if !ok {
		f.t.Fatalf("no job %s", id)
	}
	return rec.NomadJobID
}

// allocs sets the allocations of id's current Nomad job.
func (f *rescheduleFixture) allocs(id string, allocs ...nomad.Allocation) {
	if f.nc.allocationsByJob == nil {
		f.nc.allocationsByJob = map[string][]nomad.Allocation{}
	}
	f.nc.allocationsByJob[f.nomadJob(id)] = allocs
}

// get reconciles id afresh (no status cache) and returns the job.
func (f *rescheduleFixture) get(id string) *Job {
	f.t.Helper()
	f.svc.mu.Lock()
	delete(f.svc.cache, id)
	f.svc.mu.Unlock()
	job, err := f.svc.Get(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return job
}

// lostOn makes id's current Nomad job lose its allocation on node, after the script started
// when started is true; then reconciles. Nomad's own replacement is not placed (no node).
func (f *rescheduleFixture) lostOn(id, alloc, node string, started bool) *Job {
	f.t.Helper()
	f.allocs(id, nomad.Allocation{ID: alloc, NodeID: node, ClientStatus: "lost", Started: started, CreatedAt: time.Now()})
	f.nc.dispatchResult = &nomad.DispatchResult{DispatchedJobID: f.nomadJob(id) + "+"}
	return f.get(id)
}

func lostReq(script string) JobRequest {
	return JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://github.com/x/y", Ref: "main", Script: script}
}

func TestReschedule_LostBuildJobRunsAgainAnywhereInItsPool(t *testing.T) {
	f := newRescheduleFixture(t)
	id := f.submit(lostReq("make"))
	first := f.nomadJob(id)
	original := f.nc.dispatchCalls[0]

	job := f.lostOn(id, "a1", "n1", true)
	if job.Status != StatusPending || !strings.HasPrefix(job.PendingReason, "rescheduled: lost on w1") {
		t.Fatalf("after one loss: status=%s pending=%q", job.Status, job.PendingReason)
	}
	if len(job.Attempts) != 1 || job.Attempts[0].Outcome != AttemptLost || job.Attempts[0].Worker != "w1" ||
		job.Attempts[0].VM != "linux-w1-0" || !job.Attempts[0].JobStarted || job.Attempts[0].Alloc != "a1" {
		t.Errorf("attempts = %+v", job.Attempts)
	}
	if job.Placement != nil || job.FailureReason != nil || job.FinishedAt != nil {
		t.Errorf("requeued job keeps its old run: placement=%+v failure=%v finished=%v", job.Placement, job.FailureReason, job.FinishedAt)
	}
	if len(f.nc.dispatchCalls) != 2 {
		t.Fatalf("dispatches = %d, want one redispatch", len(f.nc.dispatchCalls))
	}
	again := f.nc.dispatchCalls[1]
	if again.jobName != original.jobName || string(again.payload) != string(original.payload) || len(again.meta) != len(original.meta) {
		t.Errorf("redispatch differs from the first dispatch: %+v vs %+v", again, original)
	}
	for k, v := range again.meta {
		if strings.Contains(v, "w1") || strings.Contains(k, "host") {
			t.Errorf("redispatch pins the lost worker: %s=%s", k, v)
		}
	}
	if len(f.nc.stopCalls) != 1 || f.nc.stopCalls[0].jobID != first || !f.nc.stopCalls[0].purge {
		t.Errorf("stops = %+v, want the lost copy stopped and purged", f.nc.stopCalls)
	}
	if f.nomadJob(id) == first {
		t.Errorf("record still points at the lost Nomad job")
	}

	// The rerun finishes on w2: the job succeeds, and both attempts are kept.
	f.allocs(id, nomad.Allocation{ID: "a2", NodeID: "n2", ClientStatus: "complete", ExitCode: new(int), Started: true, CreatedAt: time.Now()})
	job = f.get(id)
	if job.Status != StatusSuccess || len(job.Attempts) != 2 || job.Attempts[1].Outcome != AttemptExited || job.Attempts[1].Worker != "w2" {
		t.Errorf("after the rerun: status=%s attempts=%+v", job.Status, job.Attempts)
	}
}

func TestReschedule_LostOnTwoMachinesIsQuarantined(t *testing.T) {
	f := newRescheduleFixture(t)
	id := f.submit(lostReq("crash-the-mac"))
	f.lostOn(id, "a1", "n1", true)
	current := f.nomadJob(id)
	job := f.lostOn(id, "a2", "n2", true)
	if job.Status != StatusFailed || !job.Quarantined || job.FailureReason == nil ||
		!strings.Contains(*job.FailureReason, "2 different machines (w1, w2)") {
		t.Fatalf("status=%s quarantined=%v reason=%v", job.Status, job.Quarantined, job.FailureReason)
	}
	if len(f.nc.dispatchCalls) != 2 {
		t.Errorf("dispatches = %d, a quarantined job must not run again", len(f.nc.dispatchCalls))
	}
	last := f.nc.stopCalls[len(f.nc.stopCalls)-1]
	if last.jobID != current || last.purge {
		t.Errorf("last stop = %+v, want %s stopped (kept, not purged) so Nomad can't run it", last, current)
	}
}

func TestReschedule_BudgetEndsItLostAfterMaxAttempts(t *testing.T) {
	f := newRescheduleFixture(t)
	id := f.submit(lostReq("flaky"))
	var job *Job
	for i := 0; i < MaxAttempts; i++ {
		job = f.lostOn(id, "a"+strconv.Itoa(i), "n1", true) // one Mac: never two machines
	}
	if job.Status != StatusLost || job.Quarantined || job.FailureReason == nil || !strings.Contains(*job.FailureReason, "run 3 times") {
		t.Fatalf("status=%s quarantined=%v reason=%v", job.Status, job.Quarantined, job.FailureReason)
	}
	if len(f.nc.dispatchCalls) != MaxAttempts {
		t.Errorf("dispatches = %d, want %d runs in all", len(f.nc.dispatchCalls), MaxAttempts)
	}
	if job := f.get(id); job.Status != StatusLost {
		t.Errorf("ended job changed: %s", job.Status)
	}
}

func TestReschedule_LossesBeforeTheScriptStartedNeverQuarantine(t *testing.T) {
	f := newRescheduleFixture(t)
	id := f.submit(lostReq("make"))
	f.lostOn(id, "a1", "n1", false)
	job := f.lostOn(id, "a2", "n2", false)
	if job.Status != StatusPending || job.Quarantined {
		t.Fatalf("status=%s quarantined=%v: a VM lost during setup says nothing about the job", job.Status, job.Quarantined)
	}
}

func TestReschedule_AgentJobEndsLostAndItsNomadJobIsStopped(t *testing.T) {
	f := newRescheduleFixture(t)
	id := f.submit(JobRequest{Kind: KindAgent, Pool: "linux", Repo: "https://github.com/x/y", Script: "claude"})
	current := f.nomadJob(id)
	job := f.lostOn(id, "a1", "n1", true)
	if job.Status != StatusLost || job.FailureReason == nil || !strings.Contains(*job.FailureReason, "does not allow a rerun") {
		t.Fatalf("status=%s reason=%v", job.Status, job.FailureReason)
	}
	if len(f.nc.dispatchCalls) != 1 {
		t.Errorf("an agent job was run again")
	}
	// Nomad would otherwise place its own replacement the moment a node frees up, unwatched.
	if len(f.nc.stopCalls) != 1 || f.nc.stopCalls[0].jobID != current {
		t.Errorf("stops = %+v, want %s stopped", f.nc.stopCalls, current)
	}
}

func TestReschedule_NomadsReplacementIsTheRerunAndStaysInBudget(t *testing.T) {
	f := newRescheduleFixture(t)
	id := f.submit(lostReq("make"))
	t0 := time.Now()
	lost := nomad.Allocation{ID: "a1", NodeID: "n1", ClientStatus: "lost", Started: true, CreatedAt: t0}
	f.allocs(id, lost, nomad.Allocation{ID: "a2", NodeID: "n2", ClientStatus: "running", Started: true, CreatedAt: t0.Add(time.Second)})
	job := f.get(id)
	if job.Status != StatusRunning || len(job.Attempts) != 1 || len(f.nc.dispatchCalls) != 1 {
		t.Fatalf("status=%s attempts=%d dispatches=%d: Nomad's replacement should carry on", job.Status, len(job.Attempts), len(f.nc.dispatchCalls))
	}
	// Seen again: the same loss is not counted twice.
	if job = f.get(id); len(job.Attempts) != 1 {
		t.Errorf("attempts = %+v", job.Attempts)
	}
	// The replacement is lost on the other Mac too: quarantined, and Nomad's job stopped so it
	// can't place a third copy.
	f.allocs(id, lost, nomad.Allocation{ID: "a2", NodeID: "n2", ClientStatus: "lost", Started: true, CreatedAt: t0.Add(time.Second)},
		nomad.Allocation{ID: "a3", NodeID: "n1", ClientStatus: "pending", CreatedAt: t0.Add(2 * time.Second)})
	job = f.get(id)
	if job.Status != StatusFailed || !job.Quarantined {
		t.Fatalf("status=%s quarantined=%v", job.Status, job.Quarantined)
	}
	if len(f.nc.stopCalls) == 0 || f.nc.stopCalls[len(f.nc.stopCalls)-1].jobID != f.nomadJob(id) {
		t.Errorf("stops = %+v", f.nc.stopCalls)
	}
}

func TestReschedule_AResubmittedPoisonPillIsQuarantinedOnItsFirstLoss(t *testing.T) {
	f := newRescheduleFixture(t)
	first := f.submit(lostReq("crash-the-mac"))
	f.lostOn(first, "a1", "n1", true)
	if job := f.lostOn(first, "a2", "n2", true); !job.Quarantined {
		t.Fatal("first copy not quarantined")
	}

	again := f.submit(lostReq("crash-the-mac"))
	before := len(f.nc.dispatchCalls)
	job := f.lostOn(again, "b1", "n1", true)
	if job.Status != StatusFailed || !job.Quarantined || !strings.Contains(*job.FailureReason, "same repo, ref and script") {
		t.Fatalf("status=%s quarantined=%v reason=%v", job.Status, job.Quarantined, job.FailureReason)
	}
	if len(f.nc.dispatchCalls) != before {
		t.Errorf("the resubmitted poison pill was run again")
	}

	// A different script is a different job: it gets its reruns.
	other := f.submit(lostReq("make test"))
	if job := f.lostOn(other, "c1", "n1", true); job.Status != StatusPending {
		t.Errorf("unrelated job: status=%s", job.Status)
	}
}

func TestReschedule_PoisonMemoryFades(t *testing.T) {
	f := newRescheduleFixture(t)
	first := f.submit(lostReq("crash-the-mac"))
	f.lostOn(first, "a1", "n1", true)
	f.lostOn(first, "a2", "n2", true)
	rec, _ := f.svc.store.get(first)
	old := time.Now().Add(-PoisonMemory - time.Minute)
	rec.FinishedAt = &old
	if err := f.svc.store.put(rec); err != nil {
		t.Fatal(err)
	}
	again := f.submit(lostReq("crash-the-mac"))
	if job := f.lostOn(again, "b1", "n1", true); job.Status != StatusPending {
		t.Errorf("status=%s: a day-old quarantine should no longer count", job.Status)
	}
}

func TestReschedule_FailedStopKeepsTheJobAndRetries(t *testing.T) {
	f := newRescheduleFixture(t)
	id := f.submit(JobRequest{Kind: KindAgent, Pool: "linux", Repo: "https://github.com/x/y", Script: "claude"})
	f.allocs(id, nomad.Allocation{ID: "a1", NodeID: "n1", ClientStatus: "lost", Started: true, CreatedAt: time.Now()})
	f.nc.stopErr = errors.New("nomad down")
	f.svc.mu.Lock()
	delete(f.svc.cache, id)
	f.svc.mu.Unlock()
	if _, err := f.svc.Get(context.Background(), id); err == nil {
		t.Fatal("want the stop error")
	}
	if rec, _ := f.svc.store.get(id); isTerminal(rec.Status) || len(rec.Attempts) != 0 {
		t.Fatalf("a job whose Nomad copy may still run was ended: %s %+v", rec.Status, rec.Attempts)
	}
	f.nc.stopErr = nil
	if job := f.get(id); job.Status != StatusLost || len(job.Attempts) != 1 {
		t.Errorf("retry: status=%s attempts=%+v", job.Status, job.Attempts)
	}
}

func TestReschedule_FailedRedispatchKeepsTheJobAndRetries(t *testing.T) {
	f := newRescheduleFixture(t)
	id := f.submit(lostReq("make"))
	f.allocs(id, nomad.Allocation{ID: "a1", NodeID: "n1", ClientStatus: "lost", Started: true, CreatedAt: time.Now()})
	f.nc.dispatchErr = errors.New("nomad down")
	f.svc.mu.Lock()
	delete(f.svc.cache, id)
	f.svc.mu.Unlock()
	if _, err := f.svc.Get(context.Background(), id); err == nil {
		t.Fatal("want the dispatch error")
	}
	if rec, _ := f.svc.store.get(id); len(rec.Attempts) != 0 || isTerminal(rec.Status) {
		t.Fatalf("record changed on a failed requeue: %s %+v", rec.Status, rec.Attempts)
	}
	f.nc.dispatchErr = nil
	if job := f.get(id); job.Status != StatusPending || len(job.Attempts) != 1 {
		t.Errorf("retry: status=%s attempts=%+v", job.Status, job.Attempts)
	}
}

func TestDecideLost_Table(t *testing.T) {
	lost := func(w string, started bool) Attempt {
		return Attempt{Outcome: AttemptLost, Worker: w, JobStarted: started}
	}
	evicted := Attempt{Outcome: AttemptEvicted, Worker: "w1"}
	cases := []struct {
		name               string
		attempts           []Attempt
		retry, poisoned    bool
		rerun, quarantined bool
	}{
		{"first loss", []Attempt{lost("w1", true)}, true, false, true, false},
		{"second machine", []Attempt{lost("w1", true), lost("w2", true)}, true, false, false, true},
		{"same machine twice", []Attempt{lost("w1", true), lost("w1", true)}, true, false, true, false},
		{"budget", []Attempt{lost("w1", true), lost("w1", true), lost("w1", true)}, true, false, false, false},
		{"evictions are free", []Attempt{evicted, evicted, lost("w1", true), lost("w1", true)}, true, false, true, false},
		{"setup losses never quarantine", []Attempt{lost("w1", false), lost("w2", false)}, true, false, true, false},
		{"unknown machines never quarantine", []Attempt{lost("", true), lost("", true)}, true, false, true, false},
		{"no retry", []Attempt{lost("w1", true)}, false, false, false, false},
		{"quarantine beats no retry", []Attempt{lost("w1", true), lost("w2", true)}, false, false, false, true},
		{"poisoned and started", []Attempt{lost("w1", true)}, true, true, false, true},
		{"poisoned but setup", []Attempt{lost("w1", false)}, true, true, true, false},
	}
	for _, c := range cases {
		v := decideLost(c.attempts, c.retry, c.poisoned)
		if v.Rerun != c.rerun || v.Quarantine != c.quarantined || v.Reason == "" {
			t.Errorf("%s: %+v, want rerun=%v quarantine=%v", c.name, v, c.rerun, c.quarantined)
		}
	}
}

func TestReschedule_UnknownAllocationIsLostOnlyAfterTheGrace(t *testing.T) {
	f := newRescheduleFixture(t)
	id := f.submit(lostReq("make"))
	f.allocs(id, nomad.Allocation{ID: "a1", NodeID: "n1", ClientStatus: "unknown", Started: true, CreatedAt: time.Now()})
	job := f.get(id)
	if job.Status != StatusRunning || len(job.Attempts) != 0 || len(f.nc.dispatchCalls) != 1 {
		t.Fatalf("within the grace: status=%s attempts=%+v dispatches=%d", job.Status, job.Attempts, len(f.nc.dispatchCalls))
	}
	rec, _ := f.svc.store.get(id)
	since, ok := rec.UnknownSince["a1"]
	if !ok {
		t.Fatal("first sight of the unknown allocation not recorded")
	}
	// Seen again later: the first sighting is kept.
	f.get(id)
	if rec, _ = f.svc.store.get(id); !rec.UnknownSince["a1"].Equal(since) {
		t.Errorf("unknown since moved: %v -> %v", since, rec.UnknownSince["a1"])
	}
	// The node is back within the grace: the job carries on, the sighting is forgotten.
	f.allocs(id, nomad.Allocation{ID: "a1", NodeID: "n1", ClientStatus: "running", Started: true, CreatedAt: time.Now()})
	if job = f.get(id); job.Status != StatusRunning {
		t.Fatalf("status=%s", job.Status)
	}
	if rec, _ = f.svc.store.get(id); len(rec.UnknownSince) != 0 {
		t.Errorf("unknownSince = %v after the node came back", rec.UnknownSince)
	}

	// Gone again, past the grace: lost, and run again.
	f.allocs(id, nomad.Allocation{ID: "a1", NodeID: "n1", ClientStatus: "unknown", Started: true, CreatedAt: time.Now()})
	f.get(id)
	rec, _ = f.svc.store.get(id)
	rec.UnknownSince["a1"] = time.Now().Add(-DefaultUnknownGrace - time.Second)
	if err := f.svc.store.put(rec); err != nil {
		t.Fatal(err)
	}
	f.nc.dispatchResult = &nomad.DispatchResult{DispatchedJobID: "next"}
	job = f.get(id)
	if job.Status != StatusPending || len(job.Attempts) != 1 || !strings.Contains(job.Attempts[0].Reason, "lost contact") || len(f.nc.dispatchCalls) != 2 {
		t.Fatalf("past the grace: status=%s attempts=%+v dispatches=%d", job.Status, job.Attempts, len(f.nc.dispatchCalls))
	}
	// The old copy is purged: when the node returns, Nomad stops the job's task there.
	if last := f.nc.stopCalls[len(f.nc.stopCalls)-1]; !last.purge {
		t.Errorf("old copy not purged: %+v", last)
	}
}

// TestDecideLost_MatchesModel checks decideLost against spec/quint/reschedule.qnt's verdict on
// every sequence of up to MaxAttempts losses over three machines, started or not, for every
// retryLost and poisoned.
func TestDecideLost_MatchesModel(t *testing.T) {
	model := func(ls []Attempt, retry, poisoned bool) string {
		machines := map[string]bool{}
		for _, l := range ls {
			if l.JobStarted {
				machines[l.Worker] = true
			}
		}
		switch {
		case len(machines) >= QuarantineMachines:
			return "quarantine"
		case poisoned && ls[len(ls)-1].JobStarted:
			return "quarantine"
		case !retry:
			return "lost"
		case len(ls) >= MaxAttempts:
			return "lost"
		}
		return "rerun"
	}
	var walk func(ls []Attempt)
	n := 0
	walk = func(ls []Attempt) {
		if len(ls) > 0 {
			for _, retry := range []bool{true, false} {
				for _, poisoned := range []bool{true, false} {
					v := decideLost(ls, retry, poisoned)
					got := "rerun"
					if v.Quarantine {
						got = "quarantine"
					} else if !v.Rerun {
						got = "lost"
					}
					if want := model(ls, retry, poisoned); got != want {
						t.Errorf("%+v retry=%v poisoned=%v: %s, model says %s", ls, retry, poisoned, got, want)
					}
					n++
				}
			}
		}
		if len(ls) == MaxAttempts {
			return
		}
		for _, m := range []string{"m1", "m2", "m3"} {
			for _, started := range []bool{true, false} {
				walk(append(append([]Attempt(nil), ls...), Attempt{Outcome: AttemptLost, Worker: m, JobStarted: started}))
			}
		}
	}
	walk(nil)
	if n != 4*(6+36+216) {
		t.Errorf("checked %d cases", n)
	}
}
