package broker

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultSyncInterval is how often a broker pulls its entries. It bounds how long a finished job's
// tokens stay on the worker.
const DefaultSyncInterval = 30 * time.Second

// minResync is the least time between two syncs, so a job spraying unknown placeholders can't
// turn the broker into a load generator against the control plane.
const minResync = 2 * time.Second

// Syncer keeps a Store equal to the control plane's list of this worker's live entries.
type Syncer struct {
	// Fetch returns every live entry for this worker.
	Fetch    func(ctx context.Context) ([]Entry, error)
	Store    *Store
	Interval time.Duration
	Log      *slog.Logger

	mu     sync.Mutex
	last   time.Time
	synced bool
	now    func() time.Time
}

// Synced reports whether the latest sync with the control plane succeeded.
func (s *Syncer) Synced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced
}

// Run syncs now and then every Interval until ctx is done. A failed sync keeps the entries the
// broker has; each still dies when its lease runs out.
func (s *Syncer) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync pulls the entries and replaces the store's, unless the last sync was under two seconds ago.
// It is the Proxy's Resync. It also sweeps expired entries either way.
func (s *Syncer) Sync(ctx context.Context) {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.Store.Sweep()
	if !s.last.IsZero() && now().Sub(s.last) < minResync {
		return
	}
	entries, err := s.Fetch(ctx)
	// Measure the gap from the end of this fetch, so requests that queued behind a slow one
	// don't each fetch again.
	s.last = now()
	s.synced = false
	if err != nil {
		log.Warn("broker: sync with the control plane failed; keeping current entries until their leases end", "err", err, "entries", s.Store.Len())
		return
	}
	if err := s.Store.Replace(entries); err != nil {
		log.Error("broker: control plane sent entries the broker refuses; keeping current ones", "err", err)
		return
	}
	s.synced = true
	log.Debug("broker: synced", "entries", len(entries))
}
