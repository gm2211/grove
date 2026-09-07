package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/config"
	grovemcp "github.com/gm2211/grove/internal/mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	Root.AddCommand(mcpCmd)
}

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run grove's MCP server over stdio (for Claude Code, Codex, Claude Desktop)",
	Long: `grove mcp runs a Model Context Protocol server on stdin/stdout, exposing the fleet and
job-dispatch API (grove_fleet, grove_run, grove_job_status, grove_job_logs, grove_job_cancel,
grove_recycle_vm, grove_pause_worker) as MCP tools, plus grove://fleet and grove://jobs/recent
resources and a grove_ci prompt.

It reads server.url and server.token from the grove config (~/.config/grove/config.yaml, or
$GROVE_CONFIG) and never opens a network listener of its own — it only talks stdio to its MCP
client and HTTP to the grove server you've configured.

Register it with:
  claude mcp add grove -- grove mcp

See docs/MCP.md for Codex and Claude Desktop configuration.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, path, err := config.Load()
		if err != nil {
			return fmt.Errorf("load grove config (%s): %w", path, err)
		}

		server, err := grovemcp.NewServer(cfg, Version)
		if err != nil {
			return err
		}

		return server.Run(cmd.Context(), &mcpsdk.StdioTransport{})
	},
}
