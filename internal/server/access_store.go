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
	"sort"
	"strings"
	"sync"
	"time"
)

// Scopes control what an enrolled Grove device may do through the HTTP API.
const (
	ScopeRead     = "read"
	ScopeDispatch = "dispatch"
	ScopeOperator = "operator"
)

var validAccessScopes = map[string]struct{}{
	ScopeRead: {}, ScopeDispatch: {}, ScopeOperator: {},
}

// Principal identifies the device behind an authenticated access token.
// The raw token and its hash are never included in a Principal.
type Principal struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// DeviceCredential is the JSON-safe public record for an enrolled device.
// Token material is deliberately absent. The raw token is returned only once,
// by Issue, so callers can put it in the device's secure credential store.
type DeviceCredential struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
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
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read access store: %w", err)
		}
		if err := s.persistLocked(); err != nil {
			return nil, fmt.Errorf("create access store: %w", err)
		}
		return s, nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure access store: %w", err)
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

// Issue creates a credential and returns its raw token exactly once.
func (s *AccessStore) Issue(name string, scopes []string) (string, Principal, error) {
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
			CreatedAt: time.Now().UTC(),
		},
		TokenHash: hashToken(token),
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
	for id, record := range s.credentials {
		if record.RevokedAt != nil || !constantTimeHexEqual(digest, record.TokenHash) {
			continue
		}
		now := time.Now().UTC()
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
		if _, ok := validAccessScopes[scope]; !ok {
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
	return Principal{ID: record.ID, Name: record.Name, Scopes: cloneStrings(record.Scopes)}
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
