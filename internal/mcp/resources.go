package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/gm2211/grove/internal/dispatch"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	fleetResourceURI = "grove://fleet"
	jobsResourceURI  = "grove://jobs/recent"

	// recentJobsLimit caps how many jobs grove://jobs/recent returns, newest first.
	recentJobsLimit = 50
)

func (h *handlers) readFleetResource(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	fleet, err := h.client.Fleet(ctx)
	if err != nil {
		return nil, err
	}
	jobs, _ := h.client.ListJobs(ctx) // best-effort; fleet is still useful without job counts
	summary := summarizeFleet(fleet, jobs)

	body, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal fleet summary: %w", err)
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{
			URI:      fleetResourceURI,
			MIMEType: "application/json",
			Text:     string(body),
		}},
	}, nil
}

func (h *handlers) readRecentJobsResource(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	jobs, err := h.client.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].SubmittedAt.After(jobs[j].SubmittedAt)
	})
	if len(jobs) > recentJobsLimit {
		jobs = jobs[:recentJobsLimit]
	}

	body, err := json.MarshalIndent(struct {
		Jobs []dispatch.Job `json:"jobs"`
	}{Jobs: jobs}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal recent jobs: %w", err)
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{
			URI:      jobsResourceURI,
			MIMEType: "application/json",
			Text:     string(body),
		}},
	}, nil
}
