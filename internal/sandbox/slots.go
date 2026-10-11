package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// Slots caps how many jobs run at once in each sealed VM. Nomad can't do it, since the VMs run no
// Nomad client: every `grove sandbox run` on the control plane instead holds an exclusive lock on
// one of a VM's slot files for as long as its job runs. The kernel drops a lock when its process
// exits, however it exits, so a crashed runner never strands a slot.
type Slots struct {
	// Dir holds the lock files, one per VM and slot (<vm>.<n>.lock).
	Dir string
	// PerVM is how many jobs one VM runs at once; values below 1 mean 1.
	PerVM int
}

// Slot is a held slot in one VM.
type Slot struct {
	VM   string
	file *os.File
}

// Release frees the slot.
func (s *Slot) Release() {
	if s != nil && s.file != nil {
		unlockFile(s.file)
		s.file.Close()
		s.file = nil
	}
}

// TryAcquire takes a free slot in the VM among vms that has the most of them, spreading jobs
// across a pool's VMs. It returns nil when every slot is taken.
func (s Slots) TryAcquire(vms []string) (*Slot, error) {
	perVM := max(s.PerVM, 1)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("slot directory: %w", err)
	}

	held := make(map[string][]*os.File, len(vms))
	releaseAll := func() {
		for _, files := range held {
			for _, f := range files {
				unlockFile(f)
				f.Close()
			}
		}
	}

	best := ""
	for _, vm := range slices.Sorted(slices.Values(vms)) {
		for n := range perVM {
			f, err := os.OpenFile(filepath.Join(s.Dir, fmt.Sprintf("%s.%d.lock", vm, n)), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				releaseAll()
				return nil, fmt.Errorf("slot file: %w", err)
			}
			ok, err := tryLockFile(f)
			if err != nil || !ok {
				f.Close()
				if err != nil {
					releaseAll()
					return nil, fmt.Errorf("lock slot file: %w", err)
				}
				continue
			}
			held[vm] = append(held[vm], f)
		}
		if len(held[vm]) > len(held[best]) {
			best = vm
		}
	}
	if best == "" {
		releaseAll()
		return nil, nil
	}

	keep := held[best][0]
	held[best] = held[best][1:]
	releaseAll()
	return &Slot{VM: best, file: keep}, nil
}
