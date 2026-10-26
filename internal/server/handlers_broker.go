package server

import (
	"net/http"

	"github.com/gm2211/grove/internal/broker"
)

// BrokerEntriesResponse is GET /broker/entries: every live entry for the calling worker's broker.
type BrokerEntriesResponse struct {
	Entries []broker.Entry `json:"entries"`
}

// handleBrokerEntries serves a worker's broker its entries, real tokens included. It is the only
// route that returns a credential, so it takes a credential whose only scope is ScopeBroker, never
// the operator's: an operator token that leaks must not also leak every running job's tokens. The
// credential's device name is the worker it serves (`grove access issue --name <worker> --scope
// broker`), so one worker's broker can't read another's entries.
func (s *Server) handleBrokerEntries(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if len(p.Scopes) != 1 || p.Scopes[0] != ScopeBroker || len(p.Tags) > 0 {
		http.Error(w, "forbidden: needs a device credential with the broker scope", http.StatusForbidden)
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
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, BrokerEntriesResponse{Entries: entries})
}
