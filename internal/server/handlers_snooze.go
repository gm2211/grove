package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/snooze"
)

// snoozeNodeDrainDeadline is the deadline for draining a snoozed worker's Nomad nodes. Never 0:
// Nomad reads a zero deadline as "no deadline".
const snoozeNodeDrainDeadline = 2 * time.Hour

// SnoozeRequest is the body of POST /workers/{name}/snooze. For and Until are mutually
// exclusive; neither means snooze.DefaultDuration.
type SnoozeRequest struct {
	For   string `json:"for,omitempty"`   // a Go duration: "2h", "90m"
	Until string `json:"until,omitempty"` // RFC 3339
	Now   bool   `json:"now,omitempty"`   // also requeue running build/shell jobs elsewhere
	Note  string `json:"note,omitempty"`
}

// SnoozeResponse is the body of a successful POST /workers/{name}/snooze.
type SnoozeResponse struct {
	Worker      string    `json:"worker"`
	Until       time.Time `json:"until"`
	Mode        string    `json:"mode"`
	Reasons     []string  `json:"reasons"`
	Requeued    []string  `json:"requeued"`
	LeftRunning []string  `json:"leftRunning"`
}

// WakeResponse is the body of POST /workers/{name}/wake.
type WakeResponse struct {
	Worker             string   `json:"worker"`
	Resumed            bool     `json:"resumed"`
	StillPausedBecause []string `json:"stillPausedBecause"`
}

// SnoozeStatus is the body of GET /workers/{name}/snooze.
type SnoozeStatus struct {
	Worker  string     `json:"worker"`
	Snoozed bool       `json:"snoozed"`
	Until   *time.Time `json:"until,omitempty"`
	Mode    string     `json:"mode,omitempty"`
	By      string     `json:"by,omitempty"`
	At      *time.Time `json:"at,omitempty"`
	Note    string     `json:"note,omitempty"`
	Reasons []string   `json:"reasons"`
}

func (s *Server) snoozeUnavailable(w http.ResponseWriter) bool {
	if s.opts.Pauses == nil {
		http.Error(w, "snoozing is not configured on this control plane", http.StatusServiceUnavailable)
		return true
	}
	return false
}

// parseSnoozeWindow turns a request into an end time, rejecting anything over snooze.MaxDuration.
func parseSnoozeWindow(req SnoozeRequest, now time.Time) (time.Time, error) {
	if req.For != "" && req.Until != "" {
		return time.Time{}, errors.New("give either for or until, not both")
	}
	var until time.Time
	switch {
	case req.For != "":
		d, err := time.ParseDuration(strings.TrimSpace(req.For))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid for %q: %v", req.For, err)
		}
		until = now.Add(d)
	case req.Until != "":
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(req.Until))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid until %q (want RFC 3339): %v", req.Until, err)
		}
		until = t
	default:
		until = now.Add(snooze.DefaultDuration)
	}
	if !until.After(now) {
		return time.Time{}, errors.New("the snooze must end in the future")
	}
	if until.Sub(now) > snooze.MaxDuration {
		return time.Time{}, fmt.Errorf("a snooze lasts at most %s", snooze.MaxDuration)
	}
	return until.UTC(), nil
}

func (s *Server) handleWorkerSnooze(w http.ResponseWriter, r *http.Request) {
	if s.snoozeUnavailable(w) {
		return
	}
	name := r.PathValue("name")
	if !validWorkerName(name) {
		http.Error(w, "invalid worker name", http.StatusBadRequest)
		return
	}
	var req SnoozeRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	now := time.Now().UTC()
	until, err := parseSnoozeWindow(req, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mode := snooze.ModeDrain
	if req.Now {
		mode = snooze.ModeNow
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 200 {
		note = note[:200]
	}
	sn := snooze.Snooze{Until: until, Mode: mode, By: principalFromContext(r.Context()).Name, At: now, Note: note}

	// 1. Record the reason and pause the worker, so nothing new is placed on it.
	if err := snooze.Engage(r.Context(), s.orchard, s.opts.Pauses, name, sn, now); err != nil {
		if errors.Is(err, snooze.ErrUnknownWorker) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	// 2. Drain the worker's ready Nomad nodes now: the reconciler would do it on its next tick,
	// but a job placed in that gap would land on the Mac the owner wants quiet.
	s.drainWorkerNodes(r, name)

	resp := SnoozeResponse{
		Worker: name, Until: until, Mode: string(mode),
		Reasons:  s.opts.Pauses.Reasons(name, now),
		Requeued: []string{}, LeftRunning: []string{},
	}
	// 3. With --now, move what can move.
	if req.Now {
		if ev, ok := dispatch.AsEvictor(s.dispatch); ok {
			requeued, left, err := ev.Evict(r.Context(), name, "worker snoozed with --now")
			if err != nil {
				s.log.Error("snooze: evicting jobs failed", "worker", name, "err", err)
			}
			if requeued != nil {
				resp.Requeued = requeued
			}
			if left != nil {
				resp.LeftRunning = left
			}
		}
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// drainWorkerNodes drains every ready Nomad node whose host is worker. Failures are logged: the
// snooze is already recorded and the fleet reconciler drains these nodes on its next tick anyway.
func (s *Server) drainWorkerNodes(r *http.Request, worker string) {
	nodes, err := s.nomad.ListNodes(r.Context())
	if err != nil {
		s.log.Warn("snooze: listing nodes to drain failed", "worker", worker, "err", err)
		return
	}
	for _, n := range nodes {
		if n.Meta["host"] != worker || n.Status != "ready" || n.Drain {
			continue
		}
		if err := s.nomad.DrainNode(r.Context(), n.ID, true, snoozeNodeDrainDeadline); err != nil {
			s.log.Warn("snooze: draining a node failed", "worker", worker, "node", n.ID, "err", err)
		}
	}
}

func (s *Server) handleWorkerWake(w http.ResponseWriter, r *http.Request) {
	if s.snoozeUnavailable(w) {
		return
	}
	name := r.PathValue("name")
	if !validWorkerName(name) {
		http.Error(w, "invalid worker name", http.StatusBadRequest)
		return
	}
	// A worker with no snooze recorded may be paused for someone else's reason (or an external
	// one): wake must never resume that.
	if s.opts.Pauses.Get(name).Snooze == nil {
		http.Error(w, fmt.Sprintf("worker %s is not snoozed", name), http.StatusConflict)
		return
	}
	resumed, still, err := snooze.Wake(r.Context(), s.orchard, s.opts.Pauses, name, time.Now())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	if still == nil {
		still = []string{}
	}
	s.writeJSON(w, http.StatusOK, WakeResponse{Worker: name, Resumed: resumed, StillPausedBecause: still})
}

func (s *Server) handleGetWorkerSnooze(w http.ResponseWriter, r *http.Request) {
	if s.snoozeUnavailable(w) {
		return
	}
	name := r.PathValue("name")
	now := time.Now()
	e := s.opts.Pauses.Get(name)
	st := SnoozeStatus{Worker: name, Reasons: e.Reasons(now)}
	if st.Reasons == nil {
		st.Reasons = []string{}
	}
	if e.Active(now) {
		sn := e.Snooze
		st.Snoozed, st.Until, st.Mode, st.By, st.At, st.Note = true, &sn.Until, string(sn.Mode), sn.By, &sn.At, sn.Note
	}
	s.writeJSON(w, http.StatusOK, st)
}
