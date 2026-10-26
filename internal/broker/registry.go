package broker

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Lease is how long a broker keeps an entry after the control plane last listed it. The control
// plane stamps every entry it serves with now+Lease, and a broker pulls every DefaultSyncInterval,
// so a broker that loses touch with the control plane drops a running job's tokens within Lease.
const Lease = 10 * time.Minute

// Registry is the control plane's record of live entries, by job. Dispatch puts a job's entries
// when it submits the job and forgets them when the job ends; each worker's broker pulls the
// entries of the jobs placed on it (GET /api/v1/broker/entries). Like the broker's Store it lives
// in memory only: after a control-plane restart a running job's tokens are back only once its
// submitter sends them again.
type Registry struct {
	mu    sync.Mutex
	byJob map[string][]Entry
	max   int
}

// NewRegistry returns an empty registry holding at most max entries in all
// (DefaultMaxEntries when max <= 0).
func NewRegistry(max int) *Registry {
	if max <= 0 {
		max = DefaultMaxEntries
	}
	return &Registry{byJob: map[string][]Entry{}, max: max}
}

// Put replaces job's entries with entries.
func (r *Registry) Put(job string, entries []Entry) error {
	if job == "" {
		return fmt.Errorf("broker: entries name no job")
	}
	for _, e := range entries {
		if err := validEntry(e); err != nil {
			return fmt.Errorf("broker: %w", err)
		}
		if e.Job != job {
			return fmt.Errorf("broker: entry for job %q put under job %q", e.Job, job)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(entries)
	for j, es := range r.byJob {
		if j != job {
			n += len(es)
		}
	}
	if n > r.max {
		return fmt.Errorf("%w: %d > %d", ErrTooMany, n, r.max)
	}
	if len(entries) == 0 {
		delete(r.byJob, job)
		return nil
	}
	r.byJob[job] = append([]Entry(nil), entries...)
	return nil
}

// Job returns job's entries.
func (r *Registry) Job(job string) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Entry(nil), r.byJob[job]...)
}

// Jobs returns every job with entries, sorted.
func (r *Registry) Jobs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.byJob))
	for j := range r.byJob {
		out = append(out, j)
	}
	sort.Strings(out)
	return out
}

// RemoveJob forgets job's entries; its worker's broker drops them on its next sync.
func (r *Registry) RemoveJob(job string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.byJob[job])
	delete(r.byJob, job)
	return n
}

// Len is the number of entries held.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, es := range r.byJob {
		n += len(es)
	}
	return n
}
