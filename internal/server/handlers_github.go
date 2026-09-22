package server

import (
	"encoding/json"
	"net/http"

	"github.com/gm2211/grove/internal/gitauth"
)

// Private-repo sourcing is operator-only and off by default: until someone with the control-plane
// operator credential PUTs a token here, grove hands no clone credentials to any job (see
// internal/gitauth and dispatch.Options.RepoAuth).
//
//	GET    /api/v1/github/sourcing  → current state, never the token
//	PUT    /api/v1/github/sourcing  → arm a credential (optionally scoped to repos, with a TTL)
//	DELETE /api/v1/github/sourcing  → disarm and wipe the stored token

func (s *Server) handleGitHubSourcing(w http.ResponseWriter, r *http.Request) {
	if s.opts.GitAuth == nil {
		http.Error(w, "private-repo sourcing is not configured on this control plane", http.StatusServiceUnavailable)
		return
	}
	s.writeJSON(w, http.StatusOK, s.opts.GitAuth.Status())
}

func (s *Server) handleEnableGitHubSourcing(w http.ResponseWriter, r *http.Request) {
	if s.opts.GitAuth == nil {
		http.Error(w, "private-repo sourcing is not configured on this control plane", http.StatusServiceUnavailable)
		return
	}
	var in gitauth.EnableRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	opts, err := in.Options(principalFromContext(r.Context()).ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	status, err := s.opts.GitAuth.Enable(opts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("private-repo sourcing enabled",
		"by", status.EnabledBy, "hosts", status.Hosts, "repos", status.Repos,
		"fingerprint", status.TokenFingerprint, "expiresAt", status.ExpiresAt)
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleDisableGitHubSourcing(w http.ResponseWriter, r *http.Request) {
	if s.opts.GitAuth == nil {
		http.Error(w, "private-repo sourcing is not configured on this control plane", http.StatusServiceUnavailable)
		return
	}
	status, err := s.opts.GitAuth.Disable()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("private-repo sourcing disabled", "by", principalFromContext(r.Context()).ID)
	s.writeJSON(w, http.StatusOK, status)
}
