// Package apiclient is a small HTTP client for grove's /api/v1 API, shared by the CLI and the
// MCP server so both talk to `grove serve` the same way.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/dispatch"
	"github.com/gm2211/grove/internal/server"
)

// Client talks to a running grove server over HTTP.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a Client pointed at baseURL (e.g. config.Config.Server.URL), authenticating with
// token (config.Config.Server.Token; empty is fine if the server has no token configured).
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) url(path string) string {
	return c.baseURL + "/api/v1" + path
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("grove api: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if respBody == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(respBody)
}

// Fleet returns the normalised fleet view: {workers, vms, nodes}.
func (c *Client) Fleet(ctx context.Context) (*server.FleetResponse, error) {
	var out server.FleetResponse
	if err := c.doJSON(ctx, http.MethodGet, "/fleet", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecycleVM starts a drain-then-delete cycle for the named VM. The server does the actual delete
// asynchronously once the underlying Nomad node is drained (or the deadline passes); this just
// reports whether the drain was accepted.
func (c *Client) RecycleVM(ctx context.Context, name string) (bool, error) {
	var out struct {
		DrainStarted bool `json:"drainStarted"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/vms/"+url.PathEscape(name)+"/recycle", nil, &out); err != nil {
		return false, err
	}
	return out.DrainStarted, nil
}

// PauseWorker cordons a worker (Orchard scheduling pause).
func (c *Client) PauseWorker(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodPost, "/workers/"+url.PathEscape(name)+"/pause", nil, nil)
}

// ResumeWorker un-cordons a worker.
func (c *Client) ResumeWorker(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodPost, "/workers/"+url.PathEscape(name)+"/resume", nil, nil)
}

// SubmitJob dispatches req and returns the new job's id.
func (c *Client) SubmitJob(ctx context.Context, req dispatch.JobRequest) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/jobs", req, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ListJobs lists all known jobs.
func (c *Client) ListJobs(ctx context.Context) ([]dispatch.Job, error) {
	var out []dispatch.Job
	if err := c.doJSON(ctx, http.MethodGet, "/jobs", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetJob fetches one job's current status.
func (c *Client) GetJob(ctx context.Context, id string) (*dispatch.Job, error) {
	var out dispatch.Job
	if err := c.doJSON(ctx, http.MethodGet, "/jobs/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelJob stops a running (or pending) job.
func (c *Client) CancelJob(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/jobs/"+url.PathEscape(id), nil, nil)
}

// JobLogs streams a job's combined stdout+stderr as plain text; follow keeps the connection open
// until the job ends.
func (c *Client) JobLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	path := "/jobs/" + url.PathEscape(id) + "/logs"
	if follow {
		path += "?follow=1"
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("grove api: GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(data)))
	}
	return resp.Body, nil
}

// Healthz reports control-plane health ({"orchard": "ok"/"error: …", "nomad": "ok"/"error: …"}).
func (c *Client) Healthz(ctx context.Context) (map[string]string, error) {
	var out map[string]string
	err := c.doJSON(ctx, http.MethodGet, "/healthz", nil, &out)
	return out, err
}
