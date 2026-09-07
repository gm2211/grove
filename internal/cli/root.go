// Package cli holds the grove command tree. Each command lives in its own file and registers
// itself on Root from an init() so files can be added without editing this one.
package cli

import (
	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags "-X github.com/gm2211/grove/internal/cli.Version=…".
var Version = "dev"

// Root is the `grove` command.
var Root = &cobra.Command{
	Use:   "grove",
	Short: "Your Macs as a private build/agent cloud: Tart VMs via Orchard, jobs via Nomad.",
	Long: `grove turns the Macs (and Linux boxes) you already own into a private cloud for CI builds,
deploys and coding-agent sessions, reachable only over your Tailscale tailnet.

Start with:  grove install --role worker        (on each Mac)
             grove install --role control-plane (on the always-on box)
             grove fleet apply                  (create the worker VMs)
             grove dispatch --pool linux -- make test`,
	SilenceUsage: true,
	// SilenceErrors: main.go is the sole place errors get printed (it special-cases *ExitError —
	// see internal/cli/logs.go — to skip printing anything at all, since that error type means a
	// command already printed its own summary, e.g. "job <id> failed (exit N) on <node>").
	// Without this, cobra's own default error reporting would print a redundant "Error: <msg>"
	// line first (a blank one for *ExitError, since its Error() is intentionally empty).
	SilenceErrors: true,
	Version:       Version,
}

// Execute runs the CLI.
func Execute() error {
	return Root.Execute()
}
