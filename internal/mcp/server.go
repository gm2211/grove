// Package mcp implements grove's Model Context Protocol server: the fleet and job-dispatch API
// exposed as MCP tools/resources/prompts over stdio, so Claude Code, Codex and Claude Desktop can
// drive the grove fleet directly. See docs/MCP.md for client setup and ARCHITECTURE.md for the
// underlying HTTP API this talks to.
package mcp

import (
	"errors"
	"strings"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/config"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverInstructions = `grove exposes a private fleet of build/agent machines (Macs + Linux) reachable only over
your tailnet. Use grove_fleet to see what's available (pools, online/cordoned workers, VM and
node counts) before submitting work. Use grove_run to submit a job (shell command, build, or
long-lived agent session) to a pool; by default it waits for completion and returns the exit
code and a log tail. Use grove_job_status / grove_job_logs / grove_job_cancel to manage jobs
submitted with wait=false or by other clients. grove_recycle_vm and grove_pause_worker are
fleet-maintenance actions — use them deliberately, they affect real machines.`

// NewServer builds the grove MCP server, wired against the grove HTTP API at cfg.Server.URL
// (Authorization: Bearer cfg.Server.Token). version is reported to clients as the server's
// implementation version (e.g. the grove CLI version).
func NewServer(cfg *config.Config, version string) (*mcpsdk.Server, error) {
	baseURL := strings.TrimSpace(cfg.Server.URL)
	if baseURL == "" {
		return nil, errors.New("grove server URL is not configured — set server.url in ~/.config/grove/config.yaml (or $GROVE_CONFIG)")
	}
	client := apiclient.New(baseURL, cfg.Server.Token)
	return newServerWithClient(client, version), nil
}

func newServerWithClient(client *apiclient.Client, version string) *mcpsdk.Server {
	h := &handlers{client: client}

	impl := &mcpsdk.Implementation{
		Name:    "grove",
		Title:   "grove fleet",
		Version: version,
	}
	s := mcpsdk.NewServer(impl, &mcpsdk.ServerOptions{
		Instructions: serverInstructions,
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "grove_fleet",
		Description: "Get a normalised summary of the grove fleet: hosts, workers online/cordoned, VMs by pool/status, nodes ready, and running job count. Call this before submitting work to see what pools and capacity exist.",
	}, h.fleet)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "grove_run",
		Description: "Submit a job to the grove fleet: run a shell command, a build (clone repo@ref then run script), " +
			"or a long-lived agent session on a given pool. By default (wait=true) this blocks until the job reaches " +
			"a terminal state and returns its exit code, node, duration, and a log tail; pass wait=false to submit " +
			"and return immediately with just the job id (useful for long-running agent sessions — poll with " +
			"grove_job_status / grove_job_logs afterwards).",
	}, h.run)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "grove_job_status",
		Description: "Get the current status of a previously submitted job (pending/running/success/failed/canceled/lost), including exit code, node, and timestamps.",
	}, h.jobStatus)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "grove_job_logs",
		Description: "Fetch the trailing log lines for a job.",
	}, h.jobLogs)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "grove_job_cancel",
		Description: "Cancel a running or pending job.",
	}, h.jobCancel)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "grove_recycle_vm",
		Description: "Recycle (drain then delete) a VM by name; the fleet reconciler recreates it from its image. Use when a VM is wedged or needs a clean image pull. This disrupts any job currently running on it.",
	}, h.recycleVM)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "grove_pause_worker",
		Description: "Cordon (paused=true) or resume (paused=false) scheduling on a worker Mac. Cordoning stops new VMs/jobs from landing there without disturbing what's already running.",
	}, h.pauseWorker)

	s.AddResource(&mcpsdk.Resource{
		URI:         fleetResourceURI,
		Name:        "grove-fleet",
		Description: "Normalised fleet summary as JSON (same shape as the grove_fleet tool result).",
		MIMEType:    "application/json",
	}, h.readFleetResource)

	s.AddResource(&mcpsdk.Resource{
		URI:         jobsResourceURI,
		Name:        "grove-jobs-recent",
		Description: "The most recent jobs (up to 50), newest first, as JSON.",
		MIMEType:    "application/json",
	}, h.readRecentJobsResource)

	s.AddPrompt(ciPrompt(), h.getCIPrompt)

	return s
}
