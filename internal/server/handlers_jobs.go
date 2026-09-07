package server

import (
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

	rc, err := s.dispatch.Logs(r.Context(), id, follow)
	if err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
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

	lines, err := s.dispatch.LogLines(r.Context(), id, follow)
	if err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
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
