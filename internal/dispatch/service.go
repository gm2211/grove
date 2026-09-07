package dispatch

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"context"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/nomad"
)

// ErrNotFound is returned by Get/Cancel/Logs when no job with the given id is known.
var ErrNotFound = errors.New("dispatch: job not found")

// ErrNoAllocationYet is returned by Logs/LogLines when a job has no Nomad allocation to stream
// from: with follow=false this is returned immediately (no wait); with follow=true it is only
// returned once allocIDFor has polled until ctx is done (deadline or cancellation) or the job
// reached a terminal status without ever getting an allocation. The HTTP server handler treats
// this as "200, empty body, X-Grove-Job-Status: pending" rather than an error response — see
// internal/server/handlers_jobs.go.
var ErrNoAllocationYet = errors.New("dispatch: job has no allocation yet")

// defaultAllocationPollInterval is how often allocIDFor re-checks Nomad for a job's allocation
// while waiting on follow=true, absent an Options.AllocationPollInterval override.
const defaultAllocationPollInterval = 1 * time.Second

// defaultPendingTimeout is how long a job may sit StatusPending with no Nomad allocation before
// reconcile gives up on it and marks it StatusLost, absent an Options.PendingTimeout override —
// see markLost's "no allocation within" reason.
const defaultPendingTimeout = 30 * time.Minute

// DefaultReconcileInterval is the tick interval `grove serve` passes to RunReconciler.
const DefaultReconcileInterval = 30 * time.Second

// defaultLogTerminalPollInterval is how often a follow=true Logs/LogLines stream re-checks
// whether the job it's following has gone terminal while already streaming, absent an
// Options.LogTerminalPollInterval override. See watchForTerminal.
const defaultLogTerminalPollInterval = 1 * time.Second

// defaultLogStreamGracePeriod is how long a follow=true Logs/LogLines stream stays open after
// observing its job go terminal, absent an Options.LogStreamGracePeriod override — long enough for
// Nomad to deliver whatever log frames it already had buffered before the stream is severed. See
// watchForTerminal.
const defaultLogStreamGracePeriod = 2 * time.Second

// Options configures a dispatch Service.
type Options struct {
	// ArtifactsBase optionally names the artifact bucket/base clients should assume artifact
	// paths (artifact_prefix) are relative to. Purely informational for now — the running job
	// is the one that actually uploads to it.
	ArtifactsBase string
	// Artifacts, when non-nil, is used to populate Job.Artifacts once a job reaches a terminal
	// status. Nil means "no artifact store configured" — Job.Artifacts is just never populated.
	Artifacts artifacts.Client
	// StorePath overrides the on-disk job history location. Empty uses
	// $XDG_STATE_HOME/grove/jobs.json (default ~/.local/state/grove/jobs.json). Pass a path in a
	// throwaway directory (or leave the default empty-string sentinel via NewForTest) to disable
	// persistence, e.g. in tests.
	StorePath string
	// StatusTTL caps how often Get/List re-query Nomad for a job's allocations. Defaults to 3s.
	StatusTTL time.Duration
	// Pools, when set, is used only to validate JobRequest.Resources hints against each pool's
	// configured job CPU/Memory defaults (Submit rejects a hint that exceeds them with a 400). It
	// is unrelated to EnsureJobs, which takes its own []PoolConfig directly — callers that already
	// have that slice (see internal/cli/serve.go) pass the same one here. Pool CPU/Memory here are
	// assumed already resolved to their effective (non-zero) value — see fleet.Pool.JobCPUOrDefault
	// /JobMemoryOrDefault — since a zero here is treated as "no configured limit for this pool" and
	// skips validation entirely, not as "0 MHz/MiB allowed".
	Pools []PoolConfig
	// AllocationPollInterval overrides defaultAllocationPollInterval — mainly for tests, so a
	// follow=true wait-for-allocation test isn't stuck sleeping in real seconds.
	AllocationPollInterval time.Duration
	// PendingTimeout overrides defaultPendingTimeout: how long a job may sit StatusPending with no
	// Nomad allocation before reconcile marks it StatusLost (fleet full, or an unmatchable
	// placement constraint — Nomad will otherwise leave it queued forever).
	PendingTimeout time.Duration
	// LogTerminalPollInterval overrides defaultLogTerminalPollInterval — mainly for tests. See
	// watchForTerminal.
	LogTerminalPollInterval time.Duration
	// LogStreamGracePeriod overrides defaultLogStreamGracePeriod — mainly for tests. See
	// watchForTerminal.
	LogStreamGracePeriod time.Duration
}

type cachedStatus struct {
	at  time.Time
	job Job
}

type service struct {
	nomad     nomad.Client
	artifacts artifacts.Client
	opts      Options
	store     *store
	pools     map[string]PoolConfig

	mu    sync.Mutex
	cache map[string]cachedStatus
}

// New builds a dispatch Service backed by nc. It opens (or creates) the on-disk job store.
func New(nc nomad.Client, opts Options) (Service, error) {
	if nc == nil {
		return nil, fmt.Errorf("dispatch: nomad client is required")
	}
	path := opts.StorePath
	if path == "" {
		path = defaultStorePath()
	}
	st, err := newStore(path)
	if err != nil {
		return nil, fmt.Errorf("dispatch: open store %s: %w", path, err)
	}
	if opts.StatusTTL <= 0 {
		opts.StatusTTL = 3 * time.Second
	}
	if opts.AllocationPollInterval <= 0 {
		opts.AllocationPollInterval = defaultAllocationPollInterval
	}
	if opts.PendingTimeout <= 0 {
		opts.PendingTimeout = defaultPendingTimeout
	}
	if opts.LogTerminalPollInterval <= 0 {
		opts.LogTerminalPollInterval = defaultLogTerminalPollInterval
	}
	if opts.LogStreamGracePeriod <= 0 {
		opts.LogStreamGracePeriod = defaultLogStreamGracePeriod
	}
	pools := make(map[string]PoolConfig, len(opts.Pools))
	for _, p := range opts.Pools {
		pools[p.Name] = p
	}
	return &service{nomad: nc, artifacts: opts.Artifacts, opts: opts, store: st, pools: pools, cache: map[string]cachedStatus{}}, nil
}

// jobName returns the parameterized Nomad job name for a kind×pool pair, e.g. "grove-build-linux".
func jobName(kind Kind, pool string) string {
	return fmt.Sprintf("grove-%s-%s", kind, pool)
}

func validate(req JobRequest) error {
	switch req.Kind {
	case KindBuild, KindAgent, KindShell:
	case "":
		return fmt.Errorf("dispatch: kind is required")
	default:
		return fmt.Errorf("dispatch: unknown kind %q", req.Kind)
	}
	if req.Pool == "" {
		return fmt.Errorf("dispatch: pool is required")
	}
	if req.Script == "" {
		return fmt.Errorf("dispatch: script is required")
	}
	if (req.Kind == KindBuild || req.Kind == KindAgent) && req.Repo == "" {
		return fmt.Errorf("dispatch: repo is required for kind %q", req.Kind)
	}
	if t := req.Timeout.Duration(); t > 0 && t < time.Second {
		return fmt.Errorf(`dispatch: timeout must be at least 1s (send a duration string like "30m" or a number of seconds)`)
	}
	return nil
}

// validateResources rejects req.Resources when it exceeds the target pool's configured job
// defaults. A pool with no configured defaults known to this service (opts.Pools didn't mention
// it, or the service was built without Pools at all) accepts any hint — there's nothing to
// validate against. See ResourceHint's doc comment for why this is validation-only, not applied
// sizing.
func (s *service) validateResources(req JobRequest) error {
	if req.Resources == nil {
		return nil
	}
	pc, ok := s.pools[req.Pool]
	if !ok || (pc.CPU <= 0 && pc.Memory <= 0) {
		return nil
	}
	if pc.CPU > 0 && req.Resources.CPU > pc.CPU {
		return fmt.Errorf("dispatch: requested cpu=%dMHz exceeds pool %q's job default cpu=%dMHz (per-dispatch sizing isn't supported yet — see docs/JOBS.md)", req.Resources.CPU, req.Pool, pc.CPU)
	}
	if pc.Memory > 0 && req.Resources.Memory > pc.Memory {
		return fmt.Errorf("dispatch: requested memory=%dMiB exceeds pool %q's job default memory=%dMiB (per-dispatch sizing isn't supported yet — see docs/JOBS.md)", req.Resources.Memory, req.Pool, pc.Memory)
	}
	return nil
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("dispatch: generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func artifactPrefix(id string) string {
	return "jobs/" + id + "/"
}

func (s *service) Submit(ctx context.Context, req JobRequest) (*Job, bool, error) {
	if err := validate(req); err != nil {
		return nil, false, err
	}
	if err := s.validateResources(req); err != nil {
		return nil, false, err
	}

	if req.IdempotencyKey != "" {
		if rec, ok := s.store.findByIdempotencyKey(req.IdempotencyKey); ok {
			job, err := s.reconcile(ctx, rec)
			if err != nil {
				return nil, false, err
			}
			return job, false, nil
		}
	}

	id, err := randomID()
	if err != nil {
		return nil, false, err
	}

	// Meta mapping follows docs/JOBS.md exactly (the images/jobs agent's authoritative dispatch
	// contract, shared with nomad/jobs/*.nomad.hcl): only "requester" is meta_required, and
	// run.sh treats an absent/empty optional key as "use the default" (e.g.
	// `timeout ${NOMAD_META_timeout_seconds:-3600}` — sending a literal "0" would instead run
	// `timeout 0`, killing the job immediately), so zero-valued fields must be omitted rather
	// than serialized as "0"/"null".
	meta := map[string]string{
		"requester": req.Requester,
	}
	if req.Repo != "" {
		meta["repo"] = req.Repo
	}
	if req.Ref != "" {
		meta["ref"] = req.Ref
	}
	if t := req.Timeout.Duration(); t > 0 {
		meta["timeout_seconds"] = strconv.FormatFloat(t.Seconds(), 'f', 0, 64)
	}
	if len(req.Env) > 0 {
		envJSON, err := json.Marshal(req.Env)
		if err != nil {
			return nil, false, fmt.Errorf("dispatch: marshal env: %w", err)
		}
		meta["env_json"] = string(envJSON)
	}
	if len(req.Meta) > 0 {
		metaJSON, err := json.Marshal(req.Meta)
		if err != nil {
			return nil, false, fmt.Errorf("dispatch: marshal meta: %w", err)
		}
		meta["grove_meta_json"] = string(metaJSON)
	}
	meta["artifact_prefix"] = artifactPrefix(id)
	if img, ok := req.Meta["image"]; ok && img != "" {
		meta["image"] = img
	}

	name := jobName(req.Kind, req.Pool)
	res, err := s.nomad.Dispatch(ctx, name, meta, []byte(req.Script))
	if err != nil {
		return nil, false, fmt.Errorf("dispatch %s: %w", name, err)
	}

	now := time.Now().UTC()
	rec := &record{
		Job: Job{
			ID:          id,
			Request:     req,
			Status:      StatusPending,
			SubmittedAt: now,
			Meta:        req.Meta,
		},
		NomadJobID: res.DispatchedJobID,
	}
	if err := s.store.put(rec); err != nil {
		return nil, false, fmt.Errorf("dispatch: persist job %s: %w", id, err)
	}
	job := rec.Job
	return &job, true, nil
}

func isTerminal(status Status) bool {
	switch status {
	case StatusSuccess, StatusFailed, StatusCanceled, StatusLost:
		return true
	default:
		return false
	}
}

func deriveStatus(alloc *nomad.Allocation) Status {
	switch alloc.ClientStatus {
	case "pending":
		return StatusPending
	case "running":
		return StatusRunning
	case "complete":
		if alloc.ExitCode != nil && *alloc.ExitCode == 0 {
			return StatusSuccess
		}
		return StatusFailed
	case "failed":
		return StatusFailed
	case "lost":
		return StatusLost
	default:
		return StatusPending
	}
}

func latestAllocation(allocs []nomad.Allocation) nomad.Allocation {
	latest := allocs[0]
	for _, a := range allocs[1:] {
		if a.CreatedAt.After(latest.CreatedAt) {
			latest = a
		}
	}
	return latest
}

// reconcile refreshes rec.Job's status from Nomad's allocations, subject to the status TTL cache,
// and persists the refreshed record. Terminal jobs are never re-queried.
func (s *service) reconcile(ctx context.Context, rec *record) (*Job, error) {
	if isTerminal(rec.Status) {
		job := rec.Job
		return &job, nil
	}

	s.mu.Lock()
	cached, ok := s.cache[rec.ID]
	s.mu.Unlock()
	if ok && time.Since(cached.at) < s.opts.StatusTTL {
		job := cached.job
		return &job, nil
	}

	allocs, err := s.nomad.ListAllocations(ctx, rec.NomadJobID)
	if err != nil {
		if errors.Is(err, nomad.ErrNotFound) {
			// The dispatched job no longer exists in Nomad — most commonly a dev/test Nomad agent
			// restarted with its data dir wiped, or the job was purged out from under us. It will
			// never report a status again, so there's nothing to keep polling for: mark it lost
			// rather than leaving it stuck non-terminal forever.
			return s.markLost(rec, fmt.Sprintf("nomad job %s not found (server restarted or job purged)", rec.NomadJobID))
		}
		return nil, fmt.Errorf("dispatch: list allocations for %s: %w", rec.ID, err)
	}

	if len(allocs) == 0 && rec.Status == StatusPending && time.Since(rec.SubmittedAt) > s.opts.PendingTimeout {
		// Nomad still knows about the job but has never placed it — the fleet is full for this
		// pool, or a constraint (e.g. meta.pool) can't be satisfied by any current node. Waiting
		// longer won't help without an operator noticing, so stop showing it as merely "pending".
		return s.markLost(rec, fmt.Sprintf("no allocation within %s (fleet full or constraint unmatchable)", s.opts.PendingTimeout))
	}

	updated := rec.Job
	if len(allocs) > 0 {
		alloc := latestAllocation(allocs)
		updated.Status = deriveStatus(&alloc)
		updated.AllocID = alloc.ID
		updated.Node = alloc.NodeName
		updated.ExitCode = alloc.ExitCode
		if updated.StartedAt == nil && alloc.ClientStatus != "pending" {
			t := alloc.CreatedAt
			updated.StartedAt = &t
		}
		if alloc.FinishedAt != nil {
			updated.FinishedAt = alloc.FinishedAt
		}

		if alloc.ExitCode != nil && *alloc.ExitCode == 124 {
			updated.TimedOut = true
		}
		if alloc.Signal != nil {
			name := signalName(*alloc.Signal)
			updated.Signal = &name
		}
		if alloc.FailureReason != "" {
			fr := alloc.FailureReason
			updated.FailureReason = &fr
		} else if updated.Status == StatusLost {
			fr := "allocation lost — worker or node became unreachable"
			updated.FailureReason = &fr
		}
		updated.Placement = &Placement{AllocID: alloc.ID, NodeID: alloc.NodeID}
		if node, nerr := s.nomad.GetNode(ctx, alloc.NodeID); nerr == nil && node != nil {
			updated.Placement.VMID = node.Meta["vm"]
			updated.Placement.WorkerID = node.Meta["host"]
		} // a GetNode error here is non-fatal — placement partially populated (AllocID/NodeID still set) beats failing the whole Get/List call over a node lookup blip.
	}

	if updated.Status == StatusPending {
		if evals, everr := s.nomad.JobEvaluations(ctx, rec.NomadJobID); everr != nil {
			// Best-effort, same rationale as the GetNode lookup above — a diagnostics lookup
			// failing shouldn't fail the whole reconcile.
			slog.Warn("dispatch: job evaluations lookup failed", "id", rec.ID, "err", everr)
		} else {
			updated.PendingReason = pendingReason(evals)
		}
	} else {
		updated.PendingReason = ""
	}

	rec.Job = updated
	if isTerminal(updated.Status) && len(rec.Job.Artifacts) == 0 && s.artifacts != nil {
		if objs, aerr := s.artifacts.List(ctx, artifactPrefix(rec.ID)); aerr != nil {
			// Artifact listing is best-effort — a bucket hiccup shouldn't fail the whole
			// reconcile, since the running job itself already succeeded or failed on its own.
			slog.Warn("dispatch: listing artifacts failed", "id", rec.ID, "err", aerr)
		} else if len(objs) > 0 {
			arts := make([]Artifact, 0, len(objs))
			for _, obj := range objs {
				arts = append(arts, Artifact{
					Path:        obj.Path,
					URL:         "/api/v1/jobs/" + url.PathEscape(rec.ID) + "/artifacts/" + escapeArtifactPath(obj.Path),
					Size:        obj.Size,
					ContentType: obj.ContentType,
				})
			}
			updated.Artifacts = arts
			rec.Job = updated
		}
	}
	if err := s.store.put(rec); err != nil {
		return nil, fmt.Errorf("dispatch: persist job %s: %w", rec.ID, err)
	}

	s.mu.Lock()
	s.cache[rec.ID] = cachedStatus{at: time.Now(), job: updated}
	s.mu.Unlock()

	job := updated
	return &job, nil
}

// markLost marks rec StatusLost with reason, persists it, refreshes the status cache, and returns
// the updated Job. Used by reconcile when Nomad no longer knows about rec's dispatched job (a 404
// — the server restarted with a wiped data dir, or the job was purged) or when a job has sat
// pending longer than Options.PendingTimeout without ever getting an allocation.
func (s *service) markLost(rec *record, reason string) (*Job, error) {
	now := time.Now().UTC()
	rec.Status = StatusLost
	rec.FailureReason = &reason
	rec.FinishedAt = &now
	rec.PendingReason = ""
	if err := s.store.put(rec); err != nil {
		return nil, fmt.Errorf("dispatch: persist job %s: %w", rec.ID, err)
	}

	s.mu.Lock()
	s.cache[rec.ID] = cachedStatus{at: time.Now(), job: rec.Job}
	s.mu.Unlock()

	job := rec.Job
	return &job, nil
}

// pendingReason summarizes why Nomad hasn't placed a job yet from its evaluations' FailedTGAllocs
// (see nomad.AllocationMetric), for Job.PendingReason. evals is Nomad's own ordering (most recent
// first); the first evaluation carrying a placement-failure metric wins. Returns "" when there's
// nothing to report yet (no evaluations at all, or none have recorded a placement failure — e.g.
// the job was *just* dispatched and Nomad hasn't run a scheduling pass on it yet).
func pendingReason(evals []nomad.Evaluation) string {
	for _, e := range evals {
		for _, m := range e.FailedTGAllocs {
			var reasons []string
			if n := sumCounts(m.ConstraintFiltered); n > 0 {
				reasons = append(reasons, fmt.Sprintf("constraint filtered (%s)", describeCounts(m.ConstraintFiltered)))
			}
			if n := sumCounts(m.DimensionExhausted); n > 0 {
				reasons = append(reasons, fmt.Sprintf("resources exhausted (%s)", describeCounts(m.DimensionExhausted)))
			}
			if n := sumCounts(m.ClassFiltered); n > 0 {
				reasons = append(reasons, fmt.Sprintf("node class filtered (%s)", describeCounts(m.ClassFiltered)))
			}
			available := sumCounts(m.NodesAvailable)
			if len(reasons) == 0 && available == 0 {
				return fmt.Sprintf("no nodes available in any datacenter (%d nodes evaluated)", m.NodesEvaluated)
			}
			if len(reasons) == 0 {
				continue // this metric didn't explain a filter — keep looking at other task groups/evals
			}
			return fmt.Sprintf("%s — %d/%d nodes available", strings.Join(reasons, ", "), available, m.NodesEvaluated)
		}
		if e.Status == "blocked" && e.StatusDescription != "" {
			return e.StatusDescription
		}
	}
	return ""
}

// sumCounts totals an AllocationMetric count map's values (e.g. how many nodes a constraint
// filtered out, across every distinct constraint description).
func sumCounts(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

// describeCounts renders an AllocationMetric count map as "key=n, key=n", sorted by key for
// deterministic output.
func describeCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// RunReconciler implements Service.RunReconciler.
func (s *service) RunReconciler(ctx context.Context, interval time.Duration) error {
	tick := func() {
		recs := s.store.list()
		for i := range recs {
			if isTerminal(recs[i].Status) {
				continue
			}
			if _, err := s.reconcile(ctx, &recs[i]); err != nil {
				slog.Warn("dispatch: reconcile sweep failed for job", "id", recs[i].ID, "err", err)
			}
		}
	}

	tick()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			tick()
		}
	}
}

// signalName maps common Unix signal numbers to their conventional names; anything else falls
// back to a numeric label rather than guessing.
func signalName(n int) string {
	switch n {
	case 1:
		return "SIGHUP"
	case 2:
		return "SIGINT"
	case 3:
		return "SIGQUIT"
	case 6:
		return "SIGABRT"
	case 9:
		return "SIGKILL"
	case 15:
		return "SIGTERM"
	default:
		return fmt.Sprintf("signal %d", n)
	}
}

// escapeArtifactPath url-escapes each "/"-separated segment of an artifact path individually and
// rejoins with "/", so a path with subdirectories keeps its slashes literal while every other
// character is safely embedded in the download URL.
func escapeArtifactPath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func (s *service) Get(ctx context.Context, id string) (*Job, error) {
	rec, ok := s.store.get(id)
	if !ok {
		return nil, ErrNotFound
	}
	return s.reconcile(ctx, rec)
}

func (s *service) List(ctx context.Context) ([]Job, error) {
	recs := s.store.list()
	out := make([]Job, 0, len(recs))
	for i := range recs {
		job, err := s.reconcile(ctx, &recs[i])
		if err != nil {
			// Don't fail the whole list because one job's refresh failed — fall back to its
			// last known state.
			out = append(out, recs[i].Job)
			continue
		}
		out = append(out, *job)
	}
	return out, nil
}

// allocIDFor resolves the current allocation id for a job, reconciling from Nomad if the store's
// cached AllocID is still empty. Shared by Logs and LogLines.
//
// follow=false does a single check and returns ErrNoAllocationYet immediately if there's still no
// allocation — no waiting. follow=true instead polls (see allocationPollInterval) until an
// allocation appears, the job reaches a terminal status, or ctx is done (a caller wanting a
// bounded wait should derive ctx via context.WithTimeout/WithDeadline before calling — the HTTP
// handler does this for the `?wait=` query), at which point it also returns ErrNoAllocationYet.
func (s *service) allocIDFor(ctx context.Context, id string, follow bool) (string, error) {
	rec, ok := s.store.get(id)
	if !ok {
		return "", ErrNotFound
	}

	allocID, err := s.resolveAllocID(ctx, rec)
	if err != nil {
		return "", err
	}
	if allocID != "" {
		return allocID, nil
	}
	if !follow {
		return "", ErrNoAllocationYet
	}

	ticker := time.NewTicker(s.opts.AllocationPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ErrNoAllocationYet
		case <-ticker.C:
			allocID, err := s.resolveAllocID(ctx, rec)
			if err != nil {
				return "", err
			}
			if allocID != "" {
				return allocID, nil
			}
			if isTerminal(rec.Status) {
				// The job ran its whole life without ever getting an allocation (e.g. it was
				// canceled before Nomad placed it) — nothing will ever appear, so stop waiting.
				return "", ErrNoAllocationYet
			}
		}
	}
}

// resolveAllocID returns rec's current allocation id, forcing a fresh (uncached) reconcile
// against Nomad when the store doesn't already have one. rec is updated in place by reconcile, so
// repeated calls from allocIDFor's poll loop observe the latest known status/AllocID.
func (s *service) resolveAllocID(ctx context.Context, rec *record) (string, error) {
	if rec.AllocID != "" {
		return rec.AllocID, nil
	}
	// Bypass the status cache so a poll loop actually re-queries Nomad every call instead of
	// replaying a stale "still no allocation" result for up to StatusTTL.
	s.mu.Lock()
	delete(s.cache, rec.ID)
	s.mu.Unlock()
	job, err := s.reconcile(ctx, rec)
	if err != nil {
		return "", err
	}
	return job.AllocID, nil
}

// streamContext decides how a Logs/LogLines call should ask Nomad for logs: nomadFollow is what's
// actually passed to nomad.Client.Logs, streamCtx is the context those calls (and any scan
// goroutines reading from them) should use, and cancel (non-nil only when a watcher was started)
// must be invoked once the caller is done reading, to stop that watcher goroutine.
//
// follow=false is untouched (nomadFollow=follow=false, streamCtx=ctx, cancel=nil) — a one-shot
// request never needs any of this. For follow=true it branches on whether id is *already*
// terminal:
//
//   - Already terminal: there's nothing left to "follow" — Nomad's AllocFS Logs API keeps a
//     follow=true stream open past task completion, waiting for frames that will never arrive
//     (this is the root cause of `curl .../logs?follow=1` hanging forever on an already-finished
//     job). So nomadFollow is downgraded to false: Nomad hands back the captured log and closes
//     the stream itself, the same as a plain `grove logs <id>` would get.
//   - Not yet terminal: nomadFollow stays true and watchForTerminal is started on a child context,
//     so that if/when the job goes terminal *while* the stream is open, the stream is severed
//     shortly after (rather than staying open for the caller's full context budget — up to the
//     HTTP handler's 10-minute `?wait=` default).
func (s *service) streamContext(ctx context.Context, id string, follow bool) (streamCtx context.Context, nomadFollow bool, cancel context.CancelFunc) {
	if !follow {
		return ctx, false, nil
	}
	if job, err := s.Get(ctx, id); err == nil && isTerminal(job.Status) {
		return ctx, false, nil
	}
	streamCtx, cancel = context.WithCancel(ctx)
	s.watchForTerminal(streamCtx, cancel, id)
	return streamCtx, true, cancel
}

// watchForTerminal polls id's job status (every Options.LogTerminalPollInterval) for as long as
// streamCtx is alive and, once the job is observed terminal, waits Options.LogStreamGracePeriod
// (giving Nomad a moment to deliver any log frames it already had buffered) and then calls cancel
// — which severs the underlying nomad.Client.Logs stream (see internal/nomad/client.go's Logs:
// it selects on ctx.Done() and closes its pipe with ctx.Err(), which mergedLogReader/LogLines's
// scan loop both treat as "this source is done" rather than a real error, so the caller sees a
// clean end of stream, not a failure).
func (s *service) watchForTerminal(streamCtx context.Context, cancel context.CancelFunc, id string) {
	go func() {
		ticker := time.NewTicker(s.opts.LogTerminalPollInterval)
		defer ticker.Stop()
		for {
			job, err := s.Get(streamCtx, id)
			if err == nil && isTerminal(job.Status) {
				select {
				case <-time.After(s.opts.LogStreamGracePeriod):
				case <-streamCtx.Done():
				}
				cancel()
				return
			}
			select {
			case <-streamCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// cancelOnCloseReader wraps a ReadCloser so that Close also invokes cancel, stopping a
// watchForTerminal goroutine as soon as the caller is done with the stream instead of leaking it
// until the outer ctx's own deadline.
type cancelOnCloseReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnCloseReader) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}

func (s *service) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	allocID, err := s.allocIDFor(ctx, id, follow)
	if err != nil {
		return nil, err
	}
	streamCtx, nomadFollow, cancel := s.streamContext(ctx, id, follow)

	stdout, err := s.nomad.Logs(streamCtx, allocID, "main", "stdout", nomadFollow)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("dispatch: stdout logs: %w", err)
	}
	stderr, err := s.nomad.Logs(streamCtx, allocID, "main", "stderr", nomadFollow)
	if err != nil {
		stdout.Close()
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("dispatch: stderr logs: %w", err)
	}
	rc := io.ReadCloser(newMergedLogReader(stdout, stderr))
	if cancel != nil {
		rc = &cancelOnCloseReader{ReadCloser: rc, cancel: cancel}
	}
	return rc, nil
}

// LogLines streams stdout/stderr as discrete, stream-tagged lines. See the Service interface doc
// for the ordering caveat (best-effort across streams, same as Logs), and streamContext/
// watchForTerminal for how a follow=true stream is kept from hanging past its job's terminal
// status.
func (s *service) LogLines(ctx context.Context, id string, follow bool) (<-chan LogLine, error) {
	allocID, err := s.allocIDFor(ctx, id, follow)
	if err != nil {
		return nil, err
	}
	streamCtx, nomadFollow, cancel := s.streamContext(ctx, id, follow)

	stdout, err := s.nomad.Logs(streamCtx, allocID, "main", "stdout", nomadFollow)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("dispatch: stdout logs: %w", err)
	}
	stderr, err := s.nomad.Logs(streamCtx, allocID, "main", "stderr", nomadFollow)
	if err != nil {
		stdout.Close()
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("dispatch: stderr logs: %w", err)
	}

	out := make(chan LogLine, 64)
	var wg sync.WaitGroup
	wg.Add(2)
	scan := func(r io.ReadCloser, stream string) {
		defer wg.Done()
		defer r.Close()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			select {
			case out <- LogLine{Stream: stream, Line: sc.Text(), Time: time.Now().UTC()}:
			case <-streamCtx.Done():
				return
			}
		}
	}
	go scan(stdout, "stdout")
	go scan(stderr, "stderr")
	go func() {
		wg.Wait()
		close(out)
		if cancel != nil {
			cancel()
		}
	}()
	return out, nil
}

func (s *service) Cancel(ctx context.Context, id string) error {
	rec, ok := s.store.get(id)
	if !ok {
		return ErrNotFound
	}
	if err := s.nomad.StopJob(ctx, rec.NomadJobID, false); err != nil {
		return fmt.Errorf("dispatch: stop job %s: %w", id, err)
	}
	now := time.Now().UTC()
	rec.Status = StatusCanceled
	rec.FinishedAt = &now
	if err := s.store.put(rec); err != nil {
		return fmt.Errorf("dispatch: persist cancel for %s: %w", id, err)
	}
	s.mu.Lock()
	delete(s.cache, id)
	s.mu.Unlock()
	return nil
}

// mergedLogReader concatenates bytes from several readers as they arrive, closing all of them
// (and the pipe) once every source is drained or the reader is closed early.
type mergedLogReader struct {
	pr      *io.PipeReader
	closers []io.Closer
}

func newMergedLogReader(readers ...io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(len(readers))
	for _, r := range readers {
		r := r
		go func() {
			defer wg.Done()
			buf := make([]byte, 32*1024)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					mu.Lock()
					_, werr := pw.Write(buf[:n])
					mu.Unlock()
					if werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		pw.Close()
	}()

	closers := make([]io.Closer, len(readers))
	for i, r := range readers {
		closers[i] = r
	}
	return &mergedLogReader{pr: pr, closers: closers}
}

func (m *mergedLogReader) Read(p []byte) (int, error) { return m.pr.Read(p) }

func (m *mergedLogReader) Close() error {
	for _, c := range m.closers {
		_ = c.Close()
	}
	return m.pr.Close()
}
