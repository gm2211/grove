package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Evictor moves a snoozed worker's jobs elsewhere (`grove worker snooze --now`). It is optional,
// like nomad.NodePurger: dispatch.Service does not carry it, so the seam other packages build
// against is unchanged; use AsEvictor.
type Evictor interface {
	// Evict requeues, onto their own pool, the non-terminal shared-pool jobs placed on worker whose
	// request allows a rerun (JobRequest.RetryLost: build and shell jobs by default), and leaves
	// the rest running (agent jobs, which must not be killed midway). It returns the ids of both.
	// A job whose redispatch fails stays where it is and is reported in left.
	Evict(ctx context.Context, worker, reason string) (requeued, left []string, err error)
}

// AsEvictor returns svc's Evictor implementation, if it has one.
func AsEvictor(svc Service) (Evictor, bool) {
	e, ok := svc.(Evictor)
	return e, ok
}

// Evict implements Evictor. Sealed pools are not handled here: their driver watches the snooze
// store and stops its own VM (internal/sandbox).
func (s *service) Evict(ctx context.Context, worker, reason string) (requeued, left []string, err error) {
	recs := s.store.list()
	for i := range recs {
		rec := &recs[i]
		if isTerminal(rec.Status) || s.pools[rec.Request.Pool].Isolated {
			continue
		}
		// Refresh first: the stored placement may be a status poll behind.
		if _, rerr := s.reconcile(ctx, rec); rerr != nil {
			slog.Warn("dispatch: refreshing a job before eviction failed", "id", rec.ID, "err", rerr)
		}
		if rec.Placement == nil || rec.Placement.WorkerID != worker || isTerminal(rec.Status) {
			continue
		}
		if !rec.Request.retryLost() {
			left = append(left, rec.ID)
			continue
		}
		if err := s.requeueEvicted(ctx, rec, worker, reason); err != nil {
			slog.Warn("dispatch: evicting a job off a snoozed worker failed; leaving it running", "id", rec.ID, "worker", worker, "err", err)
			left = append(left, rec.ID)
			continue
		}
		requeued = append(requeued, rec.ID)
	}
	return requeued, left, nil
}

func (s *service) requeueEvicted(ctx context.Context, rec *record, worker, reason string) error {
	pool := rec.Request.Pool
	if rec.Fallback != nil {
		pool = rec.Fallback.To
	}
	if err := s.redispatch(ctx, rec, pool); err != nil {
		return err
	}
	rec.Status = StatusPending
	rec.AllocID, rec.Node, rec.ExitCode = "", "", nil
	rec.StartedAt, rec.Placement, rec.Signal = nil, nil, nil
	rec.PendingReason = fmt.Sprintf("requeued: worker %s was snoozed", worker)
	rec.Evictions = append(rec.Evictions, Eviction{Worker: worker, At: time.Now().UTC(), Reason: reason})
	if err := s.store.put(rec); err != nil {
		return fmt.Errorf("dispatch: persist job %s: %w", rec.ID, err)
	}
	s.mu.Lock()
	delete(s.cache, rec.ID)
	s.mu.Unlock()
	return nil
}
