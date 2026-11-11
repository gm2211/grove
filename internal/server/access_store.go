package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/secfile"
)

// Scopes control what an enrolled Grove device may do through the HTTP API.
//
// ScopeDispatch covers the purpose-specific kinds (build and agent) only. Raw `shell` jobs run an
// arbitrary command with no repo checkout, so they need ScopeShell (or ScopeOperator) explicitly —
// a plain dispatch credential, e.g. an orchestrator's, can never run one.
const (
	ScopeRead     = "read"
	ScopeDispatch = "dispatch"
	ScopeBuild    = "dispatch:build"
	ScopeAgent    = "dispatch:agent"
	ScopeShell    = "dispatch:shell"
	ScopeOperator = "operator"
	// ScopeBroker lets a worker's credential broker pull its entries (GET /broker/entries), real
	// tokens included, and nothing else. It is never implied by ScopeOperator and never granted by
	// Tailscale identity: only a device credential named after the worker carries it, and only
	// join approval issues one (IssueJoinedBound), bound to the machine that joined (Binding).
	// Issue, and so POST /access/devices, refuses it: the operator token must not be able to mint
	// the credential that reads a worker's jobs' tokens.
	ScopeBroker = "broker"
	// ScopeSnoozePrefix starts the parameterized scope "worker:snooze:<worker>": its holder may
	// snooze and wake that one worker (POST/GET /workers/<worker>/snooze|wake) and nothing else.
	// It is never implied by ScopeOperator-less scopes, and never granted by Tailscale identity.
	ScopeSnoozePrefix = "worker:snooze:"
)

// SnoozeScope is the scope that lets its holder snooze and wake worker.
func SnoozeScope(worker string) string { return ScopeSnoozePrefix + worker }

// validWorkerName reports whether name can be the worker of a snooze scope: the characters an
// Orchard worker name (a hostname, possibly with dots) is made of.
func validWorkerName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// validScope reports whether scope is a fixed scope or a snooze scope for a valid worker name.
func validScope(scope string) bool {
	if _, ok := validAccessScopes[scope]; ok {
		return true
	}
	if worker, ok := strings.CutPrefix(scope, ScopeSnoozePrefix); ok {
		return validWorkerName(worker)
	}
	return false
}

var validAccessScopes = map[string]struct{}{
	ScopeRead: {}, ScopeDispatch: {}, ScopeBuild: {}, ScopeAgent: {}, ScopeShell: {}, ScopeOperator: {}, ScopeBroker: {},
}

// Principal identifies the device behind an authenticated access token.
// The raw token and its hash are never included in a Principal.
type Principal struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// Tags are the Tailscale node tags of a principal authenticated by Tailscale identity; empty
	// for a token. A named secret may be granted to a tag (see internal/secrets).
	Tags []string `json:"tags,omitempty"`
	// ExpiresAt is when the credential stops working, for one issued with an expiry; GET /whoami
	// shows it, so its holder can ask for a new one in time.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// DeviceCredential is the JSON-safe public record for an enrolled device.
// Token material is deliberately absent. The raw token is returned only once,
// by Issue, so callers can put it in the device's secure credential store.
type DeviceCredential struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// Source is "join" for a credential `grove setup` received through join approval, empty for
	// one an operator issued (POST /access/devices, `grove access issue`).
	Source     string     `json:"source,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	// ExpiresAt is when an expiring credential stops authenticating; nil never expires.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// BoundIP and BoundNode pin a join-issued broker credential to the machine that joined: the
	// address its join request came from and, when Tailscale could name it, that node's stable
	// id. GET /broker/entries refuses the credential from anywhere else. Empty on credentials
	// issued before the binding existed, which keep working from any address.
	BoundIP   string `json:"boundIp,omitempty"`
	BoundNode string `json:"boundNode,omitempty"`
}

// Binding is the machine a join-issued broker credential may be presented from.
type Binding struct {
	// IP is the peer address the join request came from.
	IP string
	// Node is that peer's Tailscale stable node id; empty when Tailscale identity is off or
	// could not place the address.
	Node string
}

// Bound reports whether b pins a credential to anything.
func (b Binding) Bound() bool { return b.IP != "" || b.Node != "" }

// Matches reports whether a peer at ip, which Tailscale names node ("" when it can't), is the
// machine b names. The node id decides when both sides have one: it is the machine's identity,
// while the same Mac may reach the control plane over its IPv4 or its IPv6 tailnet address.
// Otherwise the address must be the one that joined.
func (b Binding) Matches(ip, node string) bool {
	if !b.Bound() {
		return false
	}
	if b.Node != "" && node != "" {
		return b.Node == node
	}
	return b.IP != "" && ip != "" && b.IP == ip
}

type accessRecord struct {
	DeviceCredential
	TokenHash string `json:"tokenHash"`
}

type accessFile struct {
	Version     int            `json:"version"`
	Credentials []accessRecord `json:"credentials"`
}

// AccessStore persists per-device API credentials. It stores only SHA-256
// token hashes, never raw credentials. All state changes are serialized and
// persisted through an atomic same-directory rename.
type AccessStore struct {
	mu          sync.RWMutex
	path        string
	credentials map[string]accessRecord
}

// NewAccessStore loads path, creating an empty mode-0600 store when absent.
// Its parent directory is created mode 0700 when needed.
func NewAccessStore(path string) (*AccessStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("access store path is required")
	}

	s := &AccessStore{path: path, credentials: make(map[string]accessRecord)}
	data, err := secfile.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read access store: %w", err)
		}
		if err := s.persistLocked(); err != nil {
			return nil, fmt.Errorf("create access store: %w", err)
		}
		return s, nil
	}

	var file accessFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse access store: %w", err)
	}
	for _, record := range file.Credentials {
		if err := validateRecord(record); err != nil {
			return nil, fmt.Errorf("invalid access store record: %w", err)
		}
		if _, exists := s.credentials[record.ID]; exists {
			return nil, fmt.Errorf("invalid access store record: duplicate id %q", record.ID)
		}
		record.Scopes = cloneStrings(record.Scopes)
		s.credentials[record.ID] = record
	}
	return s, nil
}

// SourceJoin marks credentials issued through join approval (see DeviceCredential.Source).
const SourceJoin = "join"

// ErrBrokerScopeNotIssuable is what Issue answers for the broker scope.
var ErrBrokerScopeNotIssuable = errors.New("the broker scope is issued only by join approval, bound to the joining worker: run `grove setup` on that Mac (`grove install --role worker` on the control plane's own) and approve it with `grove join approve`")

// Issue creates a credential and returns its raw token exactly once. It refuses the broker
// scope (see ScopeBroker).
func (s *AccessStore) Issue(name string, scopes []string) (string, Principal, error) {
	if hasBrokerScope(scopes) {
		return "", Principal{}, ErrBrokerScopeNotIssuable
	}
	return s.issue(name, scopes, "", 0, Binding{})
}

func hasBrokerScope(scopes []string) bool {
	for _, scope := range scopes {
		if strings.TrimSpace(scope) == ScopeBroker {
			return true
		}
	}
	return false
}

// IssueExpiring is Issue for a credential that stops working after ttl.
func (s *AccessStore) IssueExpiring(name string, scopes []string, ttl time.Duration) (string, Principal, error) {
	if ttl <= 0 {
		return "", Principal{}, errors.New("expiry must be in the future")
	}
	if hasBrokerScope(scopes) {
		return "", Principal{}, ErrBrokerScopeNotIssuable
	}
	return s.issue(name, scopes, "", ttl, Binding{})
}

// IssueJoined is Issue for a device enrolling through join approval; see RevokeSuperseded. Like
// Issue it refuses the broker scope, which needs a binding (IssueJoinedBound).
func (s *AccessStore) IssueJoined(name string, scopes []string) (string, Principal, error) {
	if hasBrokerScope(scopes) {
		return "", Principal{}, ErrBrokerScopeNotIssuable
	}
	return s.issue(name, scopes, SourceJoin, 0, Binding{})
}

// IssueJoinedBound is IssueJoined for a credential that may only be presented by the machine
// that joined (see Binding). It is the only way a broker credential is issued, and join approval
// its only caller.
func (s *AccessStore) IssueJoinedBound(name string, scopes []string, bind Binding) (string, Principal, error) {
	if bind.IP == "" {
		return "", Principal{}, errors.New("a bound credential needs the joining machine's address")
	}
	return s.issue(name, scopes, SourceJoin, 0, bind)
}

// Binding returns the machine credential id is bound to. ok is false for an unknown id; a
// credential from before bindings existed answers ok with an unbound Binding.
func (s *AccessStore) Binding(id string) (Binding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.credentials[id]
	if !ok {
		return Binding{}, false
	}
	return Binding{IP: record.BoundIP, Node: record.BoundNode}, true
}

// BrokerBindings returns the binding of every live (not revoked, not expired) broker credential
// named name; an unbound Binding stands for a credential from before bindings existed.
func (s *AccessStore) BrokerBindings(name string) []Binding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []Binding
	for _, record := range s.credentials {
		if record.Name != name || record.RevokedAt != nil || !slices.Contains(record.Scopes, ScopeBroker) {
			continue
		}
		if record.ExpiresAt != nil && !now.Before(*record.ExpiresAt) {
			continue
		}
		out = append(out, Binding{IP: record.BoundIP, Node: record.BoundNode})
	}
	return out
}

func (s *AccessStore) issue(name string, scopes []string, source string, ttl time.Duration, bind Binding) (string, Principal, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Principal{}, errors.New("device name is required")
	}
	normalized, err := normalizeScopes(scopes)
	if err != nil {
		return "", Principal{}, err
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", Principal{}, fmt.Errorf("generate access token: %w", err)
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", Principal{}, fmt.Errorf("generate credential id: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	record := accessRecord{
		DeviceCredential: DeviceCredential{
			ID:        hex.EncodeToString(idBytes),
			Name:      name,
			Scopes:    normalized,
			Source:    source,
			CreatedAt: time.Now().UTC(),
			BoundIP:   bind.IP,
			BoundNode: bind.Node,
		},
		TokenHash: hashToken(token),
	}
	if ttl > 0 {
		expires := record.CreatedAt.Add(ttl)
		record.ExpiresAt = &expires
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.credentials[record.ID] = record
	if err := s.persistLocked(); err != nil {
		delete(s.credentials, record.ID)
		return "", Principal{}, fmt.Errorf("persist access credential: %w", err)
	}
	return token, principalFor(record), nil
}

// Authenticate checks token and records its last-use time. Raw token material
// is never retained after this call.
func (s *AccessStore) Authenticate(token string) (Principal, bool) {
	if token == "" {
		return Principal{}, false
	}
	digest := hashToken(token)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for id, record := range s.credentials {
		if record.RevokedAt != nil || !constantTimeHexEqual(digest, record.TokenHash) {
			continue
		}
		if record.ExpiresAt != nil && !now.Before(*record.ExpiresAt) {
			return Principal{}, false
		}
		persistUsage := record.LastUsedAt == nil || now.Sub(*record.LastUsedAt) >= time.Minute
		record.LastUsedAt = &now
		s.credentials[id] = record
		// Last-use is informational. Persist at most once per minute per device so browser polling
		// cannot turn authentication into continuous temp-file writes and fsyncs.
		if persistUsage {
			_ = s.persistLocked()
		}
		return principalFor(record), true
	}
	return Principal{}, false
}

// List returns JSON-safe credential records, sorted by creation time then ID.
func (s *AccessStore) List() []DeviceCredential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]DeviceCredential, 0, len(s.credentials))
	for _, record := range s.credentials {
		credential := record.DeviceCredential
		credential.Scopes = cloneStrings(credential.Scopes)
		result = append(result, credential)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}

// Revoke permanently disables a device credential.
func (s *AccessStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.credentials[id]
	if !ok {
		return fmt.Errorf("device credential %q not found", id)
	}
	if record.RevokedAt != nil {
		return nil
	}
	original := record
	now := time.Now().UTC()
	record.RevokedAt = &now
	s.credentials[id] = record
	if err := s.persistLocked(); err != nil {
		s.credentials[id] = original
		return fmt.Errorf("persist credential revocation: %w", err)
	}
	return nil
}

// RevokeSuperseded revokes every other active join credential named name, once the device holding
// the keep credentials (its client and broker credentials) has finished enrolling. Re-running
// `grove setup` on a Mac replaces the tokens in its Keychain, so the earlier credentials are no
// longer held by anything legitimate. Credentials made
// before Source existed count as join credentials when their scopes are ones join hands out (read
// and dispatch kinds, never shell or operator), and so does a broker-only credential an operator
// minted when POST /access/devices still allowed that: nothing issues those any more, and the
// worker now holds a bound one. Other operator-issued credentials are never touched.
// It returns the IDs it revoked.
func (s *AccessStore) RevokeSuperseded(name string, keep ...string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	var revoked []string
	originals := map[string]accessRecord{}
	for id, record := range s.credentials {
		if slices.Contains(keep, id) || record.Name != name || record.RevokedAt != nil || !supersededByJoin(record) {
			continue
		}
		originals[id] = record
		record.RevokedAt = &now
		s.credentials[id] = record
		revoked = append(revoked, id)
	}
	if len(revoked) == 0 {
		return nil, nil
	}
	if err := s.persistLocked(); err != nil {
		for id, record := range originals {
			s.credentials[id] = record
		}
		return nil, fmt.Errorf("persist credential revocation: %w", err)
	}
	sort.Strings(revoked)
	return revoked, nil
}

// RevokeSupersededBroker revokes every other live broker-only credential named name, once a
// broker-only join has delivered keep. It leaves the worker's dispatch credential alone.
func (s *AccessStore) RevokeSupersededBroker(name string, keep ...string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	var revoked []string
	originals := map[string]accessRecord{}
	for id, record := range s.credentials {
		if slices.Contains(keep, id) || record.Name != name || record.RevokedAt != nil ||
			len(record.Scopes) != 1 || record.Scopes[0] != ScopeBroker {
			continue
		}
		originals[id] = record
		record.RevokedAt = &now
		s.credentials[id] = record
		revoked = append(revoked, id)
	}
	if len(revoked) == 0 {
		return nil, nil
	}
	if err := s.persistLocked(); err != nil {
		for id, record := range originals {
			s.credentials[id] = record
		}
		return nil, fmt.Errorf("persist credential revocation: %w", err)
	}
	sort.Strings(revoked)
	return revoked, nil
}

func supersededByJoin(record accessRecord) bool {
	switch record.Source {
	case SourceJoin:
		return true
	case "":
		if len(record.Scopes) == 1 && record.Scopes[0] == ScopeBroker {
			return true
		}
		for _, scope := range record.Scopes {
			switch scope {
			case ScopeRead, ScopeDispatch, ScopeBuild, ScopeAgent:
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (s *AccessStore) persistLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	records := make([]accessRecord, 0, len(s.credentials))
	for _, record := range s.credentials {
		record.Scopes = cloneStrings(record.Scopes)
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	data, err := json.MarshalIndent(accessFile{Version: 1, Credentials: records}, "", "  ")
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

func validateRecord(record accessRecord) error {
	if record.ID == "" || record.Name == "" {
		return errors.New("credential id and name are required")
	}
	if len(record.TokenHash) != sha256.Size*2 {
		return fmt.Errorf("credential %q has invalid token hash", record.ID)
	}
	if _, err := hex.DecodeString(record.TokenHash); err != nil {
		return fmt.Errorf("credential %q has invalid token hash: %w", record.ID, err)
	}
	if record.CreatedAt.IsZero() {
		return fmt.Errorf("credential %q has no creation time", record.ID)
	}
	_, err := normalizeScopes(record.Scopes)
	return err
}

func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, errors.New("at least one access scope is required")
	}
	seen := make(map[string]struct{}, len(scopes))
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if !validScope(scope) {
			return nil, fmt.Errorf("unknown access scope %q", scope)
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		result = append(result, scope)
	}
	sort.Strings(result)
	return result, nil
}

func principalFor(record accessRecord) Principal {
	return Principal{ID: record.ID, Name: record.Name, Scopes: cloneStrings(record.Scopes), ExpiresAt: record.ExpiresAt}
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func constantTimeHexEqual(left, right string) bool {
	leftBytes, errLeft := hex.DecodeString(left)
	rightBytes, errRight := hex.DecodeString(right)
	if errLeft != nil || errRight != nil || len(leftBytes) != len(rightBytes) {
		return false
	}
	return subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1
}
