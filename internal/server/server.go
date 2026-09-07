// Package server is grove's HTTP API (/api/v1/…) plus the embedded web UI (mounted at "/").
// See ARCHITECTURE.md → "grove HTTP API (v1)".
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

// Options configures the grove HTTP server.
type Options struct {
	// Token is the Bearer token clients must present under /api/v1. Empty disables auth
	// (a loud warning is logged at startup) — useful for local dev, never for a real deployment.
	Token string
	// Logger receives structured request/lifecycle logs. Defaults to slog.Default().
	Logger *slog.Logger
	// RecyclePollInterval controls how often POST /vms/{name}/recycle polls Nomad while waiting
	// for the drained node's allocations to reach zero. Defaults to 5s; tests shrink this.
	RecyclePollInterval time.Duration
	// Version is a caller-supplied build version string reported by GET /healthz. Empty is fine.
	Version string
}

// Server implements http.Handler for grove's HTTP API + embedded UI.
type Server struct {
	orchard   orchard.Client
	nomad     nomad.Client
	dispatch  dispatch.Service
	artifacts artifacts.Client
	opts      Options
	log       *slog.Logger
	handler   http.Handler

	// recycleDone, if non-nil, receives the vm name every time a background recycle finishes
	// deleting a VM. Only set by tests, to synchronize on the async recycle flow.
	recycleDone chan string

	// fleetReconcile is the published state of `grove serve`'s background fleet reconciler loop
	// (see internal/cli/serve.go). Always non-nil — a server with no fleet configured just never
	// gets SetEnabled/Report called on it, so GET /fleet/reconcile reports {enabled: false}.
	fleetReconcile *FleetReconcileStatus
}

// FleetReconcile returns the Server's fleet-reconciler status sink. `grove serve` calls
// SetEnabled/Report on it from its background reconcile loop; GET /fleet/reconcile and the
// "reconcile" field of GET /fleet read it back.
func (s *Server) FleetReconcile() *FleetReconcileStatus {
	return s.fleetReconcile
}

// New builds a Server wired to the given Orchard/Nomad clients, dispatch service and artifact
// store client. ac may be nil — GET /jobs/{id}/artifacts/{path} then returns 404, and
// dispatch.Job.Artifacts is simply never populated (see dispatch.Options.Artifacts).
func New(oc orchard.Client, nc nomad.Client, ds dispatch.Service, ac artifacts.Client, opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.Token == "" {
		log.Warn("grove server: no auth token configured — /api/v1 is UNAUTHENTICATED; set server.token in config.yaml")
	}
	if opts.RecyclePollInterval <= 0 {
		opts.RecyclePollInterval = 5 * time.Second
	}
	s := &Server{
		orchard:        oc,
		nomad:          nc,
		dispatch:       ds,
		artifacts:      ac,
		opts:           opts,
		log:            log,
		fleetReconcile: &FleetReconcileStatus{},
	}
	s.handler = s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /fleet", s.handleFleet)
	api.HandleFunc("GET /fleet/reconcile", s.handleFleetReconcile)
	api.HandleFunc("POST /vms/{name}/recycle", s.handleRecycleVM)
	api.HandleFunc("POST /workers/{name}/pause", s.handleWorkerPause)
	api.HandleFunc("POST /workers/{name}/resume", s.handleWorkerResume)
	api.HandleFunc("GET /jobs", s.handleListJobs)
	api.HandleFunc("POST /jobs", s.handleSubmitJob)
	api.HandleFunc("GET /jobs/{id}", s.handleGetJob)
	api.HandleFunc("DELETE /jobs/{id}", s.handleCancelJob)
	api.HandleFunc("POST /jobs/{id}/cancel", s.handleCancelJob)
	api.HandleFunc("GET /jobs/{id}/logs", s.handleJobLogs)
	api.HandleFunc("GET /jobs/{id}/artifacts/{path...}", s.handleGetArtifact)
	api.HandleFunc("GET /healthz", s.handleHealthz)

	root := http.NewServeMux()
	root.Handle("/api/v1/", http.StripPrefix("/api/v1", s.withMiddleware(api)))
	root.Handle("/", UIHandler())
	return root
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("write json response failed", "err", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, err error) {
	s.log.Error("request failed", "status", status, "err", err)
	http.Error(w, err.Error(), status)
}
