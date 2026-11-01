// Package snooze records why a worker Mac is paused, so a timed snooze can end by itself without
// ever undoing a pause somebody else put there.
//
// Orchard has one bit per worker (SchedulingPaused). Several things can set it: the owner
// snoozing the Mac for a while, grove's health monitor after a run of lost jobs, an operator's
// `grove worker pause`, or something outside grove entirely. The Store keeps one record per
// reason, and the invariant is: the Orchard bit is set iff at least one reason is present. A
// snooze expiring (or `wake`) clears only the snooze reason, and resumes the worker only when no
// other reason is left. A pause that was already in Orchard when a snooze began, with no reason
// recorded, is kept as the "external" operator reason and never resumed by a snooze.
package snooze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// Mode says what a snooze does to jobs already running on the worker.
type Mode string

const (
	// ModeDrain lets running jobs finish; nothing new is placed (the default).
	ModeDrain Mode = "drain"
	// ModeNow also requeues the worker's retryable jobs elsewhere right away.
	ModeNow Mode = "now"
)

// Reason names for Reasons.
const (
	ReasonSnooze   = "snooze"
	ReasonHealth   = "health"
	ReasonOperator = "operator"
)

// MaxDuration is the longest snooze the server accepts; DefaultDuration the one used when a
// request names none.
const (
	MaxDuration     = 24 * time.Hour
	DefaultDuration = 2 * time.Hour
)

// ExternalBy is the Operator.By of a pause grove found in Orchard with no recorded reason.
const ExternalBy = "external"

// Snooze is the owner's timed pause.
type Snooze struct {
	Until time.Time `json:"until"`
	Mode  Mode      `json:"mode"`
	By    string    `json:"by,omitempty"`
	At    time.Time `json:"at"`
	Note  string    `json:"note,omitempty"`
}

// Health is grove's own pause of a worker that kept losing jobs.
type Health struct {
	At   time.Time `json:"at"`
	Jobs int       `json:"jobs,omitempty"`
}

// Operator is an operator's pause (`grove worker pause`), or "external" for one grove did not make.
type Operator struct {
	At time.Time `json:"at"`
	By string    `json:"by,omitempty"`
}

// Entry is every reason one worker is paused.
type Entry struct {
	Snooze   *Snooze   `json:"snooze,omitempty"`
	Health   *Health   `json:"health,omitempty"`
	Operator *Operator `json:"operator,omitempty"`
}

func (e Entry) empty() bool { return e.Snooze == nil && e.Health == nil && e.Operator == nil }

// Active reports whether e's snooze is in force at now.
func (e Entry) Active(now time.Time) bool { return e.Snooze != nil && now.Before(e.Snooze.Until) }

// Reasons lists the reasons in force at now, in a fixed order.
func (e Entry) Reasons(now time.Time) []string {
	var out []string
	if e.Active(now) {
		out = append(out, ReasonSnooze)
	}
	if e.Health != nil {
		out = append(out, ReasonHealth)
	}
	if e.Operator != nil {
		out = append(out, ReasonOperator)
	}
	return out
}

type state struct {
	Version int              `json:"version"`
	Workers map[string]Entry `json:"workers"`
}

// Store persists the entries at path (mode 0600, atomic rename). A nil *Store reads as empty.
type Store struct {
	mu       sync.Mutex
	path     string
	readOnly bool
	state    state
}

// NewStore opens (creating when absent) the store at path. It refuses a symlink.
func NewStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("snooze: store path is required")
	}
	s := &Store{path: path, state: state{Version: 1, Workers: map[string]Entry{}}}
	exists, err := s.read()
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := s.persistLocked(); err != nil {
			return nil, fmt.Errorf("snooze: create store: %w", err)
		}
	} else if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("snooze: secure store: %w", err)
	}
	return s, nil
}

// Load opens path read-only, for a process that only looks (the sandbox runner). A missing file is
// an empty store; every mutator on it fails.
func Load(path string) (*Store, error) {
	s := &Store{path: path, readOnly: true, state: state{Version: 1, Workers: map[string]Entry{}}}
	if strings.TrimSpace(path) == "" {
		return s, nil
	}
	if _, err := s.read(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) read() (bool, error) {
	info, err := os.Lstat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("snooze: stat store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("snooze: %s is a symlink; refusing to use it", s.path)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return false, fmt.Errorf("snooze: read store: %w", err)
	}
	var loaded state
	if err := json.Unmarshal(data, &loaded); err != nil {
		return false, fmt.Errorf("snooze: parse store: %w", err)
	}
	if loaded.Workers == nil {
		loaded.Workers = map[string]Entry{}
	}
	loaded.Version = 1
	s.state = loaded
	return true, nil
}

// Reload re-reads a store from disk, so a long-lived read-only reader sees later changes.
func (s *Store) Reload() error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.state
	s.state = state{Version: 1, Workers: map[string]Entry{}}
	if _, err := s.read(); err != nil {
		s.state = prev
		return err
	}
	return nil
}

// Get returns worker's entry (zero when there is none).
func (s *Store) Get(worker string) Entry {
	if s == nil {
		return Entry{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyEntry(s.state.Workers[worker])
}

// List returns every worker's entry.
func (s *Store) List() map[string]Entry {
	out := map[string]Entry{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.state.Workers {
		out[k] = copyEntry(v)
	}
	return out
}

// Reasons lists the reasons worker is paused for at now.
func (s *Store) Reasons(worker string, now time.Time) []string {
	return s.Get(worker).Reasons(now)
}

func copyEntry(e Entry) Entry {
	if e.Snooze != nil {
		v := *e.Snooze
		e.Snooze = &v
	}
	if e.Health != nil {
		v := *e.Health
		e.Health = &v
	}
	if e.Operator != nil {
		v := *e.Operator
		e.Operator = &v
	}
	return e
}

// mutate applies fn to worker's entry and persists, rolling back on a write failure.
func (s *Store) mutate(worker string, fn func(*Entry)) error {
	if s == nil || s.readOnly {
		return errors.New("snooze: store is not writable")
	}
	if strings.TrimSpace(worker) == "" {
		return errors.New("snooze: worker name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.state.Workers[worker]
	e := copyEntry(prev)
	fn(&e)
	if e.empty() {
		delete(s.state.Workers, worker)
	} else {
		s.state.Workers[worker] = e
	}
	if err := s.persistLocked(); err != nil {
		if had {
			s.state.Workers[worker] = prev
		} else {
			delete(s.state.Workers, worker)
		}
		return fmt.Errorf("snooze: persist: %w", err)
	}
	return nil
}

// SetSnooze records (or replaces) worker's snooze.
func (s *Store) SetSnooze(worker string, sn Snooze) error {
	if sn.Mode == "" {
		sn.Mode = ModeDrain
	}
	if sn.Mode != ModeDrain && sn.Mode != ModeNow {
		return fmt.Errorf("snooze: unknown mode %q", sn.Mode)
	}
	return s.mutate(worker, func(e *Entry) { e.Snooze = &sn })
}

// ClearSnooze drops worker's snooze reason only.
func (s *Store) ClearSnooze(worker string) error {
	return s.mutate(worker, func(e *Entry) { e.Snooze = nil })
}

// SetHealth records grove's own health pause.
func (s *Store) SetHealth(worker string, at time.Time, jobs int) error {
	return s.mutate(worker, func(e *Entry) { e.Health = &Health{At: at, Jobs: jobs} })
}

// SetOperator records an operator pause.
func (s *Store) SetOperator(worker, by string, at time.Time) error {
	return s.mutate(worker, func(e *Entry) { e.Operator = &Operator{At: at, By: by} })
}

// ClearOperatorAndHealth drops the operator and health reasons (what `worker resume` undoes).
func (s *Store) ClearOperatorAndHealth(worker string) error {
	return s.mutate(worker, func(e *Entry) { e.Operator, e.Health = nil, nil })
}

func (s *Store) persistLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(s.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write it", s.path)
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	return os.Chmod(s.path, 0o600)
}

// ErrUnknownWorker is returned when Orchard has no worker of that name.
var ErrUnknownWorker = errors.New("snooze: unknown worker")

// Pauser is the slice of the Orchard client the snooze logic needs.
type Pauser interface {
	ListWorkers(ctx context.Context) ([]orchard.Worker, error)
	PauseWorker(ctx context.Context, name string) error
	ResumeWorker(ctx context.Context, name string) error
}

// Engage records sn for worker and pauses it in Orchard. A pause already in Orchard with no
// recorded reason is first recorded as the external operator reason, so the snooze never resumes it.
func Engage(ctx context.Context, p Pauser, st *Store, worker string, sn Snooze, now time.Time) error {
	workers, err := p.ListWorkers(ctx)
	if err != nil {
		return err
	}
	var w *orchard.Worker
	for i := range workers {
		if workers[i].Name == worker {
			w = &workers[i]
		}
	}
	if w == nil {
		return fmt.Errorf("%w %q", ErrUnknownWorker, worker)
	}
	if w.SchedulingPaused && len(st.Reasons(worker, now)) == 0 {
		if err := st.SetOperator(worker, ExternalBy, now); err != nil {
			return err
		}
	}
	if err := st.SetSnooze(worker, sn); err != nil {
		return err
	}
	if !w.SchedulingPaused {
		return p.PauseWorker(ctx, worker)
	}
	return nil
}

// Wake ends worker's snooze and resumes it when no other reason remains. It returns the reasons
// that keep it paused.
func Wake(ctx context.Context, p Pauser, st *Store, worker string, now time.Time) (resumed bool, still []string, err error) {
	if err := st.ClearSnooze(worker); err != nil {
		return false, nil, err
	}
	still = st.Reasons(worker, now)
	if len(still) > 0 {
		return false, still, nil
	}
	if err := p.ResumeWorker(ctx, worker); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}

// Expirer ends lapsed snoozes and keeps Orchard's pause bit matching the recorded reasons.
type Expirer struct {
	Store   *Store
	Orchard Pauser
	Now     func() time.Time
}

// Tick runs one pass. It is safe to run at startup and on every reconcile interval: it clears only
// an expired snooze (resuming the worker only when nothing else keeps it paused), and re-applies
// the pause when Orchard shows a worker unpaused that still has a reason.
func (e *Expirer) Tick(ctx context.Context) error {
	if e == nil || e.Store == nil || e.Orchard == nil {
		return nil
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	entries := e.Store.List()
	if len(entries) == 0 {
		return nil
	}
	workers, err := e.Orchard.ListWorkers(ctx)
	if err != nil {
		return err
	}
	byName := map[string]orchard.Worker{}
	for _, w := range workers {
		byName[w.Name] = w
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		entry := entries[name]
		w, known := byName[name]
		if entry.Snooze != nil && !entry.Active(now) {
			if err := e.Store.ClearSnooze(name); err != nil {
				errs = append(errs, err)
				continue
			}
			if len(e.Store.Reasons(name, now)) == 0 {
				if known && w.SchedulingPaused {
					if err := e.Orchard.ResumeWorker(ctx, name); err != nil {
						errs = append(errs, fmt.Errorf("resume %s after snooze: %w", name, err))
					}
				}
				continue
			}
		}
		if known && !w.SchedulingPaused && len(e.Store.Reasons(name, now)) > 0 {
			if err := e.Orchard.PauseWorker(ctx, name); err != nil {
				errs = append(errs, fmt.Errorf("re-pause %s: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}
