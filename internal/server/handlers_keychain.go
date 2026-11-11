package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/fleet"
)

// Unlocking a worker's login keychain from the control panel, with the password kept nowhere.
//
// A macOS guest boots only while its worker user's login keychain is unlocked (fleet's
// NeedsLoginSession). grove never stores that password; an operator types it into the panel:
//
//	POST /workers/{name}/unlock-keychain {"password": "..."}   operator scope
//
// The control plane holds it in memory, for that worker only, for keychainUnlockTTL. The worker's
// credential broker collects it on its next poll of GET /broker/entries (BrokerEntriesResponse's
// KeychainUnlock: only the broker credential named after that worker gets it, and the route stays
// the only one a broker credential reaches, spec S5), which removes it here, and hands it to the
// worker daemon's wrapper through a FIFO; the wrapper unlocks the keychain in the daemon's own
// security session. The password is never written to disk, logged, put in an API response other
// than that one collection, or kept after it. After a reboot the keychain is locked again and the
// Map shows the worker's macOS VMs as needs-unlock until someone unlocks it again.

// keychainUnlockTTL is how long a typed password waits for its worker's broker before it is dropped.
const keychainUnlockTTL = 10 * time.Minute

// maxKeychainPassword bounds a password: a login password, not a payload.
const maxKeychainPassword = 1024

type pendingUnlock struct {
	password string
	expires  time.Time
}

// keychainUnlocks holds at most one pending password per worker, in memory only.
type keychainUnlocks struct {
	mu  sync.Mutex
	m   map[string]pendingUnlock
	now func() time.Time
	// rebuildDelay overrides keychainRebuildDelay (tests).
	rebuildDelay *time.Duration
}

func (k *keychainUnlocks) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}

// put queues password for worker, replacing any earlier one, and says when it expires.
func (k *keychainUnlocks) put(worker, password string) time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string]pendingUnlock{}
	}
	exp := k.clock().Add(keychainUnlockTTL)
	k.m[worker] = pendingUnlock{password: password, expires: exp}
	return exp
}

// take removes and returns worker's pending password, if one is waiting and unexpired.
func (k *keychainUnlocks) take(worker string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.m[worker]
	delete(k.m, worker)
	if !ok || k.clock().After(p.expires) {
		return "", false
	}
	return p.password, true
}

// pending says whether worker has a password waiting, and until when.
func (k *keychainUnlocks) pending(worker string) (time.Time, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.m[worker]
	if !ok || k.clock().After(p.expires) {
		return time.Time{}, false
	}
	return p.expires, true
}

// KeychainUnlockRequest is the body of POST /workers/{name}/unlock-keychain.
type KeychainUnlockRequest struct {
	Password string `json:"password"`
}

// KeychainUnlockStatus is what POST and GET /workers/{name}/unlock-keychain answer. It never
// carries the password.
type KeychainUnlockStatus struct {
	Worker    string     `json:"worker"`
	Pending   bool       `json:"pending"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

func (s *Server) handleQueueKeychainUnlock(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validWorkerName(name) {
		http.Error(w, "invalid worker name", http.StatusBadRequest)
		return
	}
	var req KeychainUnlockRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*maxKeychainPassword)).Decode(&req); err != nil {
		http.Error(w, "body must be {\"password\": \"...\"}", http.StatusBadRequest)
		return
	}
	switch {
	case req.Password == "":
		http.Error(w, "password is empty", http.StatusBadRequest)
		return
	case len(req.Password) > maxKeychainPassword:
		http.Error(w, "password is too long", http.StatusBadRequest)
		return
	case strings.ContainsAny(req.Password, "\r\n\x00"):
		// The worker reads it as one line.
		http.Error(w, "password must be one line", http.StatusBadRequest)
		return
	}
	exp := s.keychainUnlocks.put(name, req.Password)
	req.Password = ""
	s.log.Info("keychain unlock queued for a worker's broker", "worker", name, "by", principalFromContext(r.Context()).Name)
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusAccepted, KeychainUnlockStatus{Worker: name, Pending: true, ExpiresAt: &exp})
}

func (s *Server) handleKeychainUnlockStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validWorkerName(name) {
		http.Error(w, "invalid worker name", http.StatusBadRequest)
		return
	}
	st := KeychainUnlockStatus{Worker: name}
	if exp, ok := s.keychainUnlocks.pending(name); ok {
		st.Pending, st.ExpiresAt = true, &exp
	}
	s.writeJSON(w, http.StatusOK, st)
}

// keychainRebuildDelay is how long after its broker collects a password the control plane waits
// before rebuilding the worker's failed macOS VMs: long enough for the broker's handoff and the
// wrapper's unlock, both of which take seconds.
const keychainRebuildDelay = 45 * time.Second

// rebuildLockedVMs deletes worker's VMs that failed for a locked keychain, so the reconciler
// recreates them now, with the keychain unlocked, instead of after fleet.LoginSessionRetry (an
// hour). A boot-looping VM needs nothing: Orchard restarts it anyway.
func (s *Server) rebuildLockedVMs(worker string) {
	delay := keychainRebuildDelay
	if d := s.keychainUnlocks.rebuildDelay; d != nil {
		delay = *d
	}
	time.Sleep(delay)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	vms, err := s.orchard.ListVMs(ctx)
	if err != nil {
		s.log.Warn("keychain unlock: listing VMs to rebuild failed", "worker", worker, "err", err)
		return
	}
	for _, vm := range vms {
		if vm.Worker != worker || vm.Status != "failed" || !fleet.NeedsLoginSession(vm.StatusMessage) {
			continue
		}
		if err := s.orchard.DeleteVM(ctx, vm.Name); err != nil {
			s.log.Warn("keychain unlock: deleting a VM that failed for the locked keychain failed", "vm", vm.Name, "err", err)
			continue
		}
		s.log.Info("keychain unlock: deleted a VM that failed for the locked keychain; the reconciler recreates it", "vm", vm.Name)
	}
}
