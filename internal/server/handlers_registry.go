package server

import (
	"encoding/json"
	"net/http"

	"github.com/gm2211/grove/internal/registryauth"
)

// Registry pull credentials are operator-only and off by default: until someone with the
// control-plane operator credential PUTs a token here, no VM is given a registry login and no
// image tag is resolved with credentials (see internal/registryauth).
//
//	GET    /api/v1/registry/pull  → current state, never the token
//	PUT    /api/v1/registry/pull  → arm a credential (registry, user, token, repos)
//	PATCH  /api/v1/registry/pull  → add or remove covered repositories (no token; fingerprint stays)
//	DELETE /api/v1/registry/pull  → disarm and wipe the stored token

const registryNotConfigured = "registry pull credentials are not configured on this control plane"

func (s *Server) handleRegistryPull(w http.ResponseWriter, r *http.Request) {
	if s.opts.RegistryAuth == nil {
		http.Error(w, registryNotConfigured, http.StatusServiceUnavailable)
		return
	}
	s.writeJSON(w, http.StatusOK, s.opts.RegistryAuth.Status())
}

func (s *Server) handleEnableRegistryPull(w http.ResponseWriter, r *http.Request) {
	if s.opts.RegistryAuth == nil {
		http.Error(w, registryNotConfigured, http.StatusServiceUnavailable)
		return
	}
	var in registryauth.EnableRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	status, err := s.opts.RegistryAuth.Enable(in.Options(principalFromContext(r.Context()).ID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("registry pull credential enabled",
		"by", status.EnabledBy, "registry", status.Registry, "user", status.User,
		"repos", status.Repos, "fingerprint", status.TokenFingerprint)
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleRegistryPullScope(w http.ResponseWriter, r *http.Request) {
	if s.opts.RegistryAuth == nil {
		http.Error(w, registryNotConfigured, http.StatusServiceUnavailable)
		return
	}
	var in registryauth.ScopeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields() // a token in this body is a mistake, not something to ignore
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	status, err := s.opts.RegistryAuth.UpdateScope(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("registry pull scope changed", "by", principalFromContext(r.Context()).ID,
		"add", in.Add, "remove", in.Remove, "repos", status.Repos, "fingerprint", status.TokenFingerprint)
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleDisableRegistryPull(w http.ResponseWriter, r *http.Request) {
	if s.opts.RegistryAuth == nil {
		http.Error(w, registryNotConfigured, http.StatusServiceUnavailable)
		return
	}
	status, err := s.opts.RegistryAuth.Disable()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("registry pull credential disabled", "by", principalFromContext(r.Context()).ID)
	s.writeJSON(w, http.StatusOK, status)
}
