package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/config"
)

// ErrNotFound is returned by Get/Cancel/Logs when no job with the given id is known.
var ErrNotFound = errors.New("dispatch: job not found")

// ErrNoAllocationYet is returned by Logs/LogLines for a job that is still queued and so has no
// output yet: with follow=false immediately, with follow=true only once ctx is done before the job
// started. (The name dates from when jobs ran on Nomad allocations.) The HTTP server handler
// treats it as "200, empty body, X-Grove-Job-Status: pending" rather than an error response — see
// internal/server/handlers_jobs.go.
var ErrNoAllocationYet = errors.New("dispatch: job has not started yet")

// DefaultReconcileInterval is the tick interval `grove serve` passes to RunReconciler.
const DefaultReconcileInterval = 30 * time.Second

const (
	defaultLogPollInterval = 250 * time.Millisecond
	// defaultTimeoutBackstop is how long past its own timeout a started job may run before the
	// service stops it itself. The executor normally enforces the timeout (exit 124) well before.
	defaultTimeoutBackstop = 15 * time.Minute
	// defaultJobTimeout applies to a job that sets none (internal/sandbox DefaultTimeout).
	defaultJobTimeout = time.Hour
	// abandonTimeout bounds cleaning up after one job lost to a restart.
	abandonTimeout = 2 * time.Minute
)

// Options configures a dispatch Service.
type Options struct {
	// Deprecated: ArtifactsBase named the S3 bucket artifact paths were relative to, back when
	// artifacts lived in one. Nothing reads it; the store is the directory Artifacts is rooted at.
	ArtifactsBase string
	// Artifacts, when non-nil, is used to populate Job.Artifacts once a job reaches a terminal
	// status. Nil means "no artifact store configured" — Job.Artifacts is just never populated.
	Artifacts artifacts.Client
	// ArtifactRetention is how long a finished job's artifacts AND its log file are kept (see
	// config.ArtifactRetention); zero values take the defaults. RunReconciler deletes them once
	// expired (artifacts only when Artifacts also implements artifacts.Deleter).
	ArtifactRetention config.ArtifactRetention
	// StorePath overrides the on-disk job history location. Empty uses
	// $XDG_STATE_HOME/grove/jobs.json (default ~/.local/state/grove/jobs.json). The queue spool
	// (queue/) and, unless LogDir says otherwise, the job logs (logs/) live next to it.
	StorePath string
	// LogDir overrides where job log files are kept (default: logs/ next to the job history).
	LogDir string
	// RepoAuth, when non-nil, is consulted when a build/agent job starts: if the control plane
	// has a credential armed for its Repo (see internal/gitauth), the token is handed to the
	// executor as Run.CloneToken, which uses it for the control plane's own clone and nowhere
	// else. Nil — the default — means no job ever gets clone credentials from grove.
	//
	// The token is never stored: not on Job.Request, not in the job history or queue spool, not
	// in a log, not in any API response; Job.RepoCredentialUsed records that one was used.
	RepoAuth RepoAuth
	// Pools are the fleet.yaml pools jobs can be submitted to; Submit rejects any other pool. A
	// pool's CPU/Memory also bound JobRequest.Resources hints (zero means no bound).
	Pools []PoolConfig
	// LogPollInterval is how often a follow=true log stream looks for new output (default 250ms).
	LogPollInterval time.Duration
	// TimeoutBackstop is how long past its timeout a started job may run before the service stops
	// it itself and reports it timed out (default 15m) — a guard against a stuck executor.
	TimeoutBackstop time.Duration
}

type service struct {
	exec      Executor
	artifacts artifacts.Client
	opts      Options
	store     *store
	spool     spool
	logDir    string
	pools     map[string]PoolConfig

	// ctx is the parent of every execution's context; stop ends them all (Close).
	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	// mu guards everything below, and serializes every change of a job's status in the store.
	mu sync.Mutex
	// queues holds each pool's queued job ids, oldest first.
	queues map[string][]string
	// envs holds queued jobs' real env values (the store has them redacted).
	envs map[string]map[string]string
	// acquiring is, per pool, the job whose executor is looking for a free VM slot. One job per
	// pool at a time, so jobs start in submission order and only one of them polls Orchard.
	acquiring map[string]string
	running   map[string]*execution
	// started is false until restart recovery is done; queues don't move before.
	started bool
	closed  bool
}

// execution is one in-flight job.
type execution struct {
	pool     string
	cancel   context.CancelFunc
	backstop *time.Timer
	env      map[string]string
	placed   bool
	canceled bool // by Cancel
	timedOut bool // by the backstop
}

// New builds the dispatch Service: it opens (or creates) the job history, recovers from a previous
// `grove serve` (see recover), and starts running queued jobs on exec.
func New(exec Executor, opts Options) (Service, error) {
	if exec == nil {
		return nil, fmt.Errorf("dispatch: an executor is required")
	}
	path := opts.StorePath
	if path == "" {
		path = defaultStorePath()
	}
	st, err := newStore(path)
	if err != nil {
		return nil, fmt.Errorf("dispatch: open store %s: %w", path, err)
	}
	opts.ArtifactRetention = opts.ArtifactRetention.WithDefaults()
	if opts.LogPollInterval <= 0 {
		opts.LogPollInterval = defaultLogPollInterval
	}
	if opts.TimeoutBackstop <= 0 {
		opts.TimeoutBackstop = defaultTimeoutBackstop
	}
	logDir := opts.LogDir
	q := spool{}
	if path != "" {
		if logDir == "" {
			logDir = filepath.Join(filepath.Dir(path), "logs")
		}
		q.dir = filepath.Join(filepath.Dir(path), "queue")
	}
	if logDir == "" {
		if logDir, err = os.MkdirTemp("", "grove-logs-"); err != nil {
			return nil, fmt.Errorf("dispatch: log directory: %w", err)
		}
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("dispatch: log directory: %w", err)
	}
	pools := make(map[string]PoolConfig, len(opts.Pools))
	for _, p := range opts.Pools {
		pools[p.Name] = p
	}
	ctx, stop := context.WithCancel(context.Background())
	s := &service{
		exec: exec, artifacts: opts.Artifacts, opts: opts, store: st, spool: q, logDir: logDir, pools: pools,
		ctx: ctx, stop: stop,
		queues: map[string][]string{}, envs: map[string]map[string]string{},
		acquiring: map[string]string{}, running: map[string]*execution{},
	}
	if err := s.recover(); err != nil {
		stop()
		return nil, err
	}
	return s, nil
}

// recover reconciles the job history after a restart. Jobs ran inside the previous `grove serve`
// process, so none of them is running any more:
//
//   - a job that had started (StatusRunning, or placed in a VM) is marked lost, and its VM is
//     handed to Executor.Abandon (the sandbox executor deletes it; the fleet reconciler recreates
//     it) so nothing it left keeps running unsupervised. Re-attaching instead would need the
//     executor's in-memory supervision state (log offsets, the VM slot lock) to survive the
//     process, and a restart is rare enough that a clean "lost, retry it" is the better trade.
//   - a job still queued is queued again, in submission order, if its queue spool entry (its env
//     values) survived; otherwise it is marked lost.
//   - a job dispatched to Nomad by an older grove is marked lost: nothing runs it any more.
//
// The queues start moving once every abandoned VM has been dealt with, so a requeued job can't
// land in a VM that is about to be deleted.
func (s *service) recover() error {
	var abandon []Placement
	for _, rec := range s.store.list() {
		if isTerminal(rec.Status) {
			continue
		}
		rec := rec
		switch {
		case rec.LegacyNomadJobID != "":
			s.finishLost(&rec, "dispatched to Nomad by an older grove, which no longer uses Nomad")
		case rec.Status == StatusRunning || (rec.Placement != nil && rec.Placement.VMID != ""):
			s.finishLost(&rec, "grove serve restarted while the job was running; its sealed VM is recycled")
			if rec.Placement != nil && rec.Placement.VMID != "" {
				abandon = append(abandon, *rec.Placement)
			}
		default:
			entry, ok := s.spool.get(rec.ID)
			if _, known := s.pools[rec.Request.Pool]; !ok || !known {
				s.finishLost(&rec, "grove serve restarted and could not requeue the job")
				continue
			}
			s.queues[rec.Request.Pool] = append(s.queues[rec.Request.Pool], rec.ID)
			s.envs[rec.ID] = entry.Env
		}
	}
	if err := s.persistErr(); err != nil {
		return err
	}
	if len(abandon) == 0 {
		s.mu.Lock()
		s.startLocked()
		s.mu.Unlock()
		return nil
	}
	go func() {
		for _, p := range abandon {
			ctx, cancel := context.WithTimeout(s.ctx, abandonTimeout)
			if err := s.exec.Abandon(ctx, p); err != nil {
				slog.Warn("dispatch: cleaning up after a job lost to a restart failed", "vm", p.VMID, "err", err)
			}
			cancel()
		}
		s.mu.Lock()
		s.startLocked()
		s.mu.Unlock()
	}()
	return nil
}

// persistErr reports a store that can't be written, which recover's per-job writes only log.
func (s *service) persistErr() error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if err := s.store.persistLocked(); err != nil {
		return fmt.Errorf("dispatch: persist job history: %w", err)
	}
	return nil
}

// finishLost marks rec lost with reason and persists it.
func (s *service) finishLost(rec *record, reason string) {
	now := time.Now().UTC()
	rec.Status = StatusLost
	rec.FailureReason = &reason
	rec.FinishedAt = &now
	rec.PendingReason = ""
	s.spool.remove(rec.ID)
	if err := s.store.put(rec); err != nil {
		slog.Warn("dispatch: persist job failed", "id", rec.ID, "err", err)
	}
}

func (s *service) startLocked() {
	s.started = true
	for _, pool := range slices.Sorted(func(yield func(string) bool) {
		for p := range s.queues {
			if !yield(p) {
				return
			}
		}
	}) {
		s.pumpLocked(pool)
	}
}

// Close stops every running job's execution (without recording an end: to the next New they look
// like jobs a crash interrupted) and waits for the executors to return. Used by tests.
func (s *service) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.stop()
	s.wg.Wait()
}

// pumpLocked starts pool's next queued job, unless one of its jobs is still looking for a slot.
func (s *service) pumpLocked(pool string) {
	if !s.started || s.closed || s.acquiring[pool] != "" || len(s.queues[pool]) == 0 {
		return
	}
	id := s.queues[pool][0]
	s.queues[pool] = s.queues[pool][1:]
	s.acquiring[pool] = id
	ctx, cancel := context.WithCancel(s.ctx)
	ex := &execution{pool: pool, cancel: cancel, env: s.envs[id]}
	delete(s.envs, id)
	s.running[id] = ex
	s.wg.Add(1)
	go s.execute(ctx, id, ex)
}

// releaseLocked lets pool's next job look for a slot once id has one (or is over).
func (s *service) releaseLocked(pool, id string) {
	if s.acquiring[pool] == id {
		delete(s.acquiring, pool)
		s.pumpLocked(pool)
	}
}

// execute runs one job on the executor and records how it ended.
func (s *service) execute(ctx context.Context, id string, ex *execution) {
	defer s.wg.Done()
	defer ex.cancel()

	rec, ok := s.store.get(id)
	if !ok {
		s.mu.Lock()
		delete(s.running, id)
		s.releaseLocked(ex.pool, id)
		s.mu.Unlock()
		return
	}
	req := rec.Request
	req.Env = ex.env
	token, used := s.repoCredential(req)

	lf, err := openLogFile(s.logPath(id))
	if err != nil {
		slog.Warn("dispatch: could not open a job's log file", "id", id, "err", err)
		s.finish(id, ex, ExitLost, "could not open the job's log file on the control plane: "+err.Error())
		return
	}
	if used {
		s.mu.Lock()
		if cur, ok := s.store.get(id); ok {
			cur.RepoCredentialUsed = true
			_ = s.store.put(cur)
		}
		s.mu.Unlock()
	}

	run := &Run{
		ID: id, Request: req, Pool: s.pools[req.Pool], CloneToken: token,
		ArtifactPrefix: artifactPrefix(id), Stdout: lf.stdout, Stderr: lf.stderr,
	}
	run.onStart = func(p Placement) { s.markStarted(id, ex, p, req.Timeout.Duration()) }
	code := s.exec.Execute(ctx, run)
	_ = lf.Close()
	s.finish(id, ex, code, "")
}

// markStarted records that job id now runs at p, and lets its pool's next job go.
func (s *service) markStarted(id string, ex *execution, p Placement, timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ex.placed = true
	s.spool.remove(id)
	if rec, ok := s.store.get(id); ok && !isTerminal(rec.Status) {
		now := time.Now().UTC()
		rec.Status = StatusRunning
		rec.StartedAt = &now
		rec.Node = p.VMID
		rec.Placement = &p
		rec.PendingReason = ""
		if err := s.store.put(rec); err != nil {
			slog.Warn("dispatch: persist job failed", "id", id, "err", err)
		}
	}
	ex.backstop = time.AfterFunc(timeout+s.opts.TimeoutBackstop, func() {
		s.mu.Lock()
		ex.timedOut = true
		s.mu.Unlock()
		ex.cancel()
	})
	s.releaseLocked(ex.pool, id)
}

// finish records how job id ended: code is what the executor returned, reason (if any) why grove
// itself failed it.
func (s *service) finish(id string, ex *execution, code int, reason string) {
	arts := s.listArtifacts(id)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
	if ex.backstop != nil {
		ex.backstop.Stop()
	}
	defer s.releaseLocked(ex.pool, id)

	rec, ok := s.store.get(id)
	if !ok || (s.closed && !ex.canceled) {
		// Shutting down: leave the record as it is, for the next start's recover.
		return
	}
	if !ex.placed && code == ExitTempFail && !ex.canceled && rec.TempFailRetries < maxTempFailRetries {
		rec.TempFailRetries++
		if err := s.store.put(rec); err != nil {
			slog.Warn("dispatch: persist job failed", "id", id, "err", err)
		}
		s.queues[ex.pool] = append([]string{id}, s.queues[ex.pool]...)
		s.envs[id] = ex.env
		slog.Info("dispatch: requeued a job that couldn't start yet", "id", id, "attempt", rec.TempFailRetries)
		return
	}

	now := time.Now().UTC()
	switch {
	case ex.canceled:
		code = ExitCanceled
		rec.Status = StatusCanceled
	case ex.timedOut || code == ExitTimedOut:
		code = ExitTimedOut
		rec.Status = StatusFailed
		rec.TimedOut = true
	case code == ExitLost:
		rec.Status = StatusLost
		if reason == "" {
			reason = "the sealed VM running the job stopped answering, or none of the pool's sealed VMs came up (see the job's log)"
		}
	case code == 0:
		rec.Status = StatusSuccess
	default:
		rec.Status = StatusFailed
	}
	rec.ExitCode = &code
	if code > 128 && code < 160 && !rec.TimedOut {
		name := signalName(code - 128)
		rec.Signal = &name
	}
	if reason != "" {
		rec.FailureReason = &reason
	}
	if rec.FinishedAt == nil {
		rec.FinishedAt = &now
	}
	rec.PendingReason = ""
	if len(arts) > 0 {
		rec.Artifacts = arts
	}
	s.spool.remove(id)
	if err := s.store.put(rec); err != nil {
		slog.Warn("dispatch: persist job failed", "id", id, "err", err)
	}
}

// listArtifacts lists job id's artifacts in the store, best-effort.
func (s *service) listArtifacts(id string) []Artifact {
	if s.artifacts == nil {
		return nil
	}
	objs, err := s.artifacts.List(s.ctx, artifactPrefix(id))
	if err != nil {
		slog.Warn("dispatch: listing artifacts failed", "id", id, "err", err)
		return nil
	}
	arts := make([]Artifact, 0, len(objs))
	for _, obj := range objs {
		arts = append(arts, Artifact{
			Path:        obj.Path,
			URL:         "/api/v1/jobs/" + url.PathEscape(id) + "/artifacts/" + escapeArtifactPath(obj.Path),
			Size:        obj.Size,
			ContentType: obj.ContentType,
		})
	}
	return arts
}

func (s *service) Submit(ctx context.Context, req JobRequest) (*Job, bool, error) {
	if err := validate(req); err != nil {
		return nil, false, err
	}
	if _, ok := s.pools[req.Pool]; !ok {
		return nil, false, fmt.Errorf("dispatch: unknown pool %q (fleet.yaml pools: %s)", req.Pool, strings.Join(s.poolNames(), ", "))
	}
	if err := s.validateResources(req); err != nil {
		return nil, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if req.IdempotencyKey != "" {
		if rec, ok := s.store.findByIdempotencyKey(req.IdempotencyKey); ok {
			job := s.viewLocked(rec)
			return &job, false, nil
		}
	}

	id, err := randomID()
	if err != nil {
		return nil, false, err
	}
	rec := &record{Job: Job{
		ID:          id,
		Request:     withRedactedEnv(req),
		Status:      StatusPending,
		SubmittedAt: time.Now().UTC(),
		Meta:        req.Meta,
	}}
	if err := s.spool.put(id, queueEntry{Env: req.Env}); err != nil {
		return nil, false, fmt.Errorf("dispatch: queue job %s: %w", id, err)
	}
	if err := s.store.put(rec); err != nil {
		s.spool.remove(id)
		return nil, false, fmt.Errorf("dispatch: persist job %s: %w", id, err)
	}
	env := make(map[string]string, len(req.Env))
	for k, v := range req.Env {
		env[k] = v
	}
	s.queues[req.Pool] = append(s.queues[req.Pool], id)
	s.envs[id] = env
	s.pumpLocked(req.Pool)
	job := s.viewLocked(rec)
	return &job, true, nil
}

func (s *service) poolNames() []string {
	names := make([]string, 0, len(s.pools))
	for name := range s.pools {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

// viewLocked is rec as callers see it: with a PendingReason while it waits.
func (s *service) viewLocked(rec *record) Job {
	job := rec.Job
	if job.Status != StatusPending {
		return job
	}
	pool := job.Request.Pool
	switch {
	case s.acquiring[pool] == job.ID:
		job.PendingReason = fmt.Sprintf("waiting for a free slot in one of pool %s's sealed VMs", pool)
	case !s.started:
		job.PendingReason = "queued; grove serve is still recovering from a restart"
	default:
		if i := slices.Index(s.queues[pool], job.ID); i >= 0 {
			job.PendingReason = fmt.Sprintf("queued behind %d job(s) of pool %s", i+1, pool)
		}
	}
	return job
}

func (s *service) Get(_ context.Context, id string) (*Job, error) {
	rec, ok := s.store.get(id)
	if !ok {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	job := s.viewLocked(rec)
	s.mu.Unlock()
	return &job, nil
}

func (s *service) List(_ context.Context) ([]Job, error) {
	recs := s.store.list()
	out := make([]Job, 0, len(recs))
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range recs {
		out = append(out, s.viewLocked(&recs[i]))
	}
	return out, nil
}

// Cancel stops a running job (its executor stops it in the VM; it ends StatusCanceled, exit 143)
// or takes a queued one out of its queue. Canceling a finished job changes nothing.
func (s *service) Cancel(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.store.get(id)
	if !ok {
		return ErrNotFound
	}
	if isTerminal(rec.Status) {
		return nil
	}
	if ex, ok := s.running[id]; ok {
		ex.canceled = true
		ex.cancel()
	} else {
		pool := rec.Request.Pool
		if i := slices.Index(s.queues[pool], id); i >= 0 {
			s.queues[pool] = slices.Delete(s.queues[pool], i, i+1)
		}
		delete(s.envs, id)
		s.spool.remove(id)
	}
	now := time.Now().UTC()
	rec.Status = StatusCanceled
	rec.FinishedAt = &now
	rec.PendingReason = ""
	if err := s.store.put(rec); err != nil {
		return fmt.Errorf("dispatch: persist cancel for %s: %w", id, err)
	}
	return nil
}

// RunReconciler implements Service.RunReconciler.
func (s *service) RunReconciler(ctx context.Context, interval time.Duration) error {
	tick := func() {
		now := time.Now().UTC()
		s.expireArtifacts(ctx, now)
		s.expireLogs(now)
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

// envKeyPattern is what a JobRequest.Env key must look like: the job's env.sh turns each one into a bash
// `export KEY=...`, so anything else would be shell syntax rather than a name.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	if req.Repo != "" && !strings.HasPrefix(req.Repo, "https://") {
		return fmt.Errorf("dispatch: repo must be an https:// URL")
	}
	if strings.ContainsAny(req.Repo, "\n\r") || strings.ContainsAny(req.Ref, "\n\r") {
		return fmt.Errorf("dispatch: repo and ref must be a single line")
	}
	if strings.HasPrefix(req.Ref, "-") {
		return fmt.Errorf("dispatch: ref must not start with '-'")
	}
	// Nothing resolves named secrets yet; accepting them would let a caller believe a secret
	// reached the job when it silently didn't.
	if len(req.Secrets) > 0 {
		return fmt.Errorf("dispatch: named secrets are not supported; nothing would resolve them")
	}
	for key := range req.Env {
		if !envKeyPattern.MatchString(key) {
			return fmt.Errorf("dispatch: env key %q is not a valid shell variable name", key)
		}
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

// RedactedEnvValue replaces every env value in the job history and in every Job handed back: the
// values reach only the job itself and are often credentials, so the record keeps only
// which variables a job ran with.
const RedactedEnvValue = "[redacted]"

// withRedactedEnv returns req with a fresh Env whose values are RedactedEnvValue, leaving the
// caller's map untouched.
func withRedactedEnv(req JobRequest) JobRequest {
	if len(req.Env) == 0 {
		return req
	}
	env := make(map[string]string, len(req.Env))
	for k := range req.Env {
		env[k] = RedactedEnvValue
	}
	req.Env = env
	return req
}

// repoCredential returns the control plane's armed private-repo credential for this job's
// repository, and whether there is one. Only build and agent jobs clone, so only they get one.
//
// A caller that set GH_TOKEN or GIT_TOKEN in its own env gets none: an explicit per-job credential
// is more specific than the fleet-wide armed one, and the control plane clones with the caller's
// value instead (see internal/sandbox cloneToken).
func (s *service) repoCredential(req JobRequest) (string, bool) {
	if s.opts.RepoAuth == nil || req.Repo == "" {
		return "", false
	}
	if req.Kind != KindBuild && req.Kind != KindAgent {
		return "", false
	}
	if req.Env["GH_TOKEN"] != "" || req.Env["GIT_TOKEN"] != "" {
		return "", false
	}
	token, ok := s.opts.RepoAuth.TokenFor(req.Repo)
	if !ok || token == "" || strings.ContainsAny(token, "\n\r") {
		return "", false
	}
	return token, true
}

func isTerminal(status Status) bool {
	switch status {
	case StatusSuccess, StatusFailed, StatusCanceled, StatusLost:
		return true
	default:
		return false
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
