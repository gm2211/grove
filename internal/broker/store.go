// Package broker is the credential broker that runs on each worker Mac's host, outside the
// hypervisor. A job never holds a real credential: it gets a placeholder ("gph1_…"), and its
// requests to a short list of upstreams (GitHub, Anthropic, OpenAI/ChatGPT) go through the broker,
// which swaps the placeholder for the real token on the way out and strips the real token from the
// response on the way back. Everything else a job does goes direct; the broker only tunnels it
// (to public addresses only) because tools send all their traffic to one HTTPS_PROXY.
//
// The broker keeps every entry in memory only. Entries come from the control plane (see Syncer) and
// die four ways: the job ends (the control plane stops listing it), the lease runs out, the VM goes
// away, or the broker restarts.
package broker

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// PlaceholderPrefix starts every placeholder, so the broker can find them in headers and so a
// leaked one is recognisable in logs.
const PlaceholderPrefix = "gph1_"

// placeholderRE matches a placeholder: the prefix and 52 base32 characters (256 random bits).
var placeholderRE = regexp.MustCompile(`gph1_[a-z2-7]{52}`)

// DefaultMaxEntries caps how many live entries one broker holds. Past it, Replace refuses the
// update rather than growing without bound.
const DefaultMaxEntries = 1000

// Kind names an upstream service. The hosts a kind's token may be sent to are fixed in code
// (kindHosts), never configured, so no entry can point a token at another host.
type Kind string

const (
	KindGitHub    Kind = "github"
	KindAnthropic Kind = "anthropic"
	KindOpenAI    Kind = "openai"
)

var kindHosts = map[Kind][]string{
	KindGitHub:    {"github.com", "api.github.com"},
	KindAnthropic: {"api.anthropic.com"},
	KindOpenAI:    {"api.openai.com", "chatgpt.com"},
}

// Hosts returns the upstream hosts a kind's token may be sent to.
func (k Kind) Hosts() []string { return append([]string(nil), kindHosts[k]...) }

// Valid reports whether k is a kind the broker knows.
func (k Kind) Valid() bool { _, ok := kindHosts[k]; return ok }

// InjectHosts is every host the broker terminates TLS for (and so may inject into).
func InjectHosts() []string {
	var out []string
	for _, hs := range kindHosts {
		out = append(out, hs...)
	}
	sort.Strings(out)
	return out
}

func hostAllowed(k Kind, host string) bool {
	for _, h := range kindHosts[k] {
		if h == host {
			return true
		}
	}
	return false
}

// Entry is one placeholder and the real credential behind it.
type Entry struct {
	Placeholder string `json:"placeholder"`
	Job         string `json:"job"`
	// VM is the Orchard VM the job runs in. Source, when valid, is that VM's address on the
	// bridge; a request carrying this placeholder from any other address is refused.
	VM      string     `json:"vm,omitempty"`
	Source  netip.Addr `json:"source,omitempty"`
	Kind    Kind       `json:"kind"`
	Token   string     `json:"token"`
	Expires time.Time  `json:"expires"`
}

// NewPlaceholder returns a fresh placeholder: the prefix and 256 random bits, lower-case base32.
func NewPlaceholder() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return PlaceholderPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

// CheckToken accepts a token that can go in a header as is: printable ASCII, no spaces. Anything
// else could split a header, and would also break the stripper's no-pattern-spans-a-newline rule.
func CheckToken(t string) error {
	if t == "" {
		return errors.New("no token")
	}
	for i := 0; i < len(t); i++ {
		if t[i] <= ' ' || t[i] > '~' {
			return errors.New("token has a character that can't go in a header")
		}
	}
	return nil
}

// validEntry checks what Replace and Registry.Add need of an entry.
func validEntry(e Entry) error {
	if !placeholderRE.MatchString(e.Placeholder) || len(e.Placeholder) != len(PlaceholderPrefix)+52 {
		return fmt.Errorf("entry for job %q has a malformed placeholder", e.Job)
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("entry for job %q has unknown kind %q", e.Job, e.Kind)
	}
	if err := CheckToken(e.Token); err != nil {
		return fmt.Errorf("entry for job %q: %w", e.Job, err)
	}
	return nil
}

// Store holds the live entries.
type Store struct {
	mu      sync.RWMutex
	entries map[string]Entry
	max     int
	now     func() time.Time
}

// NewStore returns an empty store holding at most max entries (DefaultMaxEntries when max <= 0).
func NewStore(max int) *Store {
	if max <= 0 {
		max = DefaultMaxEntries
	}
	return &Store{entries: map[string]Entry{}, max: max, now: time.Now}
}

// ErrTooMany is returned by Replace when an update holds more entries than the store allows.
var ErrTooMany = errors.New("broker: too many entries")

// Replace swaps the whole entry set for entries. The control plane lists every live entry for
// this worker on each sync, so an entry missing from the list is gone: that is how a finished job's
// tokens leave the broker.
func (s *Store) Replace(entries []Entry) error {
	if len(entries) > s.max {
		return fmt.Errorf("%w: %d > %d", ErrTooMany, len(entries), s.max)
	}
	next := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if err := validEntry(e); err != nil {
			return fmt.Errorf("broker: %w", err)
		}
		next[e.Placeholder] = e
	}
	s.mu.Lock()
	s.entries = next
	s.mu.Unlock()
	return nil
}

// Sweep drops every entry whose lease has run out and returns how many it dropped. Lookup already
// ignores expired entries; Sweep is what frees their memory when the control plane can't be reached.
func (s *Store) Sweep() int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p, e := range s.entries {
		if !now.Before(e.Expires) {
			delete(s.entries, p)
			n++
		}
	}
	return n
}

// DropVM removes every entry issued to vm.
func (s *Store) DropVM(vm string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p, e := range s.entries {
		if e.VM == vm {
			delete(s.entries, p)
			n++
		}
	}
	return n
}

// Len is the number of entries held, expired ones included until the next Sweep.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Lookup errors. They never carry the token.
var (
	ErrUnknownPlaceholder = errors.New("unknown or expired placeholder")
	ErrWrongHost          = errors.New("placeholder not valid for this host")
	ErrWrongSource        = errors.New("placeholder not valid from this address")
)

// Lookup returns the entry for placeholder if it is live, may be sent to host, and was issued to
// the VM at src (when the entry names a source).
func (s *Store) Lookup(placeholder, host string, src netip.Addr) (Entry, error) {
	s.mu.RLock()
	e, ok := s.entries[placeholder]
	s.mu.RUnlock()
	if !ok || !s.now().Before(e.Expires) {
		return Entry{}, ErrUnknownPlaceholder
	}
	if !hostAllowed(e.Kind, host) {
		return Entry{}, ErrWrongHost
	}
	if e.Source.IsValid() && e.Source.Unmap() != src.Unmap() {
		return Entry{}, ErrWrongSource
	}
	return e, nil
}
