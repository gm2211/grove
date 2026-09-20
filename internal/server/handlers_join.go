package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const joinTTL = 10 * time.Minute
const maxPendingJoins = 256

type joinRequest struct {
	ID, Secret, Code, Name, TailnetIP string
	ExpiresAt                         time.Time
	ApprovedAt                        time.Time
	Approved                          bool
	Approving                         bool
	BootstrapToken                    string
	ClientToken, DeviceID             string
}

type joinStore struct {
	mu   sync.Mutex
	byID map[string]*joinRequest
}

func newJoinStore() *joinStore { return &joinStore{byID: map[string]*joinRequest{}} }

func randomJoinString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Server) handleCreateJoinRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string `json:"name"`
		TailnetIP string `json:"tailnetIp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" {
		http.Error(w, "worker name is required", http.StatusBadRequest)
		return
	}
	if net.ParseIP(in.TailnetIP) == nil {
		http.Error(w, "valid tailnet IP is required", http.StatusBadRequest)
		return
	}
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if remoteIP == "" {
		remoteIP = r.RemoteAddr
	}
	if remoteIP != in.TailnetIP {
		http.Error(w, "submitted tailnet IP does not match connection source", http.StatusForbidden)
		return
	}
	id, err := randomJoinString(24)
	if err != nil {
		s.writeError(w, 500, err)
		return
	}
	secret, err := randomJoinString(32)
	if err != nil {
		s.writeError(w, 500, err)
		return
	}
	codeRaw, err := randomJoinString(6)
	if err != nil {
		s.writeError(w, 500, err)
		return
	}
	code := strings.ToUpper(codeRaw[:8])
	jr := &joinRequest{ID: id, Secret: secret, Code: code, Name: strings.TrimSpace(in.Name), TailnetIP: in.TailnetIP, ExpiresAt: time.Now().Add(joinTTL)}
	s.joins.mu.Lock()
	for existingID, existing := range s.joins.byID {
		if time.Now().After(existing.ExpiresAt) {
			delete(s.joins.byID, existingID)
		}
	}
	if len(s.joins.byID) >= maxPendingJoins {
		s.joins.mu.Unlock()
		http.Error(w, "too many pending join requests", http.StatusTooManyRequests)
		return
	}
	s.joins.byID[id] = jr
	s.joins.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": id, "secret": secret, "code": code, "expiresAt": jr.ExpiresAt.UTC()})
}

func (s *Server) handlePollJoinRequest(w http.ResponseWriter, r *http.Request) {
	id, secret := r.PathValue("id"), r.Header.Get("X-Grove-Join-Secret")
	s.joins.mu.Lock()
	defer s.joins.mu.Unlock()
	jr := s.joins.byID[id]
	if jr == nil || subtleEqual(jr.Secret, secret) == false {
		http.Error(w, "join request not found", http.StatusNotFound)
		return
	}
	if time.Now().After(jr.ExpiresAt) {
		delete(s.joins.byID, id)
		http.Error(w, "join request expired", http.StatusGone)
		return
	}
	if !jr.Approved {
		s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	}
	if jr.BootstrapToken != "" && r.Header.Get("X-Grove-Join-Phase") == "verify" {
		name := jr.Name
		s.joins.mu.Unlock()
		workers, err := s.orchard.ListWorkers(r.Context())
		s.joins.mu.Lock()
		if err != nil {
			s.writeError(w, http.StatusBadGateway, errors.New("check worker registration"))
			return
		}
		online := false
		for _, worker := range workers {
			if worker.Name == name && !worker.Offline && worker.LastSeen.After(jr.ApprovedAt) {
				online = true
				break
			}
		}
		if online {
			delete(s.joins.byID, id)
			s.writeJSON(w, http.StatusOK, map[string]any{"status": "online", "workerOnline": true})
			return
		}
		// Delivery is idempotent until Orchard reports this worker online. A dropped HTTP
		// response therefore cannot strand setup with an unrecoverable credential.
		s.writeJSON(w, http.StatusOK, map[string]any{
			"status": "approved", "controllerUrl": s.opts.ControllerURL, "bootstrapToken": jr.BootstrapToken, "serverUrl": s.opts.ServerURL,
			"clientToken": jr.ClientToken, "deviceId": jr.DeviceID,
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"status": "approved", "controllerUrl": s.opts.ControllerURL, "bootstrapToken": jr.BootstrapToken, "serverUrl": s.opts.ServerURL,
		"clientToken": jr.ClientToken, "deviceId": jr.DeviceID,
	})
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (s *Server) handleApproveJoinRequest(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(r.PathValue("code"))
	s.joins.mu.Lock()
	var jr *joinRequest
	for _, candidate := range s.joins.byID {
		if candidate.Code == code && time.Now().Before(candidate.ExpiresAt) {
			jr = candidate
			break
		}
	}
	s.joins.mu.Unlock()
	if jr == nil {
		http.Error(w, "join request not found or expired", http.StatusNotFound)
		return
	}
	if s.opts.IssueWorkerBootstrap == nil {
		http.Error(w, "control plane is missing worker enrollment credentials; rerun `grove install --role control-plane`", http.StatusServiceUnavailable)
		return
	}
	if s.opts.AccessStore == nil {
		http.Error(w, "control plane is missing device credential storage; rerun `grove install --role control-plane`", http.StatusServiceUnavailable)
		return
	}
	s.joins.mu.Lock()
	if jr.Approved {
		name := jr.Name
		s.joins.mu.Unlock()
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "approved", "name": name})
		return
	}
	if jr.Approving {
		s.joins.mu.Unlock()
		http.Error(w, "join approval already in progress", http.StatusConflict)
		return
	}
	jr.Approving = true
	s.joins.mu.Unlock()
	token, err := s.opts.IssueWorkerBootstrap(r.Context(), jr.Name)
	if err != nil {
		s.joins.mu.Lock()
		jr.Approving = false
		s.joins.mu.Unlock()
		s.writeError(w, http.StatusBadGateway, errors.New("issue worker credential"))
		return
	}
	clientToken, principal, err := s.opts.AccessStore.Issue(jr.Name, []string{ScopeRead, ScopeBuild, ScopeAgent})
	if err != nil {
		s.joins.mu.Lock()
		jr.Approving = false
		s.joins.mu.Unlock()
		s.writeError(w, http.StatusInternalServerError, errors.New("issue device credential"))
		return
	}
	s.joins.mu.Lock()
	if time.Now().After(jr.ExpiresAt) {
		jr.Approving = false
		delete(s.joins.byID, jr.ID)
		s.joins.mu.Unlock()
		_ = s.opts.AccessStore.Revoke(principal.ID)
		http.Error(w, "join request expired during approval", http.StatusGone)
		return
	}
	jr.BootstrapToken = token
	jr.ClientToken = clientToken
	jr.DeviceID = principal.ID
	jr.Approving = false
	jr.Approved = true
	jr.ApprovedAt = time.Now()
	jr.ExpiresAt = time.Now().Add(30 * time.Minute)
	s.joins.mu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "approved", "name": jr.Name})
}
