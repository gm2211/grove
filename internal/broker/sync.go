package broker

import (
	"context"
	"log/slog"
	"net/netip"
	"sort"
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
	Fetch func(ctx context.Context) ([]Entry, error)
	// Sources resolves each entry's VM to its address on this worker's bridge. An entry is
	// installed only with that address as its Source; with no resolver, none is installed.
	Sources  SourceResolver
	Store    *Store
	Interval time.Duration
	Log      *slog.Logger

	mu     sync.Mutex
	last   time.Time
	synced bool
	now    func() time.Time
	// unbound is, per VM, the reason last logged for leaving its entries out, so a VM that stays
	// unresolved is reported once and not on every sync.
	unbound map[string]string
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

// Sync pulls the entries, binds each to its VM's address and replaces the store's with those,
// unless the last sync was under two seconds ago. It is the Proxy's Resync, which is how an entry
// left out for want of an address is tried again as soon as its job uses it. It also sweeps
// expired entries either way.
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
	bound := s.bind(ctx, entries, log)
	if err := s.Store.Replace(bound); err != nil {
		log.Error("broker: control plane sent entries the broker refuses; keeping current ones", "err", err)
		return
	}
	s.synced = true
	log.Debug("broker: synced", "entries", len(bound), "unbound", len(entries)-len(bound))
}

// bind returns the entries whose VM has an address on this worker, each with that address as its
// Source, whatever Source the control plane sent. The rest are left out, never installed unbound:
// an entry that names no VM, one whose VM isn't running here or has no address yet, and all of
// them when there is no resolver. The reason is logged by VM and job; an entry's token and
// placeholder never are. The caller holds s.mu.
func (s *Syncer) bind(ctx context.Context, entries []Entry, log *slog.Logger) []Entry {
	if len(entries) == 0 {
		s.unbound = nil
		return nil
	}
	seen := map[string]bool{}
	var vms []string
	for _, e := range entries {
		if !seen[e.VM] {
			seen[e.VM] = true
			vms = append(vms, e.VM)
		}
	}
	sort.Strings(vms)
	var addrs map[string]netip.Addr
	var failed map[string]error
	if s.Sources != nil {
		addrs, failed = s.Sources.Sources(ctx, vms)
	}
	bound := make([]Entry, 0, len(entries))
	jobs := map[string][]string{}
	for _, e := range entries {
		// The empty name is checked here too, so no resolver can bind an entry that names no VM.
		if a, ok := addrs[e.VM]; ok && e.VM != "" && a.IsValid() {
			e.Source = a.Unmap()
			bound = append(bound, e)
			continue
		}
		if n := len(jobs[e.VM]); n == 0 || jobs[e.VM][n-1] != e.Job {
			jobs[e.VM] = append(jobs[e.VM], e.Job)
		}
	}
	unbound := make(map[string]string, len(jobs))
	for _, vm := range vms {
		if _, left := jobs[vm]; !left {
			continue
		}
		reason := "no address for this VM"
		switch err := failed[vm]; {
		case s.Sources == nil:
			reason = "this broker has no way to resolve a VM's address"
		case vm == "":
			reason = errNoVM.Error()
		case err != nil:
			reason = err.Error()
		}
		unbound[vm] = reason
		if s.unbound[vm] != reason {
			log.Warn("broker: left out the entries of a VM with no address to bind them to; its jobs' placeholders are refused until it resolves", "vm", vm, "jobs", jobs[vm], "reason", reason)
		}
	}
	s.unbound = unbound
	return bound
}
