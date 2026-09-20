package server

import (
	"net/http"
)

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
