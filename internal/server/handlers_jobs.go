package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/dispatch"
)

// defaultAllocationWait bounds how long a follow=true /logs request waits for a job to get a
// Nomad allocation before giving up (see dispatch.Service.Logs/LogLines and
// dispatch.ErrNoAllocationYet). Overridable per-request via the `?wait=` query
// (time.ParseDuration syntax, e.g. "30s", "5m").
const defaultAllocationWait = 10 * time.Minute

// allocationWaitFor parses the `?wait=` query param (a time.ParseDuration string), falling back to
// defaultAllocationWait when absent or invalid.
func allocationWaitFor(r *http.Request) time.Duration {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return defaultAllocationWait
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultAllocationWait
	}
	return d
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.dispatch.List(r.Context())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	s.writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	var req dispatch.JobRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	job, created, err := s.dispatch.Submit(r.Context(), req)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, map[string]string{"id": job.ID})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.dispatch.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	s.writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.dispatch.Cancel(r.Context(), id); err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query().Get("follow")
	follow := q == "1" || q == "true"

	if strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") {
		s.handleJobLogsNDJSON(w, r, id, follow)
		return
	}

	ctx := r.Context()
	if follow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, allocationWaitFor(r))
		defer cancel()
	}

	rc, err := s.dispatch.Logs(ctx, id, follow)
	if err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, dispatch.ErrNoAllocationYet) {
			// Not yet allocated is a legitimate state for a pending job, not a failure — see
			// dispatch.ErrNoAllocationYet. Report it as an empty, successful stream rather than a
			// 502, so `grove logs`/`grove dispatch --follow` can retry instead of hard-erroring.
			w.Header().Set("X-Grove-Job-Status", "pending")
			w.WriteHeader(http.StatusOK)
			return
		}
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	defer rc.Close()

	var streamErr error
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		streamErr = streamSSE(w, rc)
	} else {
		streamErr = streamChunked(w, rc)
	}
	if streamErr != nil {
		s.log.Warn("job logs stream ended with error", "id", id, "err", streamErr)
	}
}

// handleJobLogsNDJSON streams one JSON object per log line: {offset, ts, stream, line}. offset is
// a purely request-local running byte counter (each request replays the full log from Nomad's own
// retained start, offset 0), so sinceOffset=N skips every line whose offset is below N to support
// simple polling-based resumption without grove needing its own offset-indexed log storage.
func (s *Server) handleJobLogsNDJSON(w http.ResponseWriter, r *http.Request, id string, follow bool) {
	sinceOffset, _ := strconv.ParseInt(r.URL.Query().Get("sinceOffset"), 10, 64)

	ctx := r.Context()
	if follow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, allocationWaitFor(r))
		defer cancel()
	}

	lines, err := s.dispatch.LogLines(ctx, id, follow)
	if err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, dispatch.ErrNoAllocationYet) {
			// See handleJobLogs — same "pending, not a failure" reasoning applies to the NDJSON
			// mode: an empty NDJSON stream with the pending header, not a 502.
			w.Header().Set("X-Grove-Job-Status", "pending")
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			return
		}
		s.writeError(w, http.StatusBadGateway, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, canFlush := w.(http.Flusher)

	enc := json.NewEncoder(w)
	var offset int64
	for ln := range lines {
		lineOffset := offset
		offset += int64(len(ln.Line)) + 1 // +1 for the newline this line occupies in the logical stream
		if lineOffset < sinceOffset {
			continue
		}
		rec := struct {
			Offset int64  `json:"offset"`
			Ts     string `json:"ts"`
			Stream string `json:"stream"`
			Line   string `json:"line"`
		}{Offset: lineOffset, Ts: ln.Time.Format(time.RFC3339Nano), Stream: ln.Stream, Line: ln.Line}
		if err := enc.Encode(rec); err != nil {
			s.log.Warn("ndjson log stream ended with error", "id", id, "err", err)
			return
		}
		if canFlush {
			fl.Flush()
		}
	}
}

// handleGetArtifact streams one artifact a job uploaded, from the server's own artifact-bucket
// credentials — clients never talk to the bucket directly.
func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		http.Error(w, "artifact store is not configured on this grove server", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	raw := r.PathValue("path")
	path, err := url.PathUnescape(raw)
	if err != nil {
		path = raw
	}
	key := "jobs/" + id + "/" + path
	rc, obj, err := s.artifacts.Get(r.Context(), key)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			http.Error(w, "artifact not found", http.StatusNotFound)
			return
		}
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	if _, err := io.Copy(w, rc); err != nil {
		s.log.Warn("artifact download stream ended with error", "id", id, "path", path, "err", err)
	}
}
