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
	return &service{nomad: nc, artifacts: opts.Artifacts, opts: opts, store: st, cache: map[string]cachedStatus{}}, nil
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
	if req.Timeout > 0 {
		meta["timeout_seconds"] = strconv.FormatFloat(req.Timeout.Seconds(), 'f', 0, 64)
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
		return nil, fmt.Errorf("dispatch: list allocations for %s: %w", rec.ID, err)
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
func (s *service) allocIDFor(ctx context.Context, id string) (string, error) {
	rec, ok := s.store.get(id)
	if !ok {
		return "", ErrNotFound
	}
	allocID := rec.AllocID
	if allocID == "" {
		job, err := s.reconcile(ctx, rec)
		if err != nil {
			return "", err
		}
		allocID = job.AllocID
	}
	if allocID == "" {
		return "", fmt.Errorf("dispatch: job %s has no allocation yet", id)
	}
	return allocID, nil
}

func (s *service) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	allocID, err := s.allocIDFor(ctx, id)
	if err != nil {
		return nil, err
	}

	stdout, err := s.nomad.Logs(ctx, allocID, "main", "stdout", follow)
	if err != nil {
		return nil, fmt.Errorf("dispatch: stdout logs: %w", err)
	}
	stderr, err := s.nomad.Logs(ctx, allocID, "main", "stderr", follow)
	if err != nil {
		stdout.Close()
		return nil, fmt.Errorf("dispatch: stderr logs: %w", err)
	}
	return newMergedLogReader(stdout, stderr), nil
}

// LogLines streams stdout/stderr as discrete, stream-tagged lines. See the Service interface doc
// for the ordering caveat (best-effort across streams, same as Logs).
func (s *service) LogLines(ctx context.Context, id string, follow bool) (<-chan LogLine, error) {
	allocID, err := s.allocIDFor(ctx, id)
	if err != nil {
		return nil, err
	}

	stdout, err := s.nomad.Logs(ctx, allocID, "main", "stdout", follow)
	if err != nil {
		return nil, fmt.Errorf("dispatch: stdout logs: %w", err)
	}
	stderr, err := s.nomad.Logs(ctx, allocID, "main", "stderr", follow)
	if err != nil {
		stdout.Close()
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
			case <-ctx.Done():
				return
			}
		}
	}
	go scan(stdout, "stdout")
	go scan(stderr, "stderr")
	go func() { wg.Wait(); close(out) }()
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
