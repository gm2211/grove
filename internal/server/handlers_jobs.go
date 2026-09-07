package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gm2211/grove/internal/dispatch"
)

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.dispatch.List(r.Context())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	s.writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	var req dispatch.JobRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	job, err := s.dispatch.Submit(r.Context(), req)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]string{"id": job.ID})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.dispatch.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	s.writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.dispatch.Cancel(r.Context(), id); err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query().Get("follow")
	follow := q == "1" || q == "true"

	rc, err := s.dispatch.Logs(r.Context(), id, follow)
	if err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	defer rc.Close()

	var streamErr error
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		streamErr = streamSSE(w, rc)
	} else {
		streamErr = streamChunked(w, rc)
	}
	if streamErr != nil {
		s.log.Warn("job logs stream ended with error", "id", id, "err", streamErr)
	}
}
