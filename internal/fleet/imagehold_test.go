package fleet

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

const pullDenied = "failed to clone the VM: tart command failed: AuthFailed 'received unexpected HTTP status code 403 while retrieving an authentication token' DENIED 'requested access to the resource is denied'"

// imageWorld is an Orchard whose workers can pull every image except the ones in unpullable.
type imageWorld struct {
	t          *testing.T
	client     *fakeOrchardClient
	holds      *ImageHolds
	spec       *Spec
	now        time.Time
	unpullable map[string]bool
	notes      []string
}

func newImageWorld(t *testing.T, pool Pool) *imageWorld {
	holds, err := OpenImageHolds(filepath.Join(t.TempDir(), "images.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &imageWorld{
		t:          t,
		client:     &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}},
		holds:      holds,
		spec:       &Spec{Pools: []Pool{pool}},
		now:        time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
		unpullable: map[string]bool{},
	}
}

// tick is one reconcile pass: observe, plan, apply, and Orchard starting (or failing) what was
// created. It returns the image the pool's VM has afterwards and its status.
func (w *imageWorld) tick() (image, status string) {
	w.t.Helper()
	w.notes = append(w.notes, w.holds.Observe(w.spec, w.client.vms, w.now)...)
	r := &Reconciler{Client: w.client, Spec: w.spec, Options: Options{Images: w.holds, Now: func() time.Time { return w.now }}}
	plan, err := r.Plan(context.Background())
	if err != nil {
		w.t.Fatalf("Plan: %v", err)
	}
	for _, d := range plan.Deletes {
		kept := w.client.vms[:0]
		for _, vm := range w.client.vms {
			if vm.Name != d.Name {
				kept = append(kept, vm)
			}
		}
		w.client.vms = kept
	}
	for _, c := range plan.Creates {
		vm := orchard.VM{
			Name: c.Spec.Name, Worker: c.Worker, Image: c.Spec.Image, Status: "running",
			Labels:    map[string]string{LabelWorkerPin: c.Worker},
			CreatedAt: w.now, DiskSize: c.Spec.DiskSize, RestartPolicy: c.Spec.RestartPolicy,
			CPU: c.Spec.CPU, Memory: c.Spec.Memory, Softnet: c.Spec.Softnet, SoftnetBlock: c.Spec.SoftnetBlock,
			StartupScript: c.Spec.StartupScript, ShutdownScript: c.Spec.ShutdownScript,
			ShutdownTimeout: c.Spec.ShutdownTimeout, TTL: c.Spec.TTL, HostDirs: c.Spec.HostDirs,
		}
		if w.unpullable[vm.Image] {
			vm.Status, vm.StatusMessage = "failed", pullDenied
		}
		w.client.vms = append(w.client.vms, vm)
	}
	if len(w.client.vms) != 1 {
		w.t.Fatalf("want exactly one VM, have %+v", w.client.vms)
	}
	return w.client.vms[0].Image, w.client.vms[0].Status
}

func (w *imageWorld) setImage(image string) { w.spec.Pools[0].Image = image }

func TestImageHolds_KeepsLastGoodImageAndRecoversByItself(t *testing.T) {
	pool := basePool()
	good, bad := pool.Image, "ghcr.io/example/worker@sha256:deadbeef"
	w := newImageWorld(t, pool)

	if image, status := w.tick(); image != good || status != "running" {
		t.Fatalf("first VM: %s %s", image, status)
	}

	// fleet.yaml now names an image nobody can pull.
	w.setImage(bad)
	w.unpullable[bad] = true
	if image, status := w.tick(); image != bad || status != "failed" {
		t.Fatalf("the named image is tried first: %s %s", image, status)
	}
	// The very next pass puts the pool back on the image that ran, with no wait.
	if image, status := w.tick(); image != good || status != "running" {
		t.Fatalf("want the pool back on %s, got %s %s", good, image, status)
	}
	if len(w.notes) != 1 || !strings.Contains(w.notes[0], "can't be pulled") {
		t.Fatalf("notes: %q", w.notes)
	}

	// It stays there, and says so, until the retry is due.
	w.now = w.now.Add(ImageHoldRetry - time.Minute)
	if image, _ := w.tick(); image != good {
		t.Fatalf("retried early: %s", image)
	}
	r := &Reconciler{Client: w.client, Spec: w.spec, Options: Options{Images: w.holds, Now: func() time.Time { return w.now }}}
	plan, _ := r.Plan(context.Background())
	if !plan.Empty() || len(plan.Fallbacks) != 1 || plan.Fallbacks[0].Running != good || plan.Fallbacks[0].Image != bad {
		t.Fatalf("plan while held: %+v", plan)
	}

	// A due retry that fails again costs one recycle and doubles the wait.
	w.now = w.now.Add(2 * time.Minute)
	if image, status := w.tick(); image != bad || status != "failed" {
		t.Fatalf("retry: %s %s", image, status)
	}
	if image, _ := w.tick(); image != good {
		t.Fatalf("after the failed retry: %s", image)
	}
	w.now = w.now.Add(ImageHoldRetry + time.Minute)
	if image, _ := w.tick(); image != good {
		t.Fatalf("second wait must be longer than the first: %s", image)
	}

	// The image is pushed (or the login fixed): the next due retry moves the pool over for good.
	delete(w.unpullable, bad)
	w.now = w.now.Add(ImageHoldRetry)
	if image, status := w.tick(); image != bad || status != "running" {
		t.Fatalf("recovery: %s %s", image, status)
	}
	w.tick()
	if _, fb := w.holds.effective(w.spec.Pools[0], w.now); fb != nil {
		t.Fatalf("hold not cleared: %+v", fb)
	}
	if last := w.notes[len(w.notes)-1]; !strings.Contains(last, "runs again") {
		t.Fatalf("notes: %q", w.notes)
	}
}

func TestImageHolds_NewImageInFleetYAMLIsTriedAtOnce(t *testing.T) {
	pool := basePool()
	good, bad, fixed := pool.Image, "ghcr.io/example/worker@sha256:deadbeef", "ghcr.io/example/worker@sha256:f00d"
	w := newImageWorld(t, pool)
	w.tick()
	w.setImage(bad)
	w.unpullable[bad] = true
	w.tick()
	if image, _ := w.tick(); image != good {
		t.Fatalf("not held: %s", image)
	}
	w.setImage(fixed)
	if image, status := w.tick(); image != fixed || status != "running" {
		t.Fatalf("a changed image must not wait for the retry: %s %s", image, status)
	}
}

func TestImageHolds_SurviveARestart(t *testing.T) {
	pool := basePool()
	good, bad := pool.Image, "ghcr.io/example/worker@sha256:deadbeef"
	w := newImageWorld(t, pool)
	w.tick()
	w.setImage(bad)
	w.unpullable[bad] = true
	w.tick()
	w.tick()

	reopened, err := OpenImageHolds(w.holds.path)
	if err != nil {
		t.Fatal(err)
	}
	if image, fb := reopened.effective(w.spec.Pools[0], w.now); image != good || fb == nil {
		t.Fatalf("after reopening: %s %+v", image, fb)
	}
}

func TestImageHolds_NothingToFallBackTo(t *testing.T) {
	pool := basePool()
	w := newImageWorld(t, pool)
	w.unpullable[pool.Image] = true
	if image, status := w.tick(); image != pool.Image || status != "failed" {
		t.Fatalf("%s %s", image, status)
	}
	// No image ever ran: the failed VM stays for FailedVMRetry as before, and is reported as an
	// image problem.
	if image, _ := w.tick(); image != pool.Image {
		t.Fatalf("%s", image)
	}
	problems := Problems(w.client.vms, nil, w.now)
	if len(problems) != 1 || problems[0].Kind != ProblemImageUnavailable {
		t.Fatalf("%+v", problems)
	}
}

func TestImageUnavailable(t *testing.T) {
	for msg, want := range map[string]bool{
		pullDenied:                        true,
		"failed to pull the VM: no space": true,
		"VM exited unexpectedly":          false,
		"Failed to get current host key":  false,
		"":                                false,
	} {
		if got := ImageUnavailable(msg); got != want {
			t.Errorf("ImageUnavailable(%q) = %t, want %t", msg, got, want)
		}
	}
}
