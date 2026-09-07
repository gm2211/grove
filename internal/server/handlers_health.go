package server

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	result := map[string]string{}
	ok := true

	if err := s.orchard.Ping(ctx); err != nil {
		result["orchard"] = "error: " + err.Error()
		ok = false
	} else {
		result["orchard"] = "ok"
	}

	if err := s.nomad.Ping(ctx); err != nil {
		result["nomad"] = "error: " + err.Error()
		ok = false
	} else {
		result["nomad"] = "ok"
	}

	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	s.writeJSON(w, status, result)
}
