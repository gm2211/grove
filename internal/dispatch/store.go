package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// record is what's persisted for one submitted job. It wraps the public Job (returned to API
// callers) with the bookkeeping needed to talk to Nomad: the actual dispatched Nomad job id.
//
// Job.ID is the id grove hands out to callers. It's minted before Dispatch is called (so it can
// be baked into the artifact_prefix meta the running task sees), so it is *not* literally Nomad's
// own dispatched-job id — that value is kept separately as NomadJobID and used for all
// ListAllocations/GetAllocation/StopJob calls.
type record struct {
	Job
	NomadJobID string `json:"nomadJobId"`
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
