package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

// Fallback records that a job was moved off the pool it asked for because nothing there could
// place it (see PoolConfig.Fallback). The job then ran on To; Request.Pool still says From.
type Fallback struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// Fallback timing defaults (see Options.FallbackAfter / FallbackNoNodesAfter).
const (
	// defaultFallbackAfter is how long a job may wait unplaced in a pool that has nodes (they're
	// all busy) before it moves to the fallback pool.
	defaultFallbackAfter = 3 * time.Minute
	// defaultFallbackNoNodesAfter is how long a job waits before moving when Nomad reports that
	// no node of its pool exists at all: just long enough for a first scheduling pass.
	defaultFallbackNoNodesAfter = 20 * time.Second
)

// Learning which jobs don't work in the fallback pool. A job's signature is its kind and repo
// (see fallbackSignature). When its last fallbackStrikes runs in the fallback pool all failed, it
// stops falling back for fallbackCooldown after the last of those failures, so it waits for (and
// creates demand for) capacity in its own pool. The verdict comes from the job history, so it
// survives restarts, and it decays on its own: after the cooldown the next job tries again.
const (
	fallbackStrikes  = 3
	fallbackCooldown = 7 * 24 * time.Hour
)

// fallbackSignature groups jobs that would fail alike in another pool: same kind, same repo.
func fallbackSignature(req JobRequest) string {
	return string(req.Kind) + "|" + req.Pool + "|" + req.Repo
}

// fallbackTarget is the pool rec may move to now, or "" with no move. evals are rec's Nomad
// evaluations (most recent first).
func (s *service) fallbackTarget(rec *record, evals []nomad.Evaluation, now time.Time) (target, reason string) {
	req := rec.Request
	target = s.pools[req.Pool].Fallback
	if target == "" || rec.Fallback != nil || req.PinPool || req.Meta["image"] != "" {
		return "", ""
	}
	if _, ok := s.pools[target]; !ok {
		return "", ""
	}
	if len(req.Credentials) > 0 && s.pools[target].Isolated {
		// A sealed pool's VMs can't reach the credential broker.
		return "", ""
	}

	waited := now.Sub(rec.SubmittedAt)
	switch {
	case noPoolNodes(evals) && waited >= s.opts.FallbackNoNodesAfter:
		reason = fmt.Sprintf("no %s node can run jobs", req.Pool)
	case waited >= s.opts.FallbackAfter:
		reason = fmt.Sprintf("not placed on %s within %s", req.Pool, s.opts.FallbackAfter)
	default:
		return "", ""
	}
	if until, ok := s.fallbackBlockedUntil(req, now); ok {
		rec.PendingReason = fmt.Sprintf("waiting for %s: its last %d runs on %s failed, so it won't fall back until %s",
			req.Pool, fallbackStrikes, target, until.Format(time.RFC3339))
		return "", ""
	}
	return target, reason
}

// noPoolNodes reports whether the most recent blocked evaluation filtered out every node it
// looked at by constraint (or saw none at all): no node of the job's pool exists right now.
func noPoolNodes(evals []nomad.Evaluation) bool {
	for _, e := range evals {
		for _, m := range e.FailedTGAllocs {
			return sumCounts(m.DimensionExhausted) == 0 && sumCounts(m.ConstraintFiltered)+sumCounts(m.ClassFiltered) >= m.NodesEvaluated
		}
	}
	return false
}

// fallbackBlockedUntil says whether jobs like req have stopped falling back, and until when.
func (s *service) fallbackBlockedUntil(req JobRequest, now time.Time) (time.Time, bool) {
	sig := fallbackSignature(req)
	var runs []Job
	for _, rec := range s.store.list() {
		if rec.Fallback == nil || rec.FinishedAt == nil || fallbackSignature(rec.Request) != sig {
			continue
		}
		switch rec.Status {
		case StatusSuccess, StatusFailed:
			if !rec.TimedOut && !rec.Quarantined {
				runs = append(runs, rec.Job)
			}
		}
	}
	if len(runs) < fallbackStrikes {
		return time.Time{}, false
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].FinishedAt.After(*runs[j].FinishedAt) })
	for _, r := range runs[:fallbackStrikes] {
		if r.Status != StatusFailed {
			return time.Time{}, false
		}
	}
	until := runs[0].FinishedAt.Add(fallbackCooldown)
	return until, now.Before(until)
}

// fallBack moves rec's still-unplaced job to target: it dispatches the same meta and payload
// (read back from Nomad, since grove keeps neither) to target's parameterized job, then stops and
// purges the original so it can't also run later. On success rec points at the new Nomad job.
func (s *service) fallBack(ctx context.Context, rec *record, target, reason string, now time.Time) error {
	if err := s.redispatch(ctx, rec, target); err != nil {
		return err
	}
	rec.Fallback = &Fallback{From: rec.Request.Pool, To: target, At: now, Reason: reason}
	return nil
}

// redispatch dispatches rec's job again to pool's parameterized job with the meta and payload read
// back from Nomad, then stops and purges the old Nomad job so exactly one copy exists. On success
// rec.NomadJobID names the new copy; on any failure rec is unchanged and the original is the one
// left (a failed stop also removes the new copy). The new dispatch carries nothing about where the
// old one ran: the pool's workers are fungible and any of them may take it.
func (s *service) redispatch(ctx context.Context, rec *record, pool string) error {
	inputs, ok := nomad.AsDispatchInputs(s.nomad)
	if !ok {
		return errors.New("nomad client can't read dispatch inputs back")
	}
	meta, payload, err := inputs.DispatchInput(ctx, rec.NomadJobID)
	if err != nil {
		return err
	}
	name := jobName(rec.Request.Kind, pool)
	res, err := s.nomad.Dispatch(ctx, name, meta, payload)
	if err != nil {
		return fmt.Errorf("dispatch %s: %w", name, err)
	}
	if err := s.nomad.StopJob(ctx, rec.NomadJobID, true); err != nil && !errors.Is(err, nomad.ErrNotFound) {
		// Keep exactly one copy queued: the original, which grove still tracks.
		if serr := s.nomad.StopJob(ctx, res.DispatchedJobID, true); serr != nil {
			return errors.Join(fmt.Errorf("stop %s: %w", rec.NomadJobID, err), fmt.Errorf("stop redispatched copy %s: %w", res.DispatchedJobID, serr))
		}
		return fmt.Errorf("stop %s: %w", rec.NomadJobID, err)
	}
	rec.NomadJobID = res.DispatchedJobID
	return nil
}
