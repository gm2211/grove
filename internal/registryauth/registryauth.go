// Package registryauth holds the optional, operator-armed credential grove uses to PULL job images
// from a private container registry (GHCR, by default).
//
// Like internal/gitauth it is off until an operator arms it through the control plane
// (PUT /api/v1/registry/pull, `grove registry pull enable`): there is no config.yaml key and no
// environment variable for it. The credential has two consumers and no others:
//
//   - the fleet reconciler's Enrol, which writes it, as a Docker config.json, into a Linux VM that
//     runs an image the credential covers (over Orchard exec, on stdin), where Nomad's docker
//     driver reads it to pull the pool's runner image and its job environment images;
//   - the dispatch service, which uses it to resolve an environment image tag to its digest
//     (Resolver).
//
// It never goes into a job: not the payload, env_json, meta, the job record, an API response, a
// log, a VM spec or a command line. Status carries a fingerprint, not the token.
package registryauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/secfile"
)

// DefaultRegistry is the registry an armed Store covers when the operator names none.
const DefaultRegistry = "ghcr.io"

// maxTokenLen bounds an accepted token, as gitauth does.
const maxTokenLen = 512

// EmptyDockerConfig is the Docker config a VM gets when no credential covers its images. The file
// must exist for Nomad's docker driver once its plugin config points at it.
const EmptyDockerConfig = `{"auths":{}}`

// Status is the JSON-safe public view of the store. It carries no token.
type Status struct {
	Enabled bool `json:"enabled"`
	// Registry is the registry host the credential is used against, lowercased.
	Registry string `json:"registry,omitempty"`
	User     string `json:"user,omitempty"`
	// Repos are the "owner/name" repositories ("owner/*" for every repo of an owner) on Registry
	// the credential covers.
	Repos []string `json:"repos,omitempty"`
	// TokenFingerprint is a short SHA-256 prefix of the token, so an operator can tell which
	// credential is loaded.
	TokenFingerprint string     `json:"tokenFingerprint,omitempty"`
	EnabledAt        *time.Time `json:"enabledAt,omitempty"`
	EnabledBy        string     `json:"enabledBy,omitempty"`
}

// EnableRequest is the wire body of PUT /api/v1/registry/pull.
type EnableRequest struct {
	// Registry is the registry host, default ghcr.io.
	Registry string `json:"registry,omitempty"`
	// User is the registry user name (for GHCR, the GitHub login that owns the token). Required.
	User string `json:"user"`
	// Token is the registry credential (for GHCR a classic PAT with only read:packages). Required.
	Token string `json:"token"`
	// Repos names the repositories the credential covers. Required: "owner/name" or "owner/*".
	Repos []string `json:"repos"`
}

// EnableOptions is the in-process form of EnableRequest plus the arming principal.
type EnableOptions struct {
	Registry string
	User     string
	Token    string
	Repos    []string
	By       string
}

// Options converts a wire request into EnableOptions.
func (r EnableRequest) Options(by string) EnableOptions {
	return EnableOptions{Registry: r.Registry, User: r.User, Token: r.Token, Repos: r.Repos, By: by}
}

// Credential is a user and token to present to one registry. Its String and GoString methods
// never print the token.
type Credential struct {
	Registry string
	User     string
	Token    string
}

func (c Credential) String() string {
	return "registryauth.Credential{" + c.Registry + ", " + c.User + ", token elided}"
}
func (c Credential) GoString() string { return c.String() }

// state is what lands on disk, the one place the raw token lives.
type state struct {
	Version   int        `json:"version"`
	Enabled   bool       `json:"enabled"`
	Registry  string     `json:"registry,omitempty"`
	User      string     `json:"user,omitempty"`
	Token     string     `json:"token,omitempty"`
	Repos     []string   `json:"repos,omitempty"`
	EnabledAt *time.Time `json:"enabledAt,omitempty"`
	EnabledBy string     `json:"enabledBy,omitempty"`
}

// Store persists the armed credential at path, mode 0600, through an atomic same-directory
// rename. A nil *Store is safe to call every read method on and reports "off".
type Store struct {
	mu    sync.Mutex
	path  string
	state state
}

// NewStore loads path, creating an empty (disarmed) mode-0600 store when absent. A symlink, a
// non-regular file or a file another user owns is refused (internal/secfile).
func NewStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("registryauth: store path is required")
	}
	s := &Store{path: path, state: state{Version: 1}}
	data, err := secfile.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("registryauth: read store: %w", err)
		}
		if err := s.persistLocked(); err != nil {
			return nil, fmt.Errorf("registryauth: create store: %w", err)
		}
		return s, nil
	}
	var loaded state
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("registryauth: parse store: %w", err)
	}
	if loaded.Enabled && (strings.TrimSpace(loaded.Token) == "" || loaded.User == "" || loaded.Registry == "") {
		return nil, errors.New("registryauth: store is marked enabled but holds no complete credential")
	}
	loaded.Version = 1
	s.state = loaded
	return s, nil
}

// Enable arms the store, replacing whatever was armed. Invalid input leaves the previous state.
func (s *Store) Enable(opts EnableOptions) (Status, error) {
	if s == nil {
		return Status{}, errors.New("registryauth: registry pull credentials are not configured on this control plane")
	}
	registry, err := normalizeRegistry(opts.Registry)
	if err != nil {
		return Status{}, err
	}
	user := strings.TrimSpace(opts.User)
	if user == "" {
		return Status{}, errors.New("registryauth: user is required")
	}
	if strings.ContainsAny(user, ":\r\n\t ") || len(user) > 128 {
		return Status{}, errors.New("registryauth: user must be a plain user name (no colon or whitespace)")
	}
	token, err := validateToken(opts.Token)
	if err != nil {
		return Status{}, err
	}
	repos, err := normalizeRepos(opts.Repos)
	if err != nil {
		return Status{}, err
	}
	if len(repos) == 0 {
		return Status{}, errors.New("registryauth: name at least one repository (owner/name or owner/*)")
	}
	now := time.Now().UTC()
	next := state{
		Version: 1, Enabled: true, Registry: registry, User: user, Token: token, Repos: repos,
		EnabledAt: &now, EnabledBy: strings.TrimSpace(opts.By),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	s.state = next
	if err := s.persistLocked(); err != nil {
		s.state = previous
		return Status{}, fmt.Errorf("registryauth: persist credential: %w", err)
	}
	return s.statusLocked(), nil
}

// Disable disarms the store and wipes the token from disk. Disabling a disarmed store is a no-op.
func (s *Store) Disable() (Status, error) {
	if s == nil {
		return Status{}, errors.New("registryauth: registry pull credentials are not configured on this control plane")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	s.state = state{Version: 1}
	if err := s.persistLocked(); err != nil {
		s.state = previous
		return Status{}, fmt.Errorf("registryauth: persist disarm: %w", err)
	}
	return s.statusLocked(), nil
}

// Status reports the public state; never the token.
func (s *Store) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

// CredentialFor returns the credential for pulling image (a repository reference such as
// "ghcr.io/owner/name", with or without a tag or digest), and ok=false when the store is disarmed
// or the image's registry or repository is outside the armed scope.
func (s *Store) CredentialFor(image string) (Credential, bool) {
	if s == nil {
		return Credential{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Enabled {
		return Credential{}, false
	}
	host, path, ok := SplitImage(image)
	if !ok || host != s.state.Registry || !repoCovered(path, s.state.Repos) {
		return Credential{}, false
	}
	return Credential{Registry: s.state.Registry, User: s.state.User, Token: s.state.Token}, true
}

// PullConfig returns the Docker config.json to put on a VM that runs images (references with a
// registry host, tag and digest allowed, or prefixes ending in "/"): the armed credential when
// any of them is covered by it, else EmptyDockerConfig. fingerprint changes exactly when config
// does, so a caller can push again only on change.
func (s *Store) PullConfig(images []string) (config, fingerprint string) {
	if s == nil {
		return EmptyDockerConfig, configFingerprint(EmptyDockerConfig)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Enabled {
		return EmptyDockerConfig, configFingerprint(EmptyDockerConfig)
	}
	covered := false
	for _, image := range images {
		if s.imageInScopeLocked(image) {
			covered = true
			break
		}
	}
	if !covered {
		return EmptyDockerConfig, configFingerprint(EmptyDockerConfig)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(s.state.User + ":" + s.state.Token))
	cfg := map[string]any{"auths": map[string]any{s.state.Registry: map[string]string{"auth": auth}}}
	data, err := json.Marshal(cfg)
	if err != nil { // a map of strings cannot fail to marshal
		return EmptyDockerConfig, configFingerprint(EmptyDockerConfig)
	}
	return string(data), configFingerprint(string(data))
}

// imageInScopeLocked: image names a repository (or, ending in "/", a prefix of repositories) on
// the armed registry that the scope covers. A prefix counts when it overlaps the scope.
func (s *Store) imageInScopeLocked(image string) bool {
	if strings.HasSuffix(image, "/") {
		host, path, ok := SplitImage(image + "x")
		if !ok || host != s.state.Registry {
			return false
		}
		prefix := strings.TrimSuffix(path, "x") // "owner/" or "owner/sub/"
		for _, scope := range s.state.Repos {
			if strings.HasPrefix(scope, prefix) {
				return true
			}
			if owner, ok := strings.CutSuffix(scope, "/*"); ok && strings.HasPrefix(prefix, owner+"/") {
				return true
			}
		}
		return false
	}
	host, path, ok := SplitImage(image)
	return ok && host == s.state.Registry && repoCovered(path, s.state.Repos)
}

func (s *Store) statusLocked() Status {
	st := Status{
		Enabled: s.state.Enabled, Registry: s.state.Registry, User: s.state.User,
		Repos: append([]string(nil), s.state.Repos...), EnabledAt: s.state.EnabledAt, EnabledBy: s.state.EnabledBy,
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

func configFingerprint(config string) string {
	digest := sha256.Sum256([]byte("registry-config:" + config))
	return hex.EncodeToString(digest[:])[:16]
}

func validateToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("registryauth: token is required")
	}
	if len(token) > maxTokenLen {
		return "", fmt.Errorf("registryauth: token is longer than %d characters — is this a file rather than a token?", maxTokenLen)
	}
	for _, r := range token {
		if r <= ' ' || r == 0x7f {
			return "", errors.New("registryauth: token contains whitespace or control characters")
		}
	}
	return token, nil
}

func normalizeRegistry(registry string) (string, error) {
	registry = strings.ToLower(strings.TrimSpace(registry))
	if registry == "" {
		return DefaultRegistry, nil
	}
	if strings.ContainsAny(registry, "/@ \t\r\n") || strings.Contains(registry, "://") {
		return "", fmt.Errorf("registryauth: %q is not a bare registry host (no scheme, path or credentials)", registry)
	}
	return registry, nil
}

func normalizeRepos(repos []string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	for _, repo := range repos {
		repo = strings.ToLower(strings.TrimSpace(repo))
		if repo == "" {
			continue
		}
		owner, name, found := strings.Cut(repo, "/")
		if !found || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(repo, "@: \t") {
			return nil, fmt.Errorf("registryauth: %q is not an owner/name repository (use %q for every repo of an owner)", repo, "owner/*")
		}
		if _, dup := seen[repo]; dup {
			continue
		}
		seen[repo] = struct{}{}
		out = append(out, repo)
	}
	sort.Strings(out)
	return out, nil
}

// repoCovered: path ("owner/name", lowercase) is in scope. An empty scope covers nothing.
func repoCovered(path string, scope []string) bool {
	owner, _, _ := strings.Cut(path, "/")
	for _, candidate := range scope {
		if candidate == path || candidate == owner+"/*" {
			return true
		}
	}
	return false
}

// SplitImage splits a repository reference "<host>/<path>[:tag][@digest]" into its registry host
// and repository path, lowercased and without tag or digest. It does not guess a host: a
// reference whose first component has no ".", ":" or "localhost" is not accepted (ok=false).
func SplitImage(image string) (host, path string, ok bool) {
	image = strings.TrimSpace(image)
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	host, rest, found := strings.Cut(image, "/")
	if !found || host == "" || rest == "" {
		return "", "", false
	}
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return "", "", false
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		rest = rest[:i]
	}
	if rest == "" {
		return "", "", false
	}
	return strings.ToLower(host), strings.ToLower(rest), true
}
