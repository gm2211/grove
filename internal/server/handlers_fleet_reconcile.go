package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/fleet"
)

// FleetReconcileStatus is a thread-safe snapshot of `grove serve`'s background fleet reconciler
// loop (see internal/cli/serve.go), published by that loop and read by GET /fleet/reconcile (and
// folded into GET /fleet's own response). A Server always owns one (see New) — a server with no
// fleet configured, or run with --no-fleet-reconcile, simply never calls SetEnabled/Report on it,
// so it reports the zero value: {enabled: false}.
type FleetReconcileStatus struct {
	mu sync.RWMutex

	enabled  bool
	interval time.Duration

	lastRunAt time.Time
	lastPlan  fleet.Plan
	lastErr   error
}

// SetEnabled records whether the fleet reconciler loop is running and, if so, its tick interval.
// Called once at `grove serve` startup.
func (s *FleetReconcileStatus) SetEnabled(enabled bool, interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = enabled
	s.interval = interval
}

// Report records the outcome of one reconcile tick: the plan that was computed (the zero Plan if
// planning itself failed before producing one) and any error from planning or applying it. Safe
// to call from the reconciler loop's own goroutine while handlers concurrently read snapshots.
func (s *FleetReconcileStatus) Report(plan fleet.Plan, err error, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRunAt = at
	s.lastPlan = plan
	s.lastErr = err
}

// FleetReconcilePlanNames is the create/delete VM names from the reconciler's most recent Plan —
// enough for an operator or the UI to see what happened without the full orchard.VMSpec payload.
type FleetReconcilePlanNames struct {
	Creates []string `json:"creates"`
	Deletes []string `json:"deletes"`
}

// FleetReconcileResponse is the body of GET /fleet/reconcile.
type FleetReconcileResponse struct {
	LastRunAt time.Time               `json:"lastRunAt"`
	LastPlan  FleetReconcilePlanNames `json:"lastPlan"`
	LastError string                  `json:"lastError,omitempty"`
	Interval  string                  `json:"interval,omitempty"`
	Enabled   bool                    `json:"enabled"`
}

// FleetReconcileSummary is the subset of FleetReconcileResponse folded into GET /fleet's own
// response, under "reconcile" — just enough for the Fleet page to show "last reconcile: …"
// without a second request.
type FleetReconcileSummary struct {
	LastRunAt time.Time `json:"lastRunAt"`
	Enabled   bool      `json:"enabled"`
	LastError string    `json:"lastError,omitempty"`
}

func planNames(p fleet.Plan) FleetReconcilePlanNames {
	creates := make([]string, 0, len(p.Creates))
	for _, c := range p.Creates {
		creates = append(creates, c.Spec.Name)
	}
	deletes := make([]string, 0, len(p.Deletes))
	for _, d := range p.Deletes {
		deletes = append(deletes, d.Name)
	}
	return FleetReconcilePlanNames{Creates: creates, Deletes: deletes}
}

func (s *FleetReconcileStatus) response() FleetReconcileResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	resp := FleetReconcileResponse{
		LastRunAt: s.lastRunAt,
		LastPlan:  planNames(s.lastPlan),
		Enabled:   s.enabled,
	}
	if s.interval > 0 {
		resp.Interval = s.interval.String()
	}
	if s.lastErr != nil {
		resp.LastError = s.lastErr.Error()
	}
	return resp
}

func (s *FleetReconcileStatus) summary() FleetReconcileSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sum := FleetReconcileSummary{LastRunAt: s.lastRunAt, Enabled: s.enabled}
	if s.lastErr != nil {
		sum.LastError = s.lastErr.Error()
	}
	return sum
}

func (s *Server) handleFleetReconcile(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.fleetReconcile.response())
}
