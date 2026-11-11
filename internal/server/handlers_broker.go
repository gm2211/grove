package server

import (
	"net/http"

	"github.com/gm2211/grove/internal/broker"
)

// BrokerEntriesResponse is GET /broker/entries: every live entry for the calling worker's broker.
type BrokerEntriesResponse struct {
	Entries []broker.Entry `json:"entries"`
	// KeychainUnlock is the worker user's login password, when an operator typed it into the
	// panel for this worker (POST /workers/{name}/unlock-keychain) and no earlier poll took it. It
	// is handed over once and then forgotten (handlers_keychain.go).
	KeychainUnlock string `json:"keychainUnlock,omitempty"`
}

// handleBrokerEntries serves a worker's broker its entries, real tokens included. It is the only
// route that returns a credential, so it takes a credential whose only scope is ScopeBroker, never
// the operator's: an operator token that leaks must not also leak every running job's tokens.
// That holds only because the operator token can't mint such a credential either: join approval
// alone issues one (AccessStore.IssueJoinedBound; POST /access/devices refuses the scope), named
// after the worker it serves, so one worker's broker can't read another's entries, and bound to
// the machine that joined, so the token is no use from anywhere else (brokerPeerAllowed).
func (s *Server) handleBrokerEntries(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if len(p.Scopes) != 1 || p.Scopes[0] != ScopeBroker || len(p.Tags) > 0 {
		http.Error(w, "forbidden: needs a device credential with the broker scope", http.StatusForbidden)
		return
	}
	if !s.brokerPeerAllowed(r, p) {
		http.Error(w, "forbidden: this broker credential is bound to the machine that joined with it; run `grove setup` on this Mac for its own", http.StatusForbidden)
		return
	}
	entries, err := s.dispatch.BrokerEntries(r.Context(), p.Name)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	if entries == nil {
		entries = []broker.Entry{}
	}
	resp := BrokerEntriesResponse{Entries: entries}
	if pw, ok := s.keychainUnlocks.take(p.Name); ok {
		resp.KeychainUnlock = pw
		s.log.Info("keychain unlock collected by the worker's broker", "worker", p.Name)
		go s.rebuildLockedVMs(p.Name)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, resp)
}

// brokerPeerAllowed reports whether r comes from the machine p's broker credential is bound to:
// the Tailscale node that joined when Tailscale can name both, otherwise the address the join
// request came from. That covers a worker on the control plane's own Mac, whose broker reaches
// `grove serve` from this machine's own tailnet address or from loopback, the same address its
// join came from. The local socket has no peer address and so never matches a binding.
//
// A credential from before bindings existed has none and keeps working from anywhere, with one
// warning per credential: the worker gets a bound one the next time `grove setup` runs on it.
func (s *Server) brokerPeerAllowed(r *http.Request, p Principal) bool {
	if s.opts.AccessStore == nil {
		// A broker-only principal always comes from the store; without one there is nothing to
		// check it against.
		return false
	}
	bind, ok := s.opts.AccessStore.Binding(p.ID)
	if !ok {
		return false
	}
	if !bind.Bound() {
		if _, warned := s.unboundBrokerWarned.LoadOrStore(p.ID, true); !warned {
			s.log.Warn("broker: this worker's broker credential is not bound to its machine (it was issued before credentials were bound), so it works from any tailnet address; run `grove setup` on that Mac again to replace it",
				"worker", p.Name, "device", p.ID)
		}
		return true
	}
	ip := peerIP(r)
	if ip == "" {
		s.log.Warn("broker: refusing a bound broker credential presented with no peer address (local socket)", "worker", p.Name, "device", p.ID)
		return false
	}
	node := ""
	if bind.Node != "" {
		// A lookup that fails leaves the address to decide: the caller can't make tailscaled
		// fail, and the address is the one the join was verified against.
		if id, ok := s.identifyPeer(r.Context(), ip); ok {
			node = id.StableID
		}
	}
	if !bind.Matches(ip, node) {
		s.log.Warn("broker: refusing a broker credential presented from a machine it is not bound to",
			"worker", p.Name, "device", p.ID, "from", ip, "fromNode", node, "boundIP", bind.IP, "boundNode", bind.Node)
		return false
	}
	return true
}
