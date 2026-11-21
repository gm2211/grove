package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// A pool names no Mac. Which Macs run it follows from which are up and which can get its image:
// a Mac that fails to get it while another runs it (or while it is a local Tart VM) is left out
// and tried again later, so a pool with a local image stays on the Macs that have it and spreads
// once the image is pullable, with no pin in fleet.yaml.

const localLinux = "grove-studio-linux-v2" // a local Tart VM: only the Mac that built it has it

func dynamicPool(image string) Pool {
	p := basePool()
	p.Image = image
	return p
}

func ourVM(pool, worker string, n int) orchard.VM {
	return orchard.VM{Name: VMName(pool, worker, n), Worker: worker, Labels: map[string]string{LabelWorkerPin: worker}}
}

func vmState(pool, worker, image, status, msg string) orchard.VM {
	vm := ourVM(pool, worker, 0)
	vm.Image, vm.Status, vm.StatusMessage = image, status, msg
	return vm
}

type placementFixture struct {
	t       *testing.T
	pool    Pool
	workers []orchard.Worker
	images  *ImageHolds
	now     time.Time
}

func newPlacement(t *testing.T, image string, workers ...orchard.Worker) *placementFixture {
	images, _ := OpenImageHolds("")
	return &placementFixture{t: t, pool: dynamicPool(image), workers: workers, images: images, now: time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)}
}

func (f *placementFixture) observe(vms ...orchard.VM) []string {
	return f.images.Observe(&Spec{Pools: []Pool{f.pool}}, vms, f.now)
}

func (f *placementFixture) plan(vms []orchard.VM, load *JobLoad) Plan {
	f.t.Helper()
	r := &Reconciler{
		Client:  &fakeOrchardClient{workers: f.workers, vms: vms},
		Spec:    &Spec{Pools: []Pool{f.pool}},
		Options: Options{Load: load, Images: f.images, Now: func() time.Time { return f.now }},
	}
	plan, err := r.Plan(context.Background())
	if err != nil {
		f.t.Fatal(err)
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

// touches reports whether plan creates or deletes anything on worker. (The fixtures' VMs carry no
// full spec, so a VM on another worker may be rebuilt for drift; that is not what these test.)
func touches(plan Plan, worker string) (creates, deletes []string) {
	for _, c := range plan.Creates {
		if c.Worker == worker {
			creates = append(creates, c.Spec.Name)
		}
	}
	for _, d := range plan.Deletes {
		if d.Worker == worker {
			deletes = append(deletes, d.Name+" ("+d.Reason+")")
		}
	}
	return creates, deletes
}

const noSuchVM = `failed to clone the VM: tart command failed: the specified VM "grove-studio-linux-v2" does not exist`

func TestPlacement_UnpinnedPoolRunsOnEveryMacUntilOneCantGetTheImage(t *testing.T) {
	f := newPlacement(t, localLinux, onlineWorker("studio"), onlineWorker("mbp"))
	if got := createdOn(f.plan(nil, nil)); len(got) != 2 {
		t.Fatalf("first plan creates on %v, want both Macs", got)
	}

	vms := []orchard.VM{vmState("linux", "studio", localLinux, "running", ""), vmState("linux", "mbp", localLinux, "failed", noSuchVM)}
	notes := f.observe(vms...)
	if len(notes) != 1 || !strings.Contains(notes[0], "worker mbp can't get image") {
		t.Fatalf("notes = %q", notes)
	}
	plan := f.plan(vms, nil)
	if c, d := touches(plan, "mbp"); len(c) != 0 || len(d) != 1 || !strings.Contains(d[0], "can't get the pool's image") {
		t.Errorf("on mbp: creates %v, deletes %v, want only its failed VM deleted", c, d)
	}
	if len(plan.ImageMissing) != 1 || plan.ImageMissing[0].Worker != "mbp" || len(plan.Unmatched) != 0 {
		t.Errorf("imageMissing=%+v unmatched=%v", plan.ImageMissing, plan.Unmatched)
	}

	// Converged: the failed VM is gone, the pool runs on the studio only.
	vms = vms[:1]
	if c, d := touches(f.plan(vms, nil), "mbp"); len(c)+len(d) != 0 {
		t.Errorf("after the delete, on mbp: creates %v, deletes %v", c, d)
	}
}

func TestPlacement_TriesTheMacAgainLaterAndKeepsItOnceTheImageArrives(t *testing.T) {
	f := newPlacement(t, localLinux, onlineWorker("studio"), onlineWorker("mbp"))
	running := vmState("linux", "studio", localLinux, "running", "")
	f.observe(running, vmState("linux", "mbp", localLinux, "failed", noSuchVM))

	f.now = f.now.Add(ImageHoldRetry)
	if c, _ := touches(f.plan([]orchard.VM{running}, nil), "mbp"); len(c) != 1 {
		t.Fatalf("retry due: creates on mbp %v, want one", c)
	}
	// Still missing: off again, for twice as long.
	f.observe(running, vmState("linux", "mbp", localLinux, "failed", noSuchVM))
	if m := f.plan([]orchard.VM{running}, nil).ImageMissing; len(m) != 1 || !m[0].RetryAt.Equal(f.now.Add(2*ImageHoldRetry)) {
		t.Fatalf("after a second failure: %+v", m)
	}

	// The image is there now (pushed to a registry, or built on mbp): mbp is back for good.
	f.now = f.now.Add(2 * ImageHoldRetry)
	notes := f.observe(running, vmState("linux", "mbp", localLinux, "running", ""))
	if len(notes) != 1 || !strings.Contains(notes[0], "back on it") {
		t.Fatalf("notes = %q", notes)
	}
	f.now = f.now.Add(time.Minute)
	if m := f.plan(nil, nil).ImageMissing; len(m) != 0 {
		t.Errorf("still missing %+v", m)
	}
}

func TestPlacement_NoMacWithTheImageLeavesThePoolUnmatchedSoItsJobsFallBack(t *testing.T) {
	studio, mbp := onlineWorker("studio"), onlineWorker("mbp")
	f := newPlacement(t, localLinux, studio, mbp)
	f.observe(vmState("linux", "studio", localLinux, "running", ""), vmState("linux", "mbp", localLinux, "failed", noSuchVM))

	studio.Offline = true // the only Mac with the image sleeps
	f.workers = []orchard.Worker{studio, mbp}
	plan := f.plan(nil, nil)
	if len(plan.Creates) != 0 || len(plan.Unmatched) != 1 || plan.Unmatched[0] != "linux" {
		t.Errorf("creates=%v unmatched=%v, want the pool unmatched (its jobs fall back to macos)", createdOn(plan), plan.Unmatched)
	}
}

func TestPlacement_AVMRunningAJobStaysUntilTheJobIsDone(t *testing.T) {
	f := newPlacement(t, localLinux, onlineWorker("studio"), onlineWorker("mbp"))
	f.observe(vmState("linux", "studio", localLinux, "running", ""), vmState("linux", "mbp", localLinux, "failed", noSuchVM))
	// An older VM of the pool on mbp (another image it still had) is running a job.
	busy := vmState("linux", "mbp", "ghcr.io/example/old:1", "running", "")
	plan := f.plan([]orchard.VM{vmState("linux", "studio", localLinux, "running", ""), busy}, &JobLoad{Busy: map[string]bool{busy.Name: true}})
	for _, d := range plan.Deletes {
		if d.Name == busy.Name {
			t.Fatalf("deleted %s under its job", d.Name)
		}
	}
}

func TestPlacement_ARegistryImageFailingEverywhereIsAPoolProblemNotAMacOne(t *testing.T) {
	const img = "ghcr.io/example/linux:new"
	f := newPlacement(t, img, onlineWorker("studio"), onlineWorker("mbp"))
	f.observe(vmState("linux", "studio", img, "failed", "failed to pull: manifest unknown"), vmState("linux", "mbp", img, "failed", "failed to pull: manifest unknown"))
	if m := f.plan(nil, nil).ImageMissing; len(m) != 0 {
		t.Errorf("per-Mac holds %+v for an image no Mac can pull", m)
	}

	// Pulled on one Mac and not on the other (say its registry login is missing): that Mac only.
	f.observe(vmState("linux", "studio", img, "running", ""), vmState("linux", "mbp", img, "failed", "failed to pull: unauthorized"))
	if m := f.plan(nil, nil).ImageMissing; len(m) != 1 || m[0].Worker != "mbp" {
		t.Errorf("imageMissing = %+v, want mbp", m)
	}
}

func TestPlacement_ANewImageClearsTheHolds(t *testing.T) {
	f := newPlacement(t, localLinux, onlineWorker("studio"), onlineWorker("mbp"))
	f.observe(vmState("linux", "studio", localLinux, "running", ""), vmState("linux", "mbp", localLinux, "failed", noSuchVM))
	f.pool.Image = "ghcr.io/gm2211/grove-linux-worker:latest"
	f.observe()
	if got := createdOn(f.plan(nil, nil)); len(got) != 2 {
		t.Errorf("new image: creates on %v, want both Macs", got)
	}
}

func TestPlacement_HoldsSurviveARestart(t *testing.T) {
	path := t.TempDir() + "/images.json"
	images, _ := OpenImageHolds(path)
	f := newPlacement(t, localLinux, onlineWorker("studio"), onlineWorker("mbp"))
	f.images = images
	f.observe(vmState("linux", "studio", localLinux, "running", ""), vmState("linux", "mbp", localLinux, "failed", noSuchVM))
	reopened, err := OpenImageHolds(path)
	if err != nil {
		t.Fatal(err)
	}
	f.images = reopened
	if m := f.plan(nil, nil).ImageMissing; len(m) != 1 || m[0].Worker != "mbp" {
		t.Errorf("after reopening: %+v", m)
	}
}

func TestWarnings_PinsAndPreferencesAreNotNeeded(t *testing.T) {
	s := &Spec{Pools: []Pool{
		{Name: "macos", WorkerSelector: map[string]string{"host": "studio"}},
		{Name: "linux", PreferWorkers: map[string]string{"host": "studio"}},
	}}
	var pin, pref string
	for _, x := range s.Warnings() {
		switch {
		case strings.Contains(x, "workerSelector"):
			pin = x
		case strings.Contains(x, "preferWorkers"):
			pref = x
		}
	}
	if !strings.Contains(pin, `pool "macos"`) || !strings.Contains(pin, "drop it") {
		t.Errorf("host pin warning = %q", pin)
	}
	if !strings.Contains(pref, `pool "linux"`) || !strings.Contains(pref, "ignored") {
		t.Errorf("preferWorkers warning = %q", pref)
	}
}
