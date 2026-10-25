// Package secrets is the control plane's store of named job secrets: values an operator sets once
// (`grove secret set`, PUT /api/v1/secrets/{name}) that a job asks for by name
// (JobRequest.Secrets) instead of its submitter sending the value inline.
//
// A secret reaches a job as an environment variable of the same name, resolved at dispatch time
// by internal/dispatch (Options.Secrets). The submitter never handles the value, and the value
// never lands in the job history or any API response — only the names do.
//
// Each secret names who may request it (Allow): a principal id (a device credential's id, or a
// Tailscale identity's "tailscale:<stable id>") or a Tailscale node tag ("tag:argos"). An
// operator may request any secret. A secret with an empty Allow list is operator-only.
package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxValueLen bounds a secret's value: room for a multi-line credential file (a Codex auth.json),
// small enough to travel in Nomad dispatch meta.
const MaxValueLen = 64 << 10

// usagePersistInterval throttles how often Resolve rewrites the store just for LastUsedAt.
const usagePersistInterval = time.Minute

// ErrNotFound is returned for a secret name the store doesn't hold.
var ErrNotFound = errors.New("secret not found")

// namePattern is a portable shell variable name: the secret is exported under it.
var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// reservedNames are variables the job wrapper or the shell itself depends on; a secret by one of
// these names would break the job rather than configure it.
var reservedNames = map[string]struct{}{
	"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "SHELL": {}, "TMPDIR": {}, "IFS": {},
	"BASH_ENV": {}, "ENV": {}, "LD_PRELOAD": {}, "LD_LIBRARY_PATH": {}, "DYLD_INSERT_LIBRARIES": {},
}

// ValidateName reports whether name may be a secret's name.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("secret name %q must be a shell variable name (letters, digits, _; not starting with a digit)", name)
	}
	upper := strings.ToUpper(name)
	if _, reserved := reservedNames[upper]; reserved {
		return fmt.Errorf("secret name %q is reserved", name)
	}
	if strings.HasPrefix(upper, "NOMAD_") || strings.HasPrefix(upper, "GROVE_") {
		return fmt.Errorf("secret name %q uses a prefix grove reserves (NOMAD_, GROVE_)", name)
	}
	return nil
}

// Secret is the public view of a stored secret. It carries no value: values are write-only
// through the API, and Fingerprint identifies which one is set.
type Secret struct {
	Name        string     `json:"name"`
	Allow       []string   `json:"allow,omitempty"`
	Fingerprint string     `json:"fingerprint"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	UpdatedBy   string     `json:"updatedBy,omitempty"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
}

// SetRequest is the wire body of PUT /api/v1/secrets/{name}.
type SetRequest struct {
	Value string   `json:"value"`
	Allow []string `json:"allow,omitempty"`
}

// Caller is who is asking to use secrets in a job.
type Caller struct {
	ID       string
	Tags     []string
	Operator bool
}

type record struct {
	Secret
	Value string `json:"value"`
}

type file struct {
	Version int      `json:"version"`
	Secrets []record `json:"secrets"`
}

// Store persists secrets at path, mode 0600, through an atomic same-directory rename. A nil
// *Store holds nothing: Authorize and Resolve fail and List is empty.
type Store struct {
	mu      sync.Mutex
	path    string
	records map[string]record
}

// NewStore loads path, creating an empty mode-0600 store when absent.
func NewStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("secrets: store path is required")
	}
	s := &Store{path: path, records: map[string]record{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("secrets: read store: %w", err)
		}
		if err := s.persistLocked(); err != nil {
			return nil, fmt.Errorf("secrets: create store: %w", err)
		}
		return s, nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secrets: secure store: %w", err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("secrets: parse store: %w", err)
	}
	for _, rec := range f.Secrets {
		if err := ValidateName(rec.Name); err != nil {
			return nil, fmt.Errorf("secrets: invalid store record: %w", err)
		}
		if _, dup := s.records[rec.Name]; dup {
			return nil, fmt.Errorf("secrets: invalid store record: duplicate name %q", rec.Name)
		}
		s.records[rec.Name] = rec
	}
	return s, nil
}

// Set creates or replaces the secret name. by is the operator principal's id.
func (s *Store) Set(name, value string, allow []string, by string) (Secret, error) {
	if s == nil {
		return Secret{}, errors.New("secrets: named secrets are not configured on this control plane")
	}
	if err := ValidateName(name); err != nil {
		return Secret{}, err
	}
	if value == "" {
		return Secret{}, errors.New("secrets: value is required")
	}
	if len(value) > MaxValueLen {
		return Secret{}, fmt.Errorf("secrets: value is longer than %d bytes", MaxValueLen)
	}
	if strings.ContainsRune(value, 0) {
		return Secret{}, errors.New("secrets: value contains a NUL byte")
	}
	normalized, err := normalizeAllow(allow)
	if err != nil {
		return Secret{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	prev, existed := s.records[name]
	rec := record{Secret: Secret{
		Name: name, Allow: normalized, Fingerprint: fingerprint(value),
		CreatedAt: now, UpdatedAt: now, UpdatedBy: strings.TrimSpace(by),
	}, Value: value}
	if existed {
		rec.CreatedAt = prev.CreatedAt
		rec.LastUsedAt = prev.LastUsedAt
	}
	s.records[name] = rec
	if err := s.persistLocked(); err != nil {
		if existed {
			s.records[name] = prev
		} else {
			delete(s.records, name)
		}
		return Secret{}, fmt.Errorf("secrets: persist: %w", err)
	}
	return publicView(rec), nil
}

// Delete removes name, wiping its value from disk.
func (s *Store) Delete(name string) error {
	if s == nil {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.records[name]
	if !ok {
		return ErrNotFound
	}
	delete(s.records, name)
	if err := s.persistLocked(); err != nil {
		s.records[name] = prev
		return fmt.Errorf("secrets: persist: %w", err)
	}
	return nil
}

// List returns every secret's public view, sorted by name.
func (s *Store) List() []Secret {
	if s == nil {
		return []Secret{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Secret, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, publicView(rec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Authorize returns an error naming the first secret c may not use. A non-operator gets the same
// error for a secret that doesn't exist as for one it isn't granted, so a dispatch credential
// can't probe which names are set.
func (s *Store) Authorize(names []string, c Caller) error {
	if s == nil {
		return errors.New("named secrets are not configured on this control plane")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		rec, ok := s.records[name]
		if c.Operator {
			if !ok {
				return fmt.Errorf("no secret named %q is set", name)
			}
			continue
		}
		if !ok || !granted(rec.Allow, c) {
			return fmt.Errorf("secret %q is not granted to %s", name, c.ID)
		}
	}
	return nil
}

// Resolve returns each name's value. It is the dispatch service's resolver: authorization has
// already happened at the API (Authorize), so it only fails for a name that isn't set.
func (s *Store) Resolve(names []string) (map[string]string, error) {
	if s == nil {
		return nil, errors.New("named secrets are not configured on this control plane")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(names))
	for _, name := range names {
		rec, ok := s.records[name]
		if !ok {
			return nil, fmt.Errorf("no secret named %q is set", name)
		}
		out[name] = rec.Value
	}
	now := time.Now().UTC()
	persist := false
	for _, name := range names {
		rec := s.records[name]
		if rec.LastUsedAt == nil || now.Sub(*rec.LastUsedAt) >= usagePersistInterval {
			persist = true
		}
		rec.LastUsedAt = &now
		s.records[name] = rec
	}
	if persist {
		_ = s.persistLocked()
	}
	return out, nil
}

func granted(allow []string, c Caller) bool {
	for _, entry := range allow {
		if strings.HasPrefix(entry, "tag:") {
			for _, tag := range c.Tags {
				if tag == entry {
					return true
				}
			}
			continue
		}
		if entry == c.ID && c.ID != "" {
			return true
		}
	}
	return false
}

func normalizeAllow(allow []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(allow))
	for _, entry := range allow {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.ContainsAny(entry, " \t\r\n,") {
			return nil, fmt.Errorf("secrets: allow entry %q must be one principal id or tag:<name>", entry)
		}
		if entry == "tag:" {
			return nil, errors.New("secrets: allow entry \"tag:\" names no tag")
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	sort.Strings(out)
	return out, nil
}

func publicView(rec record) Secret {
	pub := rec.Secret
	pub.Allow = append([]string(nil), rec.Allow...)
	return pub
}

func fingerprint(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:12]
}

func (s *Store) persistLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f := file{Version: 1, Secrets: make([]record, 0, len(s.records))}
	for _, rec := range s.records {
		f.Secrets = append(f.Secrets, rec)
	}
	sort.Slice(f.Secrets, func(i, j int) bool { return f.Secrets[i].Name < f.Secrets[j].Name })
	data, err := json.MarshalIndent(f, "", "  ")
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
