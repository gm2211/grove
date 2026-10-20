package dispatch

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
)

// expireArtifacts deletes the artifacts of every finished job whose retention has run out:
// opts.ArtifactRetention.Success after a job succeeded, .Failure after any other end. It works
// from the store's contents rather than the job history, so artifacts whose job grove no longer
// knows about (history removed, or uploaded by hand under jobs/) expire too, Failure after they
// were last written. A failed listing or delete is logged and retried on the next sweep.
func (s *service) expireArtifacts(ctx context.Context, now time.Time) {
	deleter, ok := s.artifacts.(artifacts.Deleter)
	if !ok || s.artifacts == nil {
		return
	}
	const root = "jobs/"
	objs, err := s.artifacts.List(ctx, root)
	if err != nil {
		slog.Warn("dispatch: listing artifacts for expiry failed", "err", err)
		return
	}

	// Newest write per job id (the first path segment under jobs/).
	newest := map[string]time.Time{}
	for _, obj := range objs {
		id, _, found := strings.Cut(obj.Path, "/")
		if !found || id == "" {
			continue
		}
		if obj.ModTime.After(newest[id]) {
			newest[id] = obj.ModTime
		}
	}

	retention := s.opts.ArtifactRetention.WithDefaults()
	for id, written := range newest {
		rec, known := s.store.get(id)
		var deadline time.Time
		switch {
		case !known:
			deadline = written.Add(retention.Failure)
		case !isTerminal(rec.Status):
			continue // still running: its artifacts may still be arriving
		default:
			finished := written
			if rec.FinishedAt != nil && rec.FinishedAt.After(finished) {
				finished = *rec.FinishedAt
			}
			keep := retention.Failure
			if rec.Status == StatusSuccess {
				keep = retention.Success
			}
			deadline = finished.Add(keep)
		}
		if now.Before(deadline) {
			continue
		}

		if err := deleter.DeletePrefix(ctx, artifactPrefix(id)); err != nil {
			slog.Warn("dispatch: expiring artifacts failed", "id", id, "err", err)
			continue
		}
		slog.Info("dispatch: expired artifacts", "id", id)
		if !known {
			continue
		}
		// Re-read: the record may have changed while the store was being swept.
		if rec, ok := s.store.get(id); ok {
			at := now
			rec.Artifacts = nil
			rec.ArtifactsExpiredAt = &at
			if err := s.store.put(rec); err != nil {
				slog.Warn("dispatch: recording artifact expiry failed", "id", id, "err", err)
			}
		}
	}
}

// expireLogs deletes the log file of every finished job whose retention has run out, under the
// same policy as artifacts: opts.ArtifactRetention.Success after a job succeeded, .Failure after
// any other end, counted from when it finished. A log file grove has no job for expires Failure
// after it was last written.
func (s *service) expireLogs(now time.Time) {
	entries, err := os.ReadDir(s.logDir)
	if err != nil {
		slog.Warn("dispatch: listing job logs for expiry failed", "err", err)
		return
	}
	retention := s.opts.ArtifactRetention.WithDefaults()
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".log")
		if !ok || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rec, known := s.store.get(id)
		var deadline time.Time
		switch {
		case !known:
			deadline = info.ModTime().Add(retention.Failure)
		case !isTerminal(rec.Status):
			continue
		default:
			finished := info.ModTime()
			if rec.FinishedAt != nil && rec.FinishedAt.After(finished) {
				finished = *rec.FinishedAt
			}
			keep := retention.Failure
			if rec.Status == StatusSuccess {
				keep = retention.Success
			}
			deadline = finished.Add(keep)
		}
		if now.Before(deadline) {
			continue
		}
		if err := os.Remove(s.logPath(id)); err != nil && !os.IsNotExist(err) {
			slog.Warn("dispatch: expiring a job log failed", "id", id, "err", err)
			continue
		}
		if !known {
			continue
		}
		s.mu.Lock()
		if rec, ok := s.store.get(id); ok {
			at := now
			rec.LogsExpiredAt = &at
			if err := s.store.put(rec); err != nil {
				slog.Warn("dispatch: recording log expiry failed", "id", id, "err", err)
			}
		}
		s.mu.Unlock()
	}
}
