package server

import "net/http"

func (s *Server) handleWorkerPause(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.orchard.PauseWorker(r.Context(), name); err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWorkerResume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.orchard.ResumeWorker(r.Context(), name); err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
