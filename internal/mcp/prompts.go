package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const ciPromptName = "grove_ci"

func ciPrompt() *mcpsdk.Prompt {
	return &mcpsdk.Prompt{
		Name:        ciPromptName,
		Description: "Run this repo's tests on a grove pool and report the result.",
		Arguments: []*mcpsdk.PromptArgument{
			{Name: "pool", Description: "grove pool to run on, e.g. linux or macos", Required: true},
			{Name: "repo", Description: "git URL of the repo to test; defaults to the current repo's origin", Required: false},
			{Name: "ref", Description: "commit SHA, branch, or tag to test; defaults to the current branch", Required: false},
			{Name: "script", Description: "test command to run; defaults to the repo's usual test target (e.g. `make test`)", Required: false},
		},
	}
}

func (h *handlers) getCIPrompt(ctx context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
	pool := req.Params.Arguments["pool"]
	if strings.TrimSpace(pool) == "" {
		return nil, fmt.Errorf("%s prompt requires a pool argument", ciPromptName)
	}
	repo := req.Params.Arguments["repo"]
	ref := req.Params.Arguments["ref"]
	script := req.Params.Arguments["script"]
	if script == "" {
		script = "make test"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Run this repository's tests on grove pool `%s` and report back.\n\n", pool)
	b.WriteString("Use the grove_run tool with:\n")
	fmt.Fprintf(&b, "- pool: %s\n", pool)
	if repo != "" {
		fmt.Fprintf(&b, "- repo: %s\n", repo)
	} else {
		b.WriteString("- repo: this repository's origin remote\n")
	}
	if ref != "" {
		fmt.Fprintf(&b, "- ref: %s\n", ref)
	} else {
		b.WriteString("- ref: the current branch/commit\n")
	}
	fmt.Fprintf(&b, "- script: %s\n", script)
	b.WriteString("- wait: true\n\n")
	b.WriteString("Then report: the job id, final status, exit code, and duration. " +
		"If the job failed, include the relevant tail of the log output and a short diagnosis " +
		"of what broke; if it succeeded, summarize what ran.")

	return &mcpsdk.GetPromptResult{
		Description: "Run this repo's tests on a grove pool and report the result.",
		Messages: []*mcpsdk.PromptMessage{
			{
				Role:    mcpsdk.Role("user"),
				Content: &mcpsdk.TextContent{Text: b.String()},
			},
		},
	}, nil
}
