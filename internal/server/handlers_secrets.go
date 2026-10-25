package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gm2211/grove/internal/secrets"
)

// Named job secrets are operator-managed: only the operator sets, lists or deletes them, and no
// response ever carries a value (see internal/secrets).
//
//	GET    /api/v1/secrets         → every secret's name, allow list and fingerprint
//	PUT    /api/v1/secrets/{name}  → create or replace one
//	DELETE /api/v1/secrets/{name}  → delete one, wiping its value

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	if s.opts.Secrets == nil {
		http.Error(w, "named secrets are not configured on this control plane", http.StatusServiceUnavailable)
		return
	}
	s.writeJSON(w, http.StatusOK, s.opts.Secrets.List())
}

func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	if s.opts.Secrets == nil {
		http.Error(w, "named secrets are not configured on this control plane", http.StatusServiceUnavailable)
		return
	}
	var in secrets.SetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*secrets.MaxValueLen)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	by := principalFromContext(r.Context()).ID
	secret, err := s.opts.Secrets.Set(r.PathValue("name"), in.Value, in.Allow, by)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("named secret set", "name", secret.Name, "by", by, "allow", secret.Allow, "fingerprint", secret.Fingerprint)
	s.writeJSON(w, http.StatusOK, secret)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if s.opts.Secrets == nil {
		http.Error(w, "named secrets are not configured on this control plane", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if err := s.opts.Secrets.Delete(name); err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			http.Error(w, "secret not found", http.StatusNotFound)
			return
		}
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("named secret deleted", "name", name, "by", principalFromContext(r.Context()).ID)
	w.WriteHeader(http.StatusNoContent)
}
