package server

import (
	"fmt"
	"net/http"
	"time"
)

func (s *Server) handleWorkerPause(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Record the reason first: an Orchard pause with no reason recorded would look external, and
	// a snooze ending later must not resume it.
	if s.opts.Pauses != nil {
		if err := s.opts.Pauses.SetOperator(name, principalFromContext(r.Context()).Name, time.Now().UTC()); err != nil {
			s.writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := s.orchard.PauseWorker(r.Context(), name); err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWorkerResume ends an operator or health pause. It does not end a snooze: that belongs to
// the owner (and `grove worker wake`), and resuming under it would put the Mac back to work.
func (s *Server) handleWorkerResume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.opts.Pauses != nil {
		if e := s.opts.Pauses.Get(name); e.Active(time.Now()) {
			http.Error(w, fmt.Sprintf("worker %s is still snoozed until %s; end the snooze with `grove worker wake %s`",
				name, e.Snooze.Until.Local().Format("15:04"), name), http.StatusConflict)
			return
		}
		if err := s.opts.Pauses.ClearOperatorAndHealth(name); err != nil {
			s.writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := s.orchard.ResumeWorker(r.Context(), name); err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
