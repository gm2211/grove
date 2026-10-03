package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	pathpkg "path"
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

// redactedEnvValue replaces every Job.Request.Env value the API hands back. The keys stay, so a
// caller (the UI's job detail page, an orchestrator) can still see WHICH variables a job ran with,
// but values are often credentials and the job's submitter already knows them.
//
// internal/dispatch already stores jobs this way; this also covers history written before it did.
const redactedEnvValue = dispatch.RedactedEnvValue

// redactJob returns a copy of job safe to serialize to any principal: Request.Env values are
// replaced with redactedEnvValue. The stored job is never mutated (Env is a shared map).
func redactJob(job dispatch.Job) dispatch.Job {
	if len(job.Request.Env) == 0 {
		return job
	}
	env := make(map[string]string, len(job.Request.Env))
	for k := range job.Request.Env {
		env[k] = redactedEnvValue
	}
	job.Request.Env = env
	return job
}

// canReadJob mirrors handleCancelJob's ownership rule for reads: an operator sees every job, any
// other principal only the jobs it submitted itself. A job with no recorded submitter (dispatched
// before SubmittedBy existed, or with auth disabled) is operator-only.
func canReadJob(p Principal, job dispatch.Job) bool {
	if principalHasScope(p, ScopeOperator) {
		return true
	}
	return job.Request.SubmittedBy != "" && job.Request.SubmittedBy == p.ID
}

// authorizeJobRead enforces canReadJob for the job-scoped read endpoints (logs, artifacts) that
// don't otherwise fetch the job. It writes the error response and returns false when the request
// must stop. Operators skip the lookup entirely, so their reads cost nothing extra.
func (s *Server) authorizeJobRead(w http.ResponseWriter, r *http.Request, id string) bool {
	principal := principalFromContext(r.Context())
	if principalHasScope(principal, ScopeOperator) {
		return true
	}
	job, err := s.dispatch.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return false
		}
		s.writeError(w, http.StatusBadGateway, err)
		return false
	}
	if !canReadJob(principal, *job) {
		http.Error(w, "only the submitting device or an operator can read this job", http.StatusForbidden)
		return false
	}
	return true
}

// handleListJobs lists jobs, filtered to the caller's own submissions unless it is an operator,
// with env values redacted (see redactJob).
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.dispatch.List(r.Context())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	principal := principalFromContext(r.Context())
	out := make([]dispatch.Job, 0, len(jobs))
	for _, job := range jobs {
		if canReadJob(principal, job) {
			out = append(out, redactJob(job))
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	var req dispatch.JobRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	principal := principalFromContext(r.Context())
	if !principalCanDispatchKind(principal, req.Kind) {
		http.Error(w, "device credential cannot dispatch this job kind", http.StatusForbidden)
		return
	}
	if len(req.Secrets) > 0 && !principalHasScope(principal, ScopeOperator) {
		http.Error(w, "named secrets require operator dispatch", http.StatusForbidden)
		return
	}
	req.SubmittedBy = principal.ID
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
	if !canReadJob(principalFromContext(r.Context()), *job) {
		http.Error(w, "only the submitting device or an operator can read this job", http.StatusForbidden)
		return
	}
	s.writeJSON(w, http.StatusOK, redactJob(*job))
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	principal := principalFromContext(r.Context())
	if !principalHasScope(principal, ScopeOperator) {
		job, err := s.dispatch.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, dispatch.ErrNotFound) {
				http.Error(w, "job not found", http.StatusNotFound)
				return
			}
			s.writeError(w, http.StatusBadGateway, err)
			return
		}
		if job.Request.SubmittedBy == "" || job.Request.SubmittedBy != principal.ID {
			http.Error(w, "only the submitting device or an operator can cancel this job", http.StatusForbidden)
			return
		}
	}
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
	if !s.authorizeJobRead(w, r, id) {
		return
	}
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

	// Best-effort: report the job's current status via the same header the pending-branch above
	// uses, so a client (or a curious operator) can see e.g. "running"/"failed" without a second
	// request. A failure to fetch it just means the header is omitted — never worth failing the
	// whole logs request over.
	if job, jerr := s.dispatch.Get(r.Context(), id); jerr == nil && job.Status != "" {
		w.Header().Set("X-Grove-Job-Status", string(job.Status))
	}

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

	if job, jerr := s.dispatch.Get(r.Context(), id); jerr == nil && job.Status != "" {
		w.Header().Set("X-Grove-Job-Status", string(job.Status))
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, canFlush := w.(http.Flusher)
	// See streamChunked's identical comment — flush the headers immediately rather than letting
	// them sit buffered until the first log line (which, for follow=1, may be a long time coming).
	if canFlush {
		fl.Flush()
	}

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
	// Both the id and the path end up inside an object-store key, which the store takes verbatim:
	// "jobs/<id>/../<other-id>/x" would read another job's artifact past the ownership check below.
	// Validate after decoding, so %2e%2e (and a double-encoded %252e%252e) is caught too.
	if !safeKeySegment(id) || !safeArtifactPath(raw) || !safeArtifactPath(path) {
		http.Error(w, "invalid artifact path", http.StatusBadRequest)
		return
	}
	if !s.authorizeJobRead(w, r, id) {
		return
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
	// Artifacts are job output — attacker-influenced bytes. Never let a browser sniff or render
	// them inline on grove's origin (where the UI's credential lives); always download.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": pathpkg.Base(path)}))
	if _, err := io.Copy(w, rc); err != nil {
		s.log.Warn("artifact download stream ended with error", "id", id, "path", path, "err", err)
	}
}

// safeKeySegment reports whether s can stand as one object-store key segment: non-empty, no
// slash or backslash, and not a dot segment.
func safeKeySegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\")
}

// safeArtifactPath reports whether p is a relative artifact path with no "." or ".." segment,
// no leading "/", and no empty segment (so "a//b" can't alias "a/b" in the store).
func safeArtifactPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
