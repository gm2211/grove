package server

import (
	"context"
	"net/http"
	"time"
)

// HealthzResponse is the body of GET /healthz.
type HealthzResponse struct {
	OK         bool   `json:"ok"`
	Version    string `json:"version"`
	Orchard    string `json:"orchard"` // "up" | "down"
	Nomad      string `json:"nomad"`   // "up" | "down"
	ServerTime string `json:"serverTime"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp := HealthzResponse{OK: true, Version: s.opts.Version, ServerTime: time.Now().UTC().Format(time.RFC3339)}

	if err := s.orchard.Ping(ctx); err != nil {
		resp.Orchard = "down"
		resp.OK = false
	} else {
		resp.Orchard = "up"
	}

	if err := s.nomad.Ping(ctx); err != nil {
		resp.Nomad = "down"
		resp.OK = false
	} else {
		resp.Nomad = "up"
	}

	status := http.StatusOK
	if !resp.OK {
		status = http.StatusServiceUnavailable
	}
	s.writeJSON(w, status, resp)
}
