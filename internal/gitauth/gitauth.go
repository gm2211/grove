// Package gitauth holds the optional, operator-armed git credential grove uses to clone PRIVATE
// repositories for build/agent jobs.
//
// It is off by default and stays off until an operator explicitly arms it through the control
// plane (PUT /api/v1/github/sourcing, `grove github enable`). Nothing in grove reads a GitHub
// token from config.yaml or the environment: the only way a job's clone gets credentials is an
// armed Store, and disarming it (or letting its TTL lapse) wipes the token from disk.
//
// The Store is the seam internal/dispatch consumes at submit time — see dispatch.Options.RepoAuth
// — so the token is injected into the dispatched Nomad job's environment and never stored on the
// Job record, returned by the API, or handled by the submitting client.
package gitauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultHost is the git host an armed Store covers when the operator names none.
const DefaultHost = "github.com"

// maxTokenLen is a sanity bound on an accepted token — long enough for any GitHub PAT, app
// installation token or fine-grained token, short enough that a pasted file is rejected.
const maxTokenLen = 512

// usagePersistInterval throttles how often a TokenFor hit rewrites the store just to refresh
// LastUsedAt, so a busy fleet doesn't turn dispatch into a stream of fsyncs.
const usagePersistInterval = time.Minute

// Status is the JSON-safe public view of the store. It deliberately carries no token: the raw
// credential is write-only through the API, and only TokenFingerprint identifies which one is
// armed.
type Status struct {
	// Enabled is the EFFECTIVE state: an armed store whose TTL has lapsed reports false (and is
	// disarmed for real the next time the store is touched).
	Enabled bool `json:"enabled"`
	// Hosts are the git hosts this credential may be used against, lowercased.
	Hosts []string `json:"hosts,omitempty"`
	// Repos, when non-empty, narrows the credential further to these "owner/name" repositories
	// ("owner/*" matches every repo of an owner). Empty means "any repo on Hosts".
	Repos []string `json:"repos,omitempty"`
	// TokenFingerprint is a short SHA-256 prefix of the armed token, so an operator can tell which
	// credential is loaded without the API ever echoing it back.
	TokenFingerprint string     `json:"tokenFingerprint,omitempty"`
	EnabledAt        *time.Time `json:"enabledAt,omitempty"`
	// EnabledBy is the id of the operator principal that armed it.
	EnabledBy string `json:"enabledBy,omitempty"`
	// ExpiresAt is when the credential auto-disarms. Nil means "armed until explicitly disabled".
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// LastUsedAt is the last time a dispatched job's clone was given this credential.
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// EnableRequest is the wire body of PUT /api/v1/github/sourcing.
type EnableRequest struct {
	// Token is the GitHub credential (PAT, fine-grained token or app installation token) with read
	// access to the private repositories jobs should be able to clone. Required.
	Token string `json:"token"`
	// Hosts optionally overrides the default github.com — set it for GitHub Enterprise.
	Hosts []string `json:"hosts,omitempty"`
	// Repos optionally narrows the credential to specific "owner/name" (or "owner/*") repos.
	Repos []string `json:"repos,omitempty"`
	// TTL is a duration string ("2h", "30m"). Empty means the credential stays armed until it is
	// explicitly disabled.
	TTL string `json:"ttl,omitempty"`
}

// EnableOptions is the in-process form of EnableRequest, with TTL already parsed and the arming
// principal recorded.
type EnableOptions struct {
	Token string
	Hosts []string
	Repos []string
	TTL   time.Duration
	By    string
}

// Options converts a wire request into EnableOptions, parsing and validating TTL.
func (r EnableRequest) Options(by string) (EnableOptions, error) {
	opts := EnableOptions{Token: r.Token, Hosts: r.Hosts, Repos: r.Repos, By: by}
	if strings.TrimSpace(r.TTL) != "" {
		ttl, err := time.ParseDuration(strings.TrimSpace(r.TTL))
		if err != nil {
			return EnableOptions{}, fmt.Errorf("parse ttl %q: %w", r.TTL, err)
		}
		if ttl <= 0 {
			return EnableOptions{}, errors.New("ttl must be positive")
		}
		opts.TTL = ttl
	}
	return opts, nil
}

// state is what lands on disk. It is the one place the raw token lives.
type state struct {
	Version   int        `json:"version"`
	Enabled   bool       `json:"enabled"`
	Token     string     `json:"token,omitempty"`
	Hosts     []string   `json:"hosts,omitempty"`
	Repos     []string   `json:"repos,omitempty"`
	EnabledAt *time.Time `json:"enabledAt,omitempty"`
	EnabledBy string     `json:"enabledBy,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// LastUsedAt is informational; see usagePersistInterval.
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// Store persists the armed credential at path, mode 0600, through an atomic same-directory
// rename — the same handling internal/server's AccessStore gives device credentials.
//
// The zero value is not usable; build one with NewStore. A nil *Store is safe to call TokenFor
// and Status on and reports "off", so a server wired without a store simply never sources
// private repos.
type Store struct {
	mu    sync.Mutex
	path  string
	state state
}

// NewStore loads path, creating an empty (disabled) mode-0600 store when absent. Its parent
// directory is created mode 0700 when needed.
func NewStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("gitauth: store path is required")
	}
	s := &Store{path: path, state: state{Version: 1}}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("gitauth: read store: %w", err)
		}
		if err := s.persistLocked(); err != nil {
			return nil, fmt.Errorf("gitauth: create store: %w", err)
		}
		return s, nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("gitauth: secure store: %w", err)
	}
	var loaded state
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("gitauth: parse store: %w", err)
	}
	if loaded.Enabled && strings.TrimSpace(loaded.Token) == "" {
		return nil, errors.New("gitauth: store is marked enabled but holds no token")
	}
	loaded.Version = 1
	s.state = loaded
	return s, nil
}

// Enable arms the store with opts.Token, replacing whatever was armed before. An empty or
// malformed token is rejected and leaves the previous state untouched.
func (s *Store) Enable(opts EnableOptions) (Status, error) {
	if s == nil {
		return Status{}, errors.New("gitauth: private-repo sourcing is not configured on this control plane")
	}
	token, err := validateToken(opts.Token)
	if err != nil {
		return Status{}, err
	}
	hosts, err := normalizeHosts(opts.Hosts)
	if err != nil {
		return Status{}, err
	}
	repos, err := normalizeRepos(opts.Repos)
	if err != nil {
		return Status{}, err
	}
	if opts.TTL < 0 {
		return Status{}, errors.New("gitauth: ttl must be positive")
	}

	now := time.Now().UTC()
	next := state{
		Version:   1,
		Enabled:   true,
		Token:     token,
		Hosts:     hosts,
		Repos:     repos,
		EnabledAt: &now,
		EnabledBy: strings.TrimSpace(opts.By),
	}
	if opts.TTL > 0 {
		expires := now.Add(opts.TTL)
		next.ExpiresAt = &expires
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	s.state = next
	if err := s.persistLocked(); err != nil {
		s.state = previous
		return Status{}, fmt.Errorf("gitauth: persist armed credential: %w", err)
	}
	return s.statusLocked(), nil
}

// Disable disarms the store and wipes the token from disk. Disabling an already-disabled store is
// a no-op, so it is safe to call as a panic button.
func (s *Store) Disable() (Status, error) {
	if s == nil {
		return Status{}, errors.New("gitauth: private-repo sourcing is not configured on this control plane")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	s.state = state{Version: 1}
	if err := s.persistLocked(); err != nil {
		s.state = previous
		return Status{}, fmt.Errorf("gitauth: persist disarm: %w", err)
	}
	return s.statusLocked(), nil
}

// Status reports the effective public state. A store whose TTL has lapsed is disarmed for real
// (token wiped from disk) as a side effect of being read.
func (s *Store) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(time.Now().UTC())
	return s.statusLocked()
}

// TokenFor returns the credential to use for cloning repoURL, and ok=false when private-repo
// sourcing is off, its TTL has lapsed, the URL isn't an https(s) clone URL, or the repo isn't
// covered by the armed host/repo scope.
//
// Only http(s) clone URLs are ever matched: a GitHub token does nothing for an ssh:// or
// git@host: remote, so handing one out there would be a leak with no upside.
func (s *Store) TokenFor(repoURL string) (string, bool) {
	if s == nil {
		return "", false
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	if !s.state.Enabled {
		return "", false
	}
	host, repo, ok := parseHTTPSRepo(repoURL)
	if !ok || !hostAllowed(host, s.state.Hosts) || !repoAllowed(repo, s.state.Repos) {
		return "", false
	}

	// Last-use is informational, so persist it at most once a minute: a fleet dispatching
	// continuously must not turn every clone into a rewrite of the credential file.
	if s.state.LastUsedAt == nil || now.Sub(*s.state.LastUsedAt) >= usagePersistInterval {
		previous := s.state.LastUsedAt
		s.state.LastUsedAt = &now
		if err := s.persistLocked(); err != nil {
			s.state.LastUsedAt = previous
		}
	} else {
		s.state.LastUsedAt = &now
	}
	return s.state.Token, true
}

// expireLocked disarms a store past its ExpiresAt, wiping the token. A failed persist leaves the
// in-memory state disarmed anyway — refusing to hand out a lapsed credential matters more than
// the file being momentarily stale, and the next mutation rewrites it.
func (s *Store) expireLocked(now time.Time) {
	if !s.state.Enabled || s.state.ExpiresAt == nil || now.Before(*s.state.ExpiresAt) {
		return
	}
	s.state = state{Version: 1}
	_ = s.persistLocked()
}

func (s *Store) statusLocked() Status {
	st := Status{
		Enabled:    s.state.Enabled,
		Hosts:      append([]string(nil), s.state.Hosts...),
		Repos:      append([]string(nil), s.state.Repos...),
		EnabledAt:  s.state.EnabledAt,
		EnabledBy:  s.state.EnabledBy,
		ExpiresAt:  s.state.ExpiresAt,
		LastUsedAt: s.state.LastUsedAt,
	}
	if s.state.Token != "" {
		st.TokenFingerprint = Fingerprint(s.state.Token)
	}
	return st
}

func (s *Store) persistLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
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

// Fingerprint is the short, non-reversible label the API shows for an armed token.
func Fingerprint(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])[:12]
}

func validateToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("gitauth: token is required to enable private-repo sourcing")
	}
	if len(token) > maxTokenLen {
		return "", fmt.Errorf("gitauth: token is longer than %d characters — is this a file rather than a token?", maxTokenLen)
	}
	for _, r := range token {
		// A token has to survive being exported into the job's shell environment; whitespace and
		// control characters mean a paste went wrong, not a valid credential.
		if r <= ' ' || r == 0x7f {
			return "", errors.New("gitauth: token contains whitespace or control characters")
		}
	}
	return token, nil
}

func normalizeHosts(hosts []string) ([]string, error) {
	if len(hosts) == 0 {
		return []string{DefaultHost}, nil
	}
	seen := make(map[string]struct{}, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			continue
		}
		if strings.ContainsAny(host, "/:@ ") {
			return nil, fmt.Errorf("gitauth: %q is not a bare host name (no scheme, port, path or credentials)", host)
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	if len(out) == 0 {
		return []string{DefaultHost}, nil
	}
	return out, nil
}

func normalizeRepos(repos []string) ([]string, error) {
	seen := make(map[string]struct{}, len(repos))
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		repo = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(repo), ".git")))
		if repo == "" {
			continue
		}
		owner, name, found := strings.Cut(repo, "/")
		if !found || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("gitauth: %q is not an owner/name repository (use %q for every repo of an owner)", repo, "owner/*")
		}
		if _, dup := seen[repo]; dup {
			continue
		}
		seen[repo] = struct{}{}
		out = append(out, repo)
	}
	return out, nil
}

// parseHTTPSRepo pulls the host and lowercased "owner/name" out of an https(s) clone URL.
func parseHTTPSRepo(repoURL string) (host, repo string, ok bool) {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return "", "", false
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", "", false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", false
	}
	host = strings.ToLower(u.Hostname())
	if host == "" {
		return "", "", false
	}
	path := strings.Trim(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	owner, name, found := strings.Cut(path, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return host, strings.ToLower(owner + "/" + name), true
}

func hostAllowed(host string, allowed []string) bool {
	for _, candidate := range allowed {
		if candidate == host {
			return true
		}
	}
	return false
}

// repoAllowed reports whether repo ("owner/name", lowercased) is covered by the armed scope. An
// empty scope means every repo on an allowed host.
func repoAllowed(repo string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	owner, _, _ := strings.Cut(repo, "/")
	for _, candidate := range allowed {
		if candidate == repo || candidate == owner+"/*" {
			return true
		}
	}
	return false
}
