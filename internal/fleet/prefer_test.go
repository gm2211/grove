package fleet

import (
	"context"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/orchard"
)

// labelled is an online worker carrying labels.
func labelled(name string, kv ...string) orchard.Worker {
	w := onlineWorker(name)
	for i := 0; i+1 < len(kv); i += 2 {
		w.Labels[kv[i]] = kv[i+1]
	}
	return w
}

func preferPool() Pool {
	p := basePool()
	p.WorkerSelector = map[string]string{"arch": "arm64"}
	p.PreferWorkers = map[string]string{"host": "studio"}
	return p
}

func ourVM(pool, worker string, n int) orchard.VM {
	return orchard.VM{Name: VMName(pool, worker, n), Worker: worker, Labels: map[string]string{LabelWorkerPin: worker}}
}

func planWith(t *testing.T, pool Pool, workers []orchard.Worker, vms []orchard.VM, load *JobLoad) Plan {
	t.Helper()
	r := &Reconciler{Client: &fakeOrchardClient{workers: workers, vms: vms}, Spec: &Spec{Pools: []Pool{pool}}, Options: Options{Load: load}}
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func createdOn(plan Plan) []string {
	var out []string
	for _, c := range plan.Creates {
		out = append(out, c.Worker)
	}
	return out
}

func TestPrefer_RunsOnlyOnThePreferredWorkerWhileItIsUp(t *testing.T) {
	workers := []orchard.Worker{labelled("studio", "host", "studio", "arch", "arm64"), labelled("mbp", "host", "mbp", "arch", "arm64")}
	plan := planWith(t, preferPool(), workers, nil, nil)
	if got := createdOn(plan); len(got) != 1 || got[0] != "studio" {
		t.Fatalf("creates on %v, want studio only", got)
	}
	if len(plan.Spilled) != 0 || len(plan.Unmatched) != 0 {
		t.Errorf("spilled=%v unmatched=%v", plan.Spilled, plan.Unmatched)
	}
}

func TestPrefer_SpillsOntoCapableWorkersWhenThePreferredOneIsAway(t *testing.T) {
	studio := labelled("studio", "host", "studio", "arch", "arm64")
	studio.Offline = true
	intel := labelled("old", "host", "old", "arch", "amd64") // the hard selector still rules it out
	workers := []orchard.Worker{studio, labelled("mbp", "host", "mbp", "arch", "arm64"), intel}
	plan := planWith(t, preferPool(), workers, []orchard.VM{ourVM("linux", "studio", 0)}, nil)
	if got := createdOn(plan); len(got) != 1 || got[0] != "mbp" {
		t.Fatalf("creates on %v, want mbp only", got)
	}
	if len(plan.Spilled) != 1 || plan.Spilled[0] != "linux" || len(plan.Unmatched) != 0 {
		t.Errorf("spilled=%v unmatched=%v", plan.Spilled, plan.Unmatched)
	}
	// Paused preferred workers spill too.
	studio.Offline, studio.SchedulingPaused = false, true
	workers[0] = studio
	if got := createdOn(planWith(t, preferPool(), workers, nil, nil)); len(got) != 1 || got[0] != "mbp" {
		t.Errorf("paused studio: creates on %v", got)
	}
}

func TestPrefer_MovesBackOnceIdleWhenThePreferredWorkerReturns(t *testing.T) {
	workers := []orchard.Worker{labelled("studio", "host", "studio", "arch", "arm64"), labelled("mbp", "host", "mbp", "arch", "arm64")}
	spilled := ourVM("linux", "mbp", 0)

	// Busy: the spilled VM stays until its job is done.
	plan := planWith(t, preferPool(), workers, []orchard.VM{spilled}, &JobLoad{Busy: map[string]bool{spilled.Name: true}})
	if len(plan.Deletes) != 0 {
		t.Fatalf("deleted %+v under a running job", plan.Deletes)
	}
	if got := createdOn(plan); len(got) != 1 || got[0] != "studio" {
		t.Errorf("creates on %v, want studio", got)
	}

	// Idle: it goes.
	plan = planWith(t, preferPool(), workers, []orchard.VM{spilled}, &JobLoad{Busy: map[string]bool{spilled.Name: false}})
	if len(plan.Deletes) != 1 || plan.Deletes[0].Name != spilled.Name || !strings.Contains(plan.Deletes[0].Reason, "preferred worker") {
		t.Fatalf("deletes = %+v", plan.Deletes)
	}
}

func TestPrefer_NoCapableWorkerAtAllIsUnmatched(t *testing.T) {
	studio := labelled("studio", "host", "studio", "arch", "arm64")
	studio.Offline = true
	plan := planWith(t, preferPool(), []orchard.Worker{studio}, nil, nil)
	if len(plan.Creates) != 0 || len(plan.Unmatched) != 1 || len(plan.Spilled) != 0 {
		t.Errorf("creates=%v unmatched=%v spilled=%v", plan.Creates, plan.Unmatched, plan.Spilled)
	}
}

func TestPrefer_ElasticPoolScalesUpOnThePreferredWorkerOrSpills(t *testing.T) {
	pool := preferPool()
	zero, one := 0, 1
	pool.MinPerWorker, pool.MaxPerWorker = &zero, &one
	big := func(w orchard.Worker) orchard.Worker {
		w.Resources = map[string]uint64{"org.cirruslabs.tart-vms": 2, "logical-cores": 16, "memory-mib": 65536}
		return w
	}
	studio, mbp := big(labelled("studio", "host", "studio", "arch", "arm64")), big(labelled("mbp", "host", "mbp", "arch", "arm64"))
	load := &JobLoad{Waiting: map[string]int{"linux": 1}}
	if got := createdOn(planWith(t, pool, []orchard.Worker{studio, mbp}, nil, load)); len(got) != 1 || got[0] != "studio" {
		t.Errorf("scale up on %v, want studio", got)
	}
	studio.Offline = true
	if got := createdOn(planWith(t, pool, []orchard.Worker{studio, mbp}, nil, load)); len(got) != 1 || got[0] != "mbp" {
		t.Errorf("scale up with studio away on %v, want mbp", got)
	}
}

func TestPrefer_WithoutPreferWorkersNothingChanges(t *testing.T) {
	pool := basePool()
	pool.WorkerSelector = map[string]string{"arch": "arm64"}
	workers := []orchard.Worker{labelled("studio", "arch", "arm64"), labelled("mbp", "arch", "arm64")}
	if got := createdOn(planWith(t, pool, workers, nil, nil)); len(got) != 2 {
		t.Errorf("creates on %v, want both", got)
	}
}

func TestWarnings_HostPinnedSelector(t *testing.T) {
	s := &Spec{Pools: []Pool{{Name: "macos", WorkerSelector: map[string]string{"host": "studio"}}, preferPool()}}
	w := s.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], `pool "macos"`) || !strings.Contains(w[0], "preferWorkers") {
		t.Errorf("warnings = %q", w)
	}
}
