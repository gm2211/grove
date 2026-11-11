package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
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
	// BrokerToken is the worker's credential-broker credential: only ScopeBroker, named after
	// the worker, so its `grove broker` can pull the entries of jobs placed on it. It is bound to
	// TailnetIP, the address this request came from, and to that address's Tailscale node.
	BrokerToken, BrokerDeviceID string
	// BrokerOnly asks for the broker credential alone: no Orchard bootstrap token, no dispatch
	// credential. The control plane's own worker joins this way (`grove install --role worker`
	// there), since it registers with Orchard without a join.
	BrokerOnly bool
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
		Name       string `json:"name"`
		TailnetIP  string `json:"tailnetIp"`
		BrokerOnly bool   `json:"brokerOnly"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" {
		http.Error(w, "worker name is required", http.StatusBadRequest)
		return
	}
	// The name becomes the name of a broker credential and of a worker:snooze:<name> scope.
	if !validWorkerName(strings.TrimSpace(in.Name)) {
		http.Error(w, "worker name may hold only letters, digits, '-', '.' and '_' (128 at most)", http.StatusBadRequest)
		return
	}
	claimed, err := netip.ParseAddr(in.TailnetIP)
	if err != nil {
		http.Error(w, "valid tailnet IP is required", http.StatusBadRequest)
		return
	}
	// The verified source is what the broker credential gets bound to, so it is kept in the one
	// canonical form peerIP gives every later request.
	source := peerIP(r)
	if source == "" || source != claimed.Unmap().String() {
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
	jr := &joinRequest{ID: id, Secret: secret, Code: code, Name: strings.TrimSpace(in.Name), TailnetIP: source, BrokerOnly: in.BrokerOnly, ExpiresAt: time.Now().Add(joinTTL)}
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
	out := map[string]any{"id": id, "secret": secret, "code": code, "expiresAt": jr.ExpiresAt.UTC()}
	if jr.BrokerOnly {
		// Echoed so the asker can tell a control plane that understood it from an older one that
		// would run a full join.
		out["brokerOnly"] = true
	}
	s.writeJSON(w, http.StatusCreated, out)
}

// joinReplaces reports whether approving a join named name from the machine at ip (Tailscale
// node id node, "" when unknown) would hand that machine the place of a worker that already
// exists: Orchard lists a worker by that name, or a live broker credential carries it, and none
// of those credentials is bound to this machine. Running `grove setup` again on the same Mac is
// not that: its credential is bound to the address (and node) asking. A worker that enrolled
// before credentials were bound has nothing to compare with, so it counts until it has joined
// once more. When Orchard can't be listed the answer is yes: an approval must not go ahead on
// the guess that the name is free.
func (s *Server) joinReplaces(ctx context.Context, name, ip, node string) (bool, error) {
	var bindings []Binding
	if s.opts.AccessStore != nil {
		bindings = s.opts.AccessStore.BrokerBindings(name)
	}
	for _, b := range bindings {
		if b.Matches(ip, node) {
			return false, nil
		}
	}
	if len(bindings) > 0 {
		return true, nil
	}
	if s.orchard == nil {
		return false, nil
	}
	workers, err := s.orchard.ListWorkers(ctx)
	if err != nil {
		return true, err
	}
	for _, worker := range workers {
		if worker.Name == name {
			return true, nil
		}
	}
	return false, nil
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
	if jr.BrokerOnly {
		if r.Header.Get("X-Grove-Join-Phase") == "verify" {
			// The asker has the credential: forget the request, and retire the broker credentials
			// this one replaces (it asked because it holds none of them).
			delete(s.joins.byID, id)
			if revoked, err := s.opts.AccessStore.RevokeSupersededBroker(jr.Name, jr.BrokerDeviceID); err != nil {
				slog.Warn("join: could not revoke superseded broker credentials", "name", jr.Name, "err", err)
			} else if len(revoked) > 0 {
				slog.Info("join: revoked superseded broker credentials", "name", jr.Name, "count", len(revoked))
			}
			s.writeJSON(w, http.StatusOK, map[string]any{"status": "done"})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"status": "approved", "brokerOnly": true, "serverUrl": s.opts.ServerURL,
			"brokerToken": jr.BrokerToken, "brokerDeviceId": jr.BrokerDeviceID,
		})
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
			// Enrollment finished with the new credential, so this Mac's earlier ones (from
			// previous `grove setup` runs) are dead weight that still authenticates.
			if revoked, err := s.opts.AccessStore.RevokeSuperseded(jr.Name, jr.DeviceID, jr.BrokerDeviceID); err != nil {
				slog.Warn("join: could not revoke superseded device credentials", "name", jr.Name, "err", err)
			} else if len(revoked) > 0 {
				slog.Info("join: revoked superseded device credentials", "name", jr.Name, "count", len(revoked))
			}
			s.writeJSON(w, http.StatusOK, map[string]any{"status": "online", "workerOnline": true})
			return
		}
		// Delivery is idempotent until Orchard reports this worker online. A dropped HTTP
		// response therefore cannot strand setup with an unrecoverable credential.
		s.writeJSON(w, http.StatusOK, map[string]any{
			"status": "approved", "controllerUrl": s.opts.ControllerURL, "bootstrapToken": jr.BootstrapToken, "serverUrl": s.opts.ServerURL,
			"clientToken": jr.ClientToken, "deviceId": jr.DeviceID,
			"brokerToken": jr.BrokerToken, "brokerDeviceId": jr.BrokerDeviceID,
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"status": "approved", "controllerUrl": s.opts.ControllerURL, "bootstrapToken": jr.BootstrapToken, "serverUrl": s.opts.ServerURL,
		"clientToken": jr.ClientToken, "deviceId": jr.DeviceID,
		"brokerToken": jr.BrokerToken, "brokerDeviceId": jr.BrokerDeviceID,
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
	s.joins.mu.Lock()
	name, source, brokerOnly := jr.Name, jr.TailnetIP, jr.BrokerOnly
	s.joins.mu.Unlock()
	if s.opts.IssueWorkerBootstrap == nil && !brokerOnly {
		http.Error(w, "control plane is missing worker enrollment credentials; rerun `grove install --role control-plane`", http.StatusServiceUnavailable)
		return
	}
	if s.opts.AccessStore == nil {
		http.Error(w, "control plane is missing device credential storage; rerun `grove install --role control-plane`", http.StatusServiceUnavailable)
		return
	}
	// The broker credential is bound to the machine that asked: the address its request came
	// from (checked against the connection then) and the Tailscale node at that address now.
	bind := Binding{IP: source}
	nodeName := ""
	if id, ok := s.identifyPeer(r.Context(), source); ok {
		bind.Node, nodeName = id.StableID, id.Node
	}
	if replaces, err := s.joinReplaces(r.Context(), name, bind.IP, bind.Node); replaces && r.URL.Query().Get("replace") != "true" {
		from := source
		if nodeName != "" {
			from += " (" + nodeName + ")"
		}
		if err != nil {
			s.log.Warn("join: cannot list Orchard workers to check the name is free; refusing without replace", "name", name, "err", err)
			http.Error(w, fmt.Sprintf("cannot check whether worker %q already exists (Orchard is unreachable). Retry, or approve with `grove join approve %s --replace` if you are sure the request from %s is that Mac", name, code, from), http.StatusBadGateway)
			return
		}
		s.log.Warn("join: refusing to approve a request for an existing worker's name from a machine its broker credential is not bound to", "name", name, "from", from)
		http.Error(w, fmt.Sprintf("worker %q already exists and its broker credential is not bound to the machine asking, %s. Approving hands that machine the worker's name and its jobs' credentials. If it is the same Mac (it enrolled before credentials were bound, or its tailnet address changed) or its replacement, approve with `grove join approve %s --replace`", name, from, code), http.StatusConflict)
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
	var token, clientToken string
	var principal Principal
	if !brokerOnly {
		var err error
		token, err = s.opts.IssueWorkerBootstrap(r.Context(), jr.Name)
		if err != nil {
			s.joins.mu.Lock()
			jr.Approving = false
			s.joins.mu.Unlock()
			s.writeError(w, http.StatusBadGateway, errors.New("issue worker credential"))
			return
		}
		clientToken, principal, err = s.opts.AccessStore.IssueJoined(jr.Name, []string{ScopeRead, ScopeBuild, ScopeAgent, SnoozeScope(jr.Name)})
		if err != nil {
			s.joins.mu.Lock()
			jr.Approving = false
			s.joins.mu.Unlock()
			s.writeError(w, http.StatusInternalServerError, errors.New("issue device credential"))
			return
		}
	}
	revokeClient := func() {
		if principal.ID != "" {
			_ = s.opts.AccessStore.Revoke(principal.ID)
		}
	}
	brokerToken, brokerPrincipal, err := s.opts.AccessStore.IssueJoinedBound(jr.Name, []string{ScopeBroker}, bind)
	if err != nil {
		revokeClient()
		s.joins.mu.Lock()
		jr.Approving = false
		s.joins.mu.Unlock()
		s.writeError(w, http.StatusInternalServerError, errors.New("issue broker credential"))
		return
	}
	s.joins.mu.Lock()
	if time.Now().After(jr.ExpiresAt) {
		jr.Approving = false
		delete(s.joins.byID, jr.ID)
		s.joins.mu.Unlock()
		revokeClient()
		_ = s.opts.AccessStore.Revoke(brokerPrincipal.ID)
		http.Error(w, "join request expired during approval", http.StatusGone)
		return
	}
	jr.BootstrapToken = token
	jr.ClientToken = clientToken
	jr.DeviceID = principal.ID
	jr.BrokerToken = brokerToken
	jr.BrokerDeviceID = brokerPrincipal.ID
	jr.Approving = false
	jr.Approved = true
	jr.ApprovedAt = time.Now()
	jr.ExpiresAt = time.Now().Add(30 * time.Minute)
	s.joins.mu.Unlock()
	s.log.Info("join: approved; broker credential bound to the joining machine", "name", name, "ip", bind.IP, "node", bind.Node, "nodeName", nodeName, "brokerOnly", brokerOnly, "by", principalFromContext(r.Context()).ID)
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "approved", "name": jr.Name})
}

// pendingJoin is what an operator needs to recognise a Mac that is waiting to be let in, and the
// code that lets them do it. It deliberately never carries the request's ID or secret: those two
// are the worker's own proof of identity when it polls, and an operator never needs them.
type pendingJoin struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// TailnetIP is the address the request came from (checked against the connection, never just
	// claimed), and Node the MagicDNS name Tailscale gives the machine at that address, when it
	// can: what tells an operator which machine is really asking, whatever name it sent.
	TailnetIP string    `json:"tailnetIp"`
	Node      string    `json:"node,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	// BrokerOnly marks a request for a broker credential alone (the control plane's own worker).
	BrokerOnly bool `json:"brokerOnly,omitempty"`
	// ReplacesWorker says a worker by this name already exists and its broker credential is not
	// bound to the machine asking, so approving needs `grove join approve CODE --replace`.
	ReplacesWorker bool `json:"replacesWorker,omitempty"`
}

// handleListPendingJoins lists the Macs currently waiting for approval, oldest first, so an
// operator can approve one from the UI instead of reading the code off the joining Mac's terminal
// and retyping it into `grove join approve`. Operator scope only (see requiredScope): holding a
// code is equivalent to being able to approve it.
func (s *Server) handleListPendingJoins(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	out := []pendingJoin{}
	s.joins.mu.Lock()
	for id, jr := range s.joins.byID {
		if now.After(jr.ExpiresAt) {
			delete(s.joins.byID, id)
			continue
		}
		if jr.Approved || jr.Approving {
			continue
		}
		out = append(out, pendingJoin{Code: jr.Code, Name: jr.Name, TailnetIP: jr.TailnetIP, ExpiresAt: jr.ExpiresAt.UTC(), BrokerOnly: jr.BrokerOnly})
	}
	s.joins.mu.Unlock()
	for i := range out {
		node := ""
		if id, ok := s.identifyPeer(r.Context(), out[i].TailnetIP); ok {
			out[i].Node, node = id.Node, id.StableID
		}
		// An Orchard that can't be listed also needs --replace at approval; say so here too.
		out[i].ReplacesWorker, _ = s.joinReplaces(r.Context(), out[i].Name, out[i].TailnetIP, node)
	}
	// Map iteration is random, so sort: every pending request has the same TTL, which makes
	// expiry order arrival order, and a list that reorders itself under a 5s poll is unusable.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpiresAt.Equal(out[j].ExpiresAt) {
			return out[i].Code < out[j].Code
		}
		return out[i].ExpiresAt.Before(out[j].ExpiresAt)
	})
	s.writeJSON(w, http.StatusOK, out)
}
