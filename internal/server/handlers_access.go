package server

import (
	"encoding/json"
	"net/http"
)

// IssueDeviceRequest is the body of POST /access/devices.
type IssueDeviceRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// IssueDeviceResponse is POST /access/devices's reply. Token is the raw credential and is never
// retrievable again — only its SHA-256 hash is stored (see AccessStore.Issue).
type IssueDeviceResponse struct {
	Token  string    `json:"token"`
	Device Principal `json:"device"`
}

// handleIssueDevice mints a named credential with explicit scopes, operator-only (everything
// under /access/ is — see requiredScope). It is the non-interactive path for a machine that can't
// go through `grove setup`'s join flow, e.g. an orchestrator that should dispatch with
// `read`+`dispatch` instead of holding the operator token.
func (s *Server) handleIssueDevice(w http.ResponseWriter, r *http.Request) {
	if s.opts.AccessStore == nil {
		http.Error(w, "device credentials are not configured", http.StatusServiceUnavailable)
		return
	}
	var in IssueDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	token, principal, err := s.opts.AccessStore.Issue(in.Name, in.Scopes)
	if err != nil {
		// Issue's errors are input validation (name/scopes) or a persistence failure; neither
		// carries token material.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, IssueDeviceResponse{Token: token, Device: principal})
}

func (s *Server) handleListDevices(w http.ResponseWriter, _ *http.Request) {
	if s.opts.AccessStore == nil {
		s.writeJSON(w, http.StatusOK, []DeviceCredential{})
		return
	}
	s.writeJSON(w, http.StatusOK, s.opts.AccessStore.List())
}

func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, principalFromContext(r.Context()))
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if s.opts.AccessStore == nil {
		http.Error(w, "device credentials are not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.opts.AccessStore.Revoke(r.PathValue("id")); err != nil {
		http.Error(w, "device credential not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
