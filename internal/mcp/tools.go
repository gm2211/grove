package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/dispatch"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// handlers holds the dependencies every tool/resource/prompt handler needs. Keeping them as
// methods on a small struct (rather than closures) makes them directly callable from tests
// without spinning up a real *mcpsdk.Server or stdio transport.
type handlers struct {
	client *apiclient.Client
}

// --- grove_fleet -------------------------------------------------------------------------

type fleetArgs struct{}

func (h *handlers) fleet(ctx context.Context, _ *mcpsdk.CallToolRequest, _ fleetArgs) (*mcpsdk.CallToolResult, *FleetSummary, error) {
	fleet, err := h.client.Fleet(ctx)
	if err != nil {
		return nil, nil, err
	}
	jobs, jerr := h.client.ListJobs(ctx)
	if jerr != nil {
		jobs = nil // fall back to node alloc counts; fleet is still useful without job data
	}
	return nil, summarizeFleet(fleet, jobs), nil
}

// --- grove_run -----------------------------------------------------------------------------

type runArgs struct {
	Pool           string            `json:"pool" jsonschema:"pool to run on, e.g. linux or macos (see grove_fleet for available pools)"`
	Script         string            `json:"script" jsonschema:"bash script to run with bash -eo pipefail"`
	Repo           string            `json:"repo,omitempty" jsonschema:"git URL to clone before running the script; omit for a bare shell job"`
	Ref            string            `json:"ref,omitempty" jsonschema:"commit SHA, branch, or tag to check out (used with repo)"`
	Env            map[string]string `json:"env,omitempty" jsonschema:"environment variables to inject into the job"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"job timeout in seconds; 0 uses the server default"`
	Kind           string            `json:"kind,omitempty" jsonschema:"job kind: shell (default), build, or agent"`
	Wait           *bool             `json:"wait,omitempty" jsonschema:"poll until the job reaches a terminal state and return its result; default true"`
	TailLines      int               `json:"tail_lines,omitempty" jsonschema:"trailing log lines to include in the result when wait is true; default 200"`
}

type runResult struct {
	JobID           string  `json:"job_id"`
	Status          string  `json:"status,omitempty"`
	ExitCode        *int    `json:"exit_code,omitempty"`
	Node            string  `json:"node,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	LogTail         string  `json:"log_tail,omitempty"`
	TimedOutWaiting bool    `json:"timed_out_waiting,omitempty"`
}

// pollInterval is how often grove_run polls job status while waiting.
var pollInterval = 2 * time.Second

// maxWait caps how long grove_run will wait even if the caller asks for longer, so a stuck job
// can't wedge the MCP tool call forever.
const maxWait = 30 * time.Minute

func (h *handlers) run(ctx context.Context, req *mcpsdk.CallToolRequest, args runArgs) (*mcpsdk.CallToolResult, *runResult, error) {
	if strings.TrimSpace(args.Pool) == "" {
		return nil, nil, errors.New("pool is required")
	}
	if strings.TrimSpace(args.Script) == "" {
		return nil, nil, errors.New("script is required")
	}

	kind := dispatch.KindShell
	if args.Kind != "" {
		kind = dispatch.Kind(args.Kind)
	}

	var timeout time.Duration
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
	}

	requester := "mcp"
	if req != nil {
		if ci := req.ClientInfo(); ci != nil && ci.Name != "" {
			requester = "mcp:" + ci.Name
		}
	}

	jobReq := dispatch.JobRequest{
		Kind:      kind,
		Pool:      args.Pool,
		Repo:      args.Repo,
		Ref:       args.Ref,
		Script:    args.Script,
		Env:       args.Env,
		Timeout:   timeout,
		Requester: requester,
	}

	jobID, err := h.client.SubmitJob(ctx, jobReq)
	if err != nil {
		return nil, nil, fmt.Errorf("submit job: %w", err)
	}

	wait := true
	if args.Wait != nil {
		wait = *args.Wait
	}
	if !wait {
		return nil, &runResult{JobID: jobID}, nil
	}

	tailLines := args.TailLines
	if tailLines <= 0 {
		tailLines = 200
	}

	waitFor := timeout
	if waitFor <= 0 || waitFor > maxWait {
		waitFor = maxWait
	}

	return h.awaitJob(ctx, req, jobID, tailLines, waitFor)
}

// awaitJob polls a job until it reaches a terminal status or waitFor elapses, sending MCP
// progress notifications on the way if the caller attached a progress token.
func (h *handlers) awaitJob(ctx context.Context, req *mcpsdk.CallToolRequest, jobID string, tailLines int, waitFor time.Duration) (*mcpsdk.CallToolResult, *runResult, error) {
	deadline := time.Now().Add(waitFor)
	var progressToken any
	var session *mcpsdk.ServerSession
	if req != nil {
		progressToken = req.Params.GetProgressToken()
		session = req.Session
	}

	var elapsed float64
	for attempt := 0; ; attempt++ {
		job, err := h.client.GetJob(ctx, jobID)
		if err != nil {
			return nil, nil, fmt.Errorf("poll job %s: %w", jobID, err)
		}

		if progressToken != nil && session != nil {
			_ = session.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{
				ProgressToken: progressToken,
				Message:       fmt.Sprintf("job %s: %s", jobID, job.Status),
				Progress:      float64(attempt),
			})
		}

		if isTerminal(job.Status) || time.Now().After(deadline) {
			res := &runResult{
				JobID:    jobID,
				Status:   string(job.Status),
				ExitCode: job.ExitCode,
				Node:     job.Node,
			}
			if job.StartedAt != nil {
				end := time.Now()
				if job.FinishedAt != nil {
					end = *job.FinishedAt
				}
				res.DurationSeconds = end.Sub(*job.StartedAt).Seconds()
			}
			if !isTerminal(job.Status) {
				res.TimedOutWaiting = true
			}
			if rc, lerr := h.client.JobLogs(ctx, jobID, false); lerr == nil {
				data, _ := io.ReadAll(rc)
				rc.Close()
				res.LogTail = tail(string(data), tailLines)
			}
			return nil, res, nil
		}

		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("wait for job %s: %w", jobID, ctx.Err())
		case <-time.After(pollInterval):
		}
		elapsed += pollInterval.Seconds()
		_ = elapsed
	}
}

func isTerminal(s dispatch.Status) bool {
	switch s {
	case dispatch.StatusSuccess, dispatch.StatusFailed, dispatch.StatusCanceled, dispatch.StatusLost:
		return true
	default:
		return false
	}
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// --- grove_job_status ------------------------------------------------------------------------

type jobIDArgs struct {
	JobID string `json:"job_id" jsonschema:"the job id returned by grove_run or grove_fleet"`
}

func (h *handlers) jobStatus(ctx context.Context, _ *mcpsdk.CallToolRequest, args jobIDArgs) (*mcpsdk.CallToolResult, *dispatch.Job, error) {
	if strings.TrimSpace(args.JobID) == "" {
		return nil, nil, errors.New("job_id is required")
	}
	job, err := h.client.GetJob(ctx, args.JobID)
	if err != nil {
		return nil, nil, err
	}
	return nil, job, nil
}

// --- grove_job_logs --------------------------------------------------------------------------

type jobLogsArgs struct {
	JobID     string `json:"job_id" jsonschema:"the job id to fetch logs for"`
	TailLines int    `json:"tail_lines,omitempty" jsonschema:"trailing log lines to return; default 200"`
}

type jobLogsResult struct {
	JobID   string `json:"job_id"`
	LogTail string `json:"log_tail"`
}

func (h *handlers) jobLogs(ctx context.Context, _ *mcpsdk.CallToolRequest, args jobLogsArgs) (*mcpsdk.CallToolResult, *jobLogsResult, error) {
	if strings.TrimSpace(args.JobID) == "" {
		return nil, nil, errors.New("job_id is required")
	}
	tailLines := args.TailLines
	if tailLines <= 0 {
		tailLines = 200
	}
	rc, err := h.client.JobLogs(ctx, args.JobID, false)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("read job logs: %w", err)
	}
	return nil, &jobLogsResult{JobID: args.JobID, LogTail: tail(string(data), tailLines)}, nil
}

// --- grove_job_cancel ------------------------------------------------------------------------

type jobCancelResult struct {
	JobID     string `json:"job_id"`
	Cancelled bool   `json:"cancelled"`
}

func (h *handlers) jobCancel(ctx context.Context, _ *mcpsdk.CallToolRequest, args jobIDArgs) (*mcpsdk.CallToolResult, *jobCancelResult, error) {
	if strings.TrimSpace(args.JobID) == "" {
		return nil, nil, errors.New("job_id is required")
	}
	if err := h.client.CancelJob(ctx, args.JobID); err != nil {
		return nil, nil, err
	}
	return nil, &jobCancelResult{JobID: args.JobID, Cancelled: true}, nil
}

// --- grove_recycle_vm ------------------------------------------------------------------------

type nameArgs struct {
	Name string `json:"name" jsonschema:"the VM name, as returned by grove_fleet"`
}

type recycleResult struct {
	Name     string `json:"name"`
	Recycled bool   `json:"recycled"`
}

func (h *handlers) recycleVM(ctx context.Context, _ *mcpsdk.CallToolRequest, args nameArgs) (*mcpsdk.CallToolResult, *recycleResult, error) {
	if strings.TrimSpace(args.Name) == "" {
		return nil, nil, errors.New("name is required")
	}
	drainStarted, err := h.client.RecycleVM(ctx, args.Name)
	if err != nil {
		return nil, nil, err
	}
	return nil, &recycleResult{Name: args.Name, Recycled: drainStarted}, nil
}

// --- grove_pause_worker ----------------------------------------------------------------------

type pauseWorkerArgs struct {
	Name   string `json:"name" jsonschema:"the worker (Mac) name, as returned by grove_fleet"`
	Paused bool   `json:"paused" jsonschema:"true to cordon (pause scheduling), false to resume"`
}

type pauseWorkerResult struct {
	Name   string `json:"name"`
	Paused bool   `json:"paused"`
}

func (h *handlers) pauseWorker(ctx context.Context, _ *mcpsdk.CallToolRequest, args pauseWorkerArgs) (*mcpsdk.CallToolResult, *pauseWorkerResult, error) {
	if strings.TrimSpace(args.Name) == "" {
		return nil, nil, errors.New("name is required")
	}
	var err error
	if args.Paused {
		err = h.client.PauseWorker(ctx, args.Name)
	} else {
		err = h.client.ResumeWorker(ctx, args.Name)
	}
	if err != nil {
		return nil, nil, err
	}
	return nil, &pauseWorkerResult{Name: args.Name, Paused: args.Paused}, nil
}
