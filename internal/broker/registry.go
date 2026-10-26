package broker

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Lease timing. The control plane gives every entry a Lease and renews it every RenewEvery while
// the job runs; a broker that loses touch with the control plane drops the entry when the lease
// runs out, so a job outliving its control plane can't keep using a token for long.
const (
	Lease      = 10 * time.Minute
	RenewEvery = 2 * time.Minute
)

// Registry is the control plane's record of live entries, by worker. Dispatch adds a job's
// entries when it places the job and renews them while it runs; each worker's broker pulls its own
// share (GET /api/v1/broker/entries). Like the broker's Store it lives in memory only: a
// restarted control plane forgets every entry, and running jobs lose their credentials.
type Registry struct {
	mu       sync.Mutex
	byWorker map[string]map[string]Entry
	max      int
	now      func() time.Time
}

// NewRegistry returns an empty registry holding at most max entries per worker
// (DefaultMaxEntries when max <= 0), the same cap a broker's Store enforces.
func NewRegistry(max int) *Registry {
	if max <= 0 {
		max = DefaultMaxEntries
	}
	return &Registry{byWorker: map[string]map[string]Entry{}, max: max, now: time.Now}
}

// Add records e for worker with a fresh lease.
func (r *Registry) Add(worker string, e Entry) error {
	if worker == "" {
		return fmt.Errorf("broker: entry for job %q names no worker", e.Job)
	}
	if err := validEntry(e); err != nil {
		return fmt.Errorf("broker: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	m := r.byWorker[worker]
	if m == nil {
		m = map[string]Entry{}
		r.byWorker[worker] = m
	}
	if _, exists := m[e.Placeholder]; !exists && len(m) >= r.max {
		return fmt.Errorf("%w: worker %s already has %d", ErrTooMany, worker, len(m))
	}
	e.Expires = r.now().Add(Lease)
	m[e.Placeholder] = e
	return nil
}

// Renew extends the lease of every entry of job and returns how many it renewed.
func (r *Registry) Renew(job string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	until := r.now().Add(Lease)
	n := 0
	for _, m := range r.byWorker {
		for p, e := range m {
			if e.Job == job {
				e.Expires = until
				m[p] = e
				n++
			}
		}
	}
	return n
}

// RemoveJob forgets every entry of job; its worker's broker drops them on its next sync.
func (r *Registry) RemoveJob(job string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for w, m := range r.byWorker {
		for p, e := range m {
			if e.Job == job {
				delete(m, p)
				n++
			}
		}
		if len(m) == 0 {
			delete(r.byWorker, w)
		}
	}
	return n
}

// For returns worker's live entries, sorted by placeholder.
func (r *Registry) For(worker string) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	out := make([]Entry, 0, len(r.byWorker[worker]))
	for _, e := range r.byWorker[worker] {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Placeholder < out[j].Placeholder })
	return out
}

// Len is the number of live entries across all workers.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	n := 0
	for _, m := range r.byWorker {
		n += len(m)
	}
	return n
}

func (r *Registry) sweepLocked() {
	now := r.now()
	for w, m := range r.byWorker {
		for p, e := range m {
			if !now.Before(e.Expires) {
				delete(m, p)
			}
		}
		if len(m) == 0 {
			delete(r.byWorker, w)
		}
	}
}
