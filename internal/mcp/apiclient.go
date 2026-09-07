package mcp

// TODO(mcp): a server agent is concurrently building internal/apiclient, a shared Go client
// for the grove HTTP API. Once that lands, switch this file's callers over to it and delete
// this hand-rolled client. Until then this speaks the "grove HTTP API (v1)" described in
// ARCHITECTURE.md directly so the MCP server isn't blocked on that package landing first.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

// defaultCallTimeout bounds every request this client makes; it's applied via context.WithTimeout
// on top of whatever deadline the caller's context already carries, so a slow or wedged grove
// server can never hang a tool call indefinitely.
const defaultCallTimeout = 20 * time.Second

// logsCallTimeout is a little more generous since log bodies can be large.
const logsCallTimeout = 30 * time.Second

// maxResponseBytes caps how much of a response body we'll ever buffer.
const maxResponseBytes = 8 << 20 // 8MiB

// FleetResponse is the normalised GET /fleet payload: workers (Orchard), VMs (Orchard), and
// Nomad client nodes. Field shapes reuse grove's existing contract types (internal/orchard,
// internal/nomad) since those are what the server-side implementation is built against.
type FleetResponse struct {
	Workers []orchard.Worker `json:"workers"`
	VMs     []orchard.VM     `json:"vms"`
	Nodes   []nomad.Node     `json:"nodes"`
}

// apiClient speaks the grove HTTP API (v1): Authorization: Bearer <token>, JSON in/out, under
// /api/v1. See ARCHITECTURE.md § "grove HTTP API (v1)".
type apiClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// newAPIClient validates baseURL and returns a client. token may be empty (unauthenticated
// deployments), though the grove server is expected to run tailnet-only regardless.
func newAPIClient(baseURL, token string) (*apiClient, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, errors.New("grove server URL is not configured — set server.url in ~/.config/grove/config.yaml (or $GROVE_CONFIG)")
	}
	if _, err := url.Parse(trimmed); err != nil {
		return nil, fmt.Errorf("invalid server.url %q: %w", baseURL, err)
	}
	return &apiClient{baseURL: trimmed, token: token, http: &http.Client{}}, nil
}

type submitJobResponse struct {
	ID string `json:"id"`
}

// Fleet fetches GET /fleet.
func (c *apiClient) Fleet(ctx context.Context) (*FleetResponse, error) {
	var out FleetResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/fleet", defaultCallTimeout, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SubmitJob posts a JobRequest to POST /jobs and returns the assigned job id.
func (c *apiClient) SubmitJob(ctx context.Context, req dispatch.JobRequest) (string, error) {
	var out submitJobResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/jobs", defaultCallTimeout, req, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("grove server accepted the job but the response carried no job id")
	}
	return out.ID, nil
}

// GetJob fetches GET /jobs/{id}.
func (c *apiClient) GetJob(ctx context.Context, id string) (*dispatch.Job, error) {
	var out dispatch.Job
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/jobs/"+url.PathEscape(id), defaultCallTimeout, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListJobs fetches GET /jobs.
func (c *apiClient) ListJobs(ctx context.Context) ([]dispatch.Job, error) {
	var out []dispatch.Job
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/jobs", defaultCallTimeout, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CancelJob issues DELETE /jobs/{id}.
func (c *apiClient) CancelJob(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/jobs/"+url.PathEscape(id), defaultCallTimeout, nil, nil)
}

// JobLogs fetches the current (non-following) log body for a job: GET /jobs/{id}/logs.
func (c *apiClient) JobLogs(ctx context.Context, id string) (string, error) {
	body, err := c.doRaw(ctx, http.MethodGet, "/api/v1/jobs/"+url.PathEscape(id)+"/logs", logsCallTimeout, nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// RecycleVM issues POST /vms/{name}/recycle.
func (c *apiClient) RecycleVM(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/vms/"+url.PathEscape(name)+"/recycle", defaultCallTimeout, nil, nil)
}

// PauseWorker issues POST /workers/{name}/pause or /resume depending on paused.
func (c *apiClient) PauseWorker(ctx context.Context, name string, paused bool) error {
	action := "resume"
	if paused {
		action = "pause"
	}
	return c.doJSON(ctx, http.MethodPost, "/api/v1/workers/"+url.PathEscape(name)+"/"+action, defaultCallTimeout, nil, nil)
}

func (c *apiClient) doJSON(ctx context.Context, method, path string, timeout time.Duration, body, out any) error {
	respBody, err := c.doRaw(ctx, method, path, timeout, body)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", method, path, err)
	}
	return nil
}

func (c *apiClient) doRaw(ctx context.Context, method, path string, timeout time.Duration, body any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s %s: timed out after %s waiting for grove server at %s", method, path, timeout, c.baseURL)
		}
		if looksUnreachable(err) {
			return nil, fmt.Errorf("grove server unreachable at %s — is `grove serve` running and are you on the tailnet? (%w)", c.baseURL, err)
		}
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response from %s %s: %w", method, path, err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("grove server at %s rejected the request (HTTP %d) — check server.token in ~/.config/grove/config.yaml", c.baseURL, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s %s: not found (HTTP 404)", method, path)
	case resp.StatusCode >= 300:
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	return respBody, nil
}

// looksUnreachable heuristically detects connection-level failures (refused, no route, DNS,
// timeout) as opposed to an HTTP-level error response.
func looksUnreachable(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}
