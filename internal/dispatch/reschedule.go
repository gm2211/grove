package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

// Rescheduling a shared pool's lost job.
//
// A shared pool's job runs in a Nomad allocation on one of the pool's VMs. When that VM, or the
// Mac under it, goes away mid-job (a crash, a reboot, a laptop lid closing, the worker leaving
// the fleet), Nomad marks the allocation lost. Grove, not Nomad, decides what happens next
// (nomad/jobs: `reschedule { attempts = 0 }`), by the same rules a sealed pool's driver follows:
//
//   - The job is run again, anywhere in its pool (the workers are fungible: no pin to the lost
//     one), up to MaxAttempts runs in all. Evictions (`snooze --now`) never count.
//   - A job lost after its script started on QuarantineMachines different Macs is quarantined:
//     it most likely takes machines down itself, a poison pill, so it is failed and not rerun.
//   - A job that looks exactly like one quarantined in the last PoisonMemory (same kind, pool,
//     repo, ref and script: poisonFingerprint) is quarantined on its first loss after its
//     script started, so resubmitting a poison pill can't take down two more Macs each time.
//   - A job that may not be rerun (JobRequest.RetryLost: agent jobs by default) ends lost.
//
// Whenever grove ends such a job, it stops its Nomad job first. Nomad may itself place a
// replacement for a lost allocation (on a node that is down) or hold a blocked evaluation for one
// (when no node can take it); left alone, that copy would run later, unwatched, past the budget.
// If Nomad's replacement already exists and the budget allows a rerun, it is the rerun.
//
// Every attempt, lost or finished, is kept on Job.Attempts with its allocation (Attempt.Alloc),
// so internal/health sees a shared pool's VMs that keep losing jobs too.

// DefaultUnknownGrace is how long a shared pool's allocation may stay "unknown" before grove
// treats it as lost. Nomad marks it unknown once its node misses heartbeats (nomad/jobs keep Nomad
// from replacing it); a node back within the grace carries on with its job.
const DefaultUnknownGrace = time.Minute

// PoisonMemory is how long a quarantined job keeps jobs exactly like it from being rerun.
const PoisonMemory = 24 * time.Hour

// lostVerdict is what happens to a job that was just lost.
type lostVerdict struct {
	Rerun      bool
	Quarantine bool
	Reason     string
}

// decideLost decides what happens to a job whose last attempt (attempts' last entry) was just
// lost. retryLost is the request's effective RetryLost; poisoned says a job with the same
// poisonFingerprint was quarantined within PoisonMemory. spec/quint/reschedule.qnt models it.
func decideLost(attempts []Attempt, retryLost, poisoned bool) lostVerdict {
	last := attempts[len(attempts)-1]
	where := last.Worker
	if where == "" {
		where = "an unknown worker"
	}
	if machines := lostMachines(attempts); len(machines) >= QuarantineMachines {
		return lostVerdict{Quarantine: true, Reason: fmt.Sprintf("quarantined: lost after its script started on %d different machines (%s); it most likely takes them down, so it is not run again",
			len(machines), strings.Join(machines, ", "))}
	}
	if poisoned && last.JobStarted {
		return lostVerdict{Quarantine: true, Reason: fmt.Sprintf("quarantined: lost on %s after its script started, and a job with the same repo, ref and script was quarantined within %s", where, PoisonMemory)}
	}
	if !retryLost {
		return lostVerdict{Reason: fmt.Sprintf("lost on %s; not run again: the job does not allow a rerun (retryLost)", where)}
	}
	if n := countedAttempts(attempts); n >= MaxAttempts {
		return lostVerdict{Reason: fmt.Sprintf("lost on %s; not run again: it was run %d times, the most a job gets", where, n)}
	}
	return lostVerdict{Rerun: true, Reason: fmt.Sprintf("rescheduled: lost on %s (run %d of at most %d)", where, countedAttempts(attempts), MaxAttempts)}
}

// lostMachines is the sorted set of machines the job was lost on after its script started. A loss
// on a machine grove couldn't name counts towards the budget only, never towards quarantine.
func lostMachines(attempts []Attempt) []string {
	seen := map[string]bool{}
	for _, a := range attempts {
		if a.Outcome == AttemptLost && a.JobStarted && a.Worker != "" {
			seen[a.Worker] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// countedAttempts is how many of attempts count towards MaxAttempts: all but evictions.
func countedAttempts(attempts []Attempt) int {
	n := 0
	for _, a := range attempts {
		if a.Outcome != AttemptEvicted {
			n++
		}
	}
	return n
}

// poisonFingerprint groups jobs that would do exactly the same thing: same kind, pool, repo, ref
// and script. Only a hash of the script is kept in it.
func poisonFingerprint(req JobRequest) string {
	h := sha256.New()
	for _, part := range []string{string(req.Kind), req.Pool, req.Repo, req.Ref, req.Script} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// poisoned reports whether a job other than id with req's fingerprint was quarantined within
// PoisonMemory of now. It reads the job history, so the memory survives restarts.
func (s *service) poisoned(id string, req JobRequest, now time.Time) bool {
	fp := poisonFingerprint(req)
	for _, rec := range s.store.list() {
		if rec.ID == id || !rec.Quarantined || rec.FinishedAt == nil || now.Sub(*rec.FinishedAt) > PoisonMemory {
			continue
		}
		if poisonFingerprint(rec.Request) == fp {
			return true
		}
	}
	return false
}

// runPool is the pool rec's job runs in now: its fallback pool once it moved there.
func runPool(rec *record) string {
	if rec.Fallback != nil {
		return rec.Fallback.To
	}
	return rec.Request.Pool
}

func hasAllocAttempt(attempts []Attempt, alloc string) bool {
	for _, a := range attempts {
		if a.Alloc == alloc {
			return true
		}
	}
	return false
}

// allocAttempt is the Attempt for one of a shared pool's allocations.
func (s *service) allocAttempt(ctx context.Context, a nomad.Allocation, outcome string, now time.Time) Attempt {
	at := Attempt{Alloc: a.ID, StartedAt: a.CreatedAt, Outcome: outcome, JobStarted: a.Started, ExitCode: a.ExitCode}
	if node, err := s.nomad.GetNode(ctx, a.NodeID); err == nil && node != nil {
		at.VM, at.Worker = node.Meta["vm"], node.Meta["host"]
	}
	if at.VM == "" {
		at.VM = a.NodeName
	}
	fin := now
	if a.FinishedAt != nil {
		fin = *a.FinishedAt
	}
	at.FinishedAt = &fin
	if outcome == AttemptLost {
		at.Reason = "allocation lost: the VM or its Mac stopped answering"
		if a.ClientStatus == "unknown" {
			at.Reason = fmt.Sprintf("allocation lost: Nomad lost contact with the VM for over %s", s.opts.UnknownGrace)
		}
		at.ExitCode = nil
	}
	return at
}

// handleLosses records every allocation of rec's shared-pool job that Nomad marked lost and acts
// on the newest loss (decideLost). handled is false when the job carries on as updated says:
// nothing new was lost, or Nomad's own replacement is the rerun the budget allows. Otherwise the
// returned Job is final for this reconcile: requeued, or ended.
func (s *service) handleLosses(ctx context.Context, rec *record, updated *Job, allocs []nomad.Allocation) (job *Job, handled bool, err error) {
	now := time.Now().UTC()
	sorted := append([]nomad.Allocation(nil), allocs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })
	lostNow := false
	unknown := map[string]time.Time{}
	for _, a := range sorted {
		if hasAllocAttempt(updated.Attempts, a.ID) {
			continue
		}
		switch a.ClientStatus {
		case "lost":
		case "unknown":
			since, ok := rec.UnknownSince[a.ID]
			if !ok {
				since = now
			}
			if now.Sub(since) < s.opts.UnknownGrace {
				unknown[a.ID] = since
				continue
			}
		default:
			continue
		}
		updated.Attempts = append(updated.Attempts, s.allocAttempt(ctx, a, AttemptLost, now))
		lostNow = true
	}
	if len(unknown) == 0 {
		unknown = nil
	}
	rec.UnknownSince = unknown
	if !lostNow {
		return nil, false, nil
	}
	latest := latestAllocation(allocs)
	if latest.ClientStatus == "complete" || latest.ClientStatus == "failed" {
		// A run after the losses (Nomad's own replacement) already finished: its result stands.
		return nil, false, nil
	}
	v := decideLost(updated.Attempts, rec.Request.retryLost(), s.poisoned(rec.ID, rec.Request, now))
	if v.Rerun && latest.ClientStatus != "lost" && latest.ClientStatus != "unknown" {
		// Nomad already placed a replacement for the lost allocation: that is the rerun.
		slog.Info("dispatch: lost job continues in Nomad's replacement allocation", "id", rec.ID, "alloc", latest.ID, "attempts", countedAttempts(updated.Attempts))
		return nil, false, nil
	}
	if v.Rerun {
		prev := rec.Job.Attempts
		rec.Job.Attempts = updated.Attempts
		if rerr := s.requeue(ctx, rec, runPool(rec), v.Reason); rerr != nil {
			// Nothing was persisted: the next reconcile sees the same losses and tries again.
			rec.Job.Attempts = prev
			return nil, true, fmt.Errorf("dispatch: rescheduling lost job %s: %w", rec.ID, rerr)
		}
		slog.Warn("dispatch: a job was lost with its VM; running it again", "id", rec.ID, "pool", runPool(rec), "reason", v.Reason)
		j := rec.Job
		return &j, true, nil
	}
	// Ending it: first make sure Nomad holds no copy that could still run (a replacement, or a
	// blocked evaluation waiting for a node). Kept, not purged, so its allocations stay readable.
	if serr := s.nomad.StopJob(ctx, rec.NomadJobID, false); serr != nil && !errors.Is(serr, nomad.ErrNotFound) {
		// Nothing was persisted: the next reconcile sees the same losses and tries again.
		return nil, true, fmt.Errorf("dispatch: stop lost job %s: %w", rec.ID, serr)
	}
	rec.Job = *updated
	status := StatusLost
	if v.Quarantine {
		status = StatusFailed
		rec.Quarantined = true
		slog.Warn("dispatch: quarantined a job that keeps getting lost with its machines", "id", rec.ID, "reason", v.Reason)
	}
	j, merr := s.markEnded(rec, status, v.Reason)
	return j, true, merr
}

// recordExit adds the attempt of a shared-pool job's allocation that ran to its end, so a VM or
// Mac that finishes jobs gets a clean slate in internal/health.
func (s *service) recordExit(ctx context.Context, updated *Job, alloc nomad.Allocation) {
	if alloc.ID == "" || hasAllocAttempt(updated.Attempts, alloc.ID) {
		return
	}
	updated.Attempts = append(updated.Attempts, s.allocAttempt(ctx, alloc, AttemptExited, time.Now().UTC()))
}

// requeue dispatches rec's job again onto pool and resets it to pending, with pendingReason
// saying why. The new dispatch carries no pin to where it ran: any worker of the pool may take it.
func (s *service) requeue(ctx context.Context, rec *record, pool, pendingReason string) error {
	if err := s.redispatch(ctx, rec, pool); err != nil {
		return err
	}
	now := time.Now().UTC()
	rec.Status = StatusPending
	rec.AllocID, rec.Node, rec.ExitCode = "", "", nil
	rec.StartedAt, rec.FinishedAt, rec.Placement, rec.Signal = nil, nil, nil, nil
	rec.FailureReason, rec.TimedOut = nil, false
	rec.PendingReason = pendingReason
	// The pending timeout and the unplaceable grace start over: the job is waiting afresh.
	rec.PendingSince = &now
	if err := s.store.put(rec); err != nil {
		return fmt.Errorf("dispatch: persist job %s: %w", rec.ID, err)
	}
	s.mu.Lock()
	delete(s.cache, rec.ID)
	s.mu.Unlock()
	return nil
}
