package snooze

import (
	"sort"
	"sync"
	"time"
)

// A worker's pause state lives in two places, this store and Orchard's pause bit, and every
// writer (Engage, Wake, the Expirer, `worker pause`/`resume`, the health monitor) reads one and
// writes the other in several steps. spec/quint/snooze.qnt shows TLC the interleavings that break
// them when two run at once: a resume landing mid-snooze un-pauses a snoozed Mac, a snooze landing
// mid-resume leaves a pause nobody recorded (which then never ends). Each writer therefore holds
// the worker's lock from its first read to its last write.

// workerLocks is the per-worker mutexes; the zero value is ready.
type workerLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (l *workerLocks) get(worker string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*sync.Mutex{}
	}
	m := l.m[worker]
	if m == nil {
		m = &sync.Mutex{}
		l.m[worker] = m
	}
	return m
}

// Lock takes worker's lock and returns the function that releases it. A nil Store locks nothing.
func (s *Store) Lock(worker string) (unlock func()) {
	if s == nil {
		return func() {}
	}
	m := s.locks.get(worker)
	m.Lock()
	return m.Unlock
}

// lockAll takes the locks of workers in name order (the Expirer's pass), so it can't deadlock with
// a writer holding one.
func (s *Store) lockAll(workers []string) (unlock func()) {
	names := append([]string(nil), workers...)
	sort.Strings(names)
	var unlocks []func()
	for _, n := range names {
		unlocks = append(unlocks, s.Lock(n))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

// EndedAt is when worker's snooze was last cleared by this process (Wake or the Expirer, after
// the worker was resumed), or the zero time. In memory only: grove serve's PoolHealth, which
// compares it with its own fleet observations, starts empty too.
func (s *Store) EndedAt(worker string) time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended[worker]
}
