package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// An image a pool names can turn out not to be pullable: a tag or digest that was never pushed, a
// package that was deleted, a registry login that lapsed. Without ImageHolds the pool then loses
// every VM as each one is recreated against it, and stays down until someone edits fleet.yaml.
//
// With it, the Reconciler keeps the pool on the last image a VM of it actually ran (the worker
// still has that one in Tart's cache) while the named image can't be pulled, tries the named image
// again on a growing interval, and moves the pool over by itself once a VM runs it. A changed
// `image:` in fleet.yaml is tried at once. Nothing is held for a pool that never ran anything:
// there is no image to fall back to, so its failed VM is rebuilt as before (FailedVMRetry).

// ImageHoldRetry is how long a pool stays on its last good image before its named image is tried
// again, doubling with every failed try up to ImageHoldRetryMax. A try recycles one VM of the
// pool, so it is not something to do every few minutes.
const (
	ImageHoldRetry    = 30 * time.Minute
	ImageHoldRetryMax = 6 * time.Hour
)

// ImageUnavailable reports whether an Orchard failure message is the worker failing to get the
// VM's image ("failed to clone the VM: tart command failed: AuthFailed … DENIED …", "failed to
// pull …"), as opposed to a VM that was cloned and then failed to boot.
func ImageUnavailable(statusMessage string) bool {
	m := strings.ToLower(statusMessage)
	return strings.Contains(m, "failed to clone") || strings.Contains(m, "failed to pull") ||
		strings.Contains(m, "manifest unknown")
}

// HeldImage is a pool's named image that could not be pulled.
type HeldImage struct {
	// Image is the pool's `image:` when the pull failed. A hold for another image than the pool
	// names now is stale and ignored.
	Image string `json:"image"`
	// Reason is Orchard's failure message.
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
	// RetryAt is when the named image is tried again; Tries counts the failed tries so far.
	RetryAt time.Time `json:"retryAt"`
	Tries   int       `json:"tries"`
}

type poolImageState struct {
	// Good is the image of the last VM of the pool seen running.
	Good string     `json:"good,omitempty"`
	Held *HeldImage `json:"held,omitempty"`
}

const imageHoldsVersion = 1

type imageHoldsFile struct {
	Version int                    `json:"version"`
	Pools   map[string]*poolImageState `json:"pools"`
}

// ImageHolds remembers, per pool, the last image that ran and a named image that can't be pulled.
// It holds image names and Orchard's messages only, never a credential. Safe for concurrent use;
// a nil *ImageHolds remembers nothing and holds nothing.
type ImageHolds struct {
	path string

	mu    sync.Mutex
	pools map[string]*poolImageState
}

// OpenImageHolds loads the holds kept at path (`images.json` next to `devices.json`). A missing
// file is an empty store; an unreadable one is an error. An empty path keeps them in memory only.
func OpenImageHolds(path string) (*ImageHolds, error) {
	h := &ImageHolds{path: path, pools: map[string]*poolImageState{}}
	if path == "" {
		return h, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read image holds: %w", err)
	}
	var f imageHoldsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse image holds %s: %w", path, err)
	}
	for name, p := range f.Pools {
		if p != nil {
			h.pools[name] = p
		}
	}
	return h, nil
}

// PlannedFallback is a pool the Reconciler runs on its last good image because the image it
// names can't be pulled.
type PlannedFallback struct {
	Pool string `json:"pool"`
	// Image is the image fleet.yaml names; Running is the one its VMs are created with instead.
	Image   string    `json:"image"`
	Running string    `json:"running"`
	Reason  string    `json:"reason"`
	Since   time.Time `json:"since"`
	RetryAt time.Time `json:"retryAt"`
}

// effective returns the image pool's VMs should be created with at now: the pool's own, unless it
// is held, a different good image is known and the next try isn't due.
func (h *ImageHolds) effective(pool Pool, now time.Time) (string, *PlannedFallback) {
	if h == nil {
		return pool.Image, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.pools[pool.Name]
	if p == nil || p.Held == nil || p.Held.Image != pool.Image || p.Good == "" || p.Good == pool.Image {
		return pool.Image, nil
	}
	if !now.Before(p.Held.RetryAt) {
		return pool.Image, nil
	}
	return p.Good, &PlannedFallback{
		Pool: pool.Name, Image: pool.Image, Running: p.Good,
		Reason: p.Held.Reason, Since: p.Held.Since, RetryAt: p.Held.RetryAt,
	}
}

// Observe updates the holds from Orchard's VMs: a running VM's image is its pool's good image, a
// running VM of the pool's named image ends the hold, and a VM of the named image that failed for
// want of the image starts one (or, once its retry was due, counts another failed try). It
// returns one line per change, for the log.
func (h *ImageHolds) Observe(spec *Spec, vms []orchard.VM, now time.Time) []string {
	if h == nil || spec == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	var notes []string
	changed := false
	for _, pool := range spec.Pools {
		p := h.pools[pool.Name]
		if p == nil {
			p = &poolImageState{}
		}
		before := *p
		var beforeHeld HeldImage
		if p.Held != nil {
			beforeHeld = *p.Held
		}

		if p.Held != nil && p.Held.Image != pool.Image {
			p.Held = nil // fleet.yaml names another image now
		}

		namedRuns, other, failure := false, "", ""
		for _, vm := range vms {
			vmPool, worker, _, ok := ParseVMName(vm.Name)
			if !ok || vmPool != pool.Name || vm.Labels[LabelWorkerPin] != worker {
				continue
			}
			switch {
			case vm.Status == "running" && vm.Image == pool.Image:
				namedRuns = true
			case vm.Status == "running" && vm.Image != "":
				other = vm.Image
			case vm.Status == "failed" && vm.Image == pool.Image && ImageUnavailable(vm.StatusMessage):
				failure = vm.StatusMessage
			}
		}
		if !namedRuns && other != "" {
			p.Good = other
		}

		switch {
		case namedRuns:
			p.Good = pool.Image
			if p.Held != nil {
				notes = append(notes, fmt.Sprintf("pool %s: image %s runs again; back on it", pool.Name, pool.Image))
			}
			p.Held = nil
		case failure != "" && p.Good != "" && p.Good != pool.Image:
			if p.Held == nil {
				p.Held = &HeldImage{Image: pool.Image, Since: now}
			}
			if p.Held.Tries == 0 || !now.Before(p.Held.RetryAt) {
				p.Held.Tries++
				p.Held.Reason = failure
				p.Held.RetryAt = now.Add(imageHoldBackoff(p.Held.Tries))
				notes = append(notes, fmt.Sprintf(
					"pool %s: image %s can't be pulled (%s); keeping the pool on %s, next try at %s",
					pool.Name, pool.Image, failure, p.Good, p.Held.RetryAt.UTC().Format(time.RFC3339)))
			}
		}

		if p.Good == "" && p.Held == nil {
			continue
		}
		h.pools[pool.Name] = p
		if before.Good != p.Good || (before.Held == nil) != (p.Held == nil) || (p.Held != nil && *p.Held != beforeHeld) {
			changed = true
		}
	}

	if changed {
		if err := h.saveLocked(); err != nil {
			notes = append(notes, "image holds not saved: "+err.Error())
		}
	}
	return notes
}

func imageHoldBackoff(tries int) time.Duration {
	d := ImageHoldRetry
	for i := 1; i < tries && d < ImageHoldRetryMax; i++ {
		d *= 2
	}
	if d > ImageHoldRetryMax {
		d = ImageHoldRetryMax
	}
	return d
}

func (h *ImageHolds) saveLocked() error {
	if h.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(imageHoldsFile{Version: imageHoldsVersion, Pools: h.pools}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(h.path), ".images-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), h.path)
}
