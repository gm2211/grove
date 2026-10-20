package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// record is what's persisted for one submitted job: the public Job (returned to API callers) plus
// bookkeeping that never leaves the control plane.
type record struct {
	Job
	// LegacyNomadJobID is set on history written while grove dispatched jobs to Nomad. A
	// non-terminal job carrying one is marked lost at startup: nothing runs it any more.
	LegacyNomadJobID string `json:"nomadJobId,omitempty"`
	// TempFailRetries counts how often the job went back to its queue after ExitTempFail.
	TempFailRetries int `json:"tempFailRetries,omitempty"`
}

// store is a thread-safe in-memory job store, persisted as JSON so history survives restarts.
type store struct {
	mu   sync.RWMutex
	path string
	jobs map[string]*record
}

func defaultStorePath() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "grove", "jobs.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "grove", "jobs.json")
}

func newStore(path string) (*store, error) {
	s := &store{path: path, jobs: map[string]*record{}}
	if path == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	var recs []*record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, err
	}
	for _, r := range recs {
		s.jobs[r.ID] = r
	}
	return s, nil
}

func (s *store) put(r *record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *r
	s.jobs[r.ID] = &cp
	return s.persistLocked()
}

func (s *store) get(id string) (*record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	cp := *r
	return &cp, true
}

// findByIdempotencyKey returns the first record whose JobRequest.IdempotencyKey matches key.
// Linear scan over s.jobs is fine — job counts are small.
func (s *store) findByIdempotencyKey(key string) (*record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.jobs {
		if r.Request.IdempotencyKey == key {
			cp := *r
			return &cp, true
		}
	}
	return nil, false
}

func (s *store) list() []record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]record, 0, len(s.jobs))
	for _, r := range s.jobs {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubmittedAt.Before(out[j].SubmittedAt) })
	return out
}

// persistLocked writes the whole store to disk. Callers must hold s.mu.
func (s *store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	out := make([]*record, 0, len(s.jobs))
	for _, r := range s.jobs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubmittedAt.Before(out[j].SubmittedAt) })
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// queueEntry is what the queue spool keeps for a job that hasn't started yet: the request's real
// env values, which the job history never holds (it keeps only the keys). With it a queued job
// survives a `grove serve` restart; it is deleted as soon as the job starts or ends. Never the
// private-repo credential, which is resolved only when the job starts.
type queueEntry struct {
	Env map[string]string `json:"env,omitempty"`
}

// spool is the on-disk queue: one 0600 file per queued job in a 0700 directory. A zero spool
// (empty dir) keeps nothing, for a service with no persistent store.
type spool struct{ dir string }

func (q spool) path(id string) string { return filepath.Join(q.dir, id+".json") }

func (q spool) put(id string, e queueEntry) error {
	if q.dir == "" {
		return nil
	}
	if err := os.MkdirAll(q.dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp := q.path(id) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, q.path(id))
}

// get returns id's entry and whether there is one.
func (q spool) get(id string) (queueEntry, bool) {
	var e queueEntry
	if q.dir == "" {
		return e, false
	}
	data, err := os.ReadFile(q.path(id))
	if err != nil || json.Unmarshal(data, &e) != nil {
		return e, false
	}
	return e, true
}

func (q spool) remove(id string) {
	if q.dir != "" {
		_ = os.Remove(q.path(id))
	}
}
