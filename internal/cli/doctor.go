package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/install"
)

// doctorTimeout bounds the whole run, in addition to each individual HTTP check's own 3s budget,
// so a hung DNS lookup or similar can't make `grove doctor` hang forever.
const doctorTimeout = 20 * time.Second

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that this machine's grove setup is healthy: tailnet, services, Tart capacity.",
	RunE:  runDoctor,
}

func init() {
	Root.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), doctorTimeout)
	defer cancel()

	cfg, _, err := config.Load()
	if err != nil && err != config.ErrNotFound {
		return err
	}

	results := install.RunDoctor(ctx, install.ExecRunner{}, install.Options{}, cfg)

	out := cmd.OutOrStdout()
	nameWidth := 0
	for _, r := range results {
		if len(r.Name) > nameWidth {
			nameWidth = len(r.Name)
		}
	}
	allOK := true
	for _, r := range results {
		mark := "✓" // ✓
		if !r.OK {
			mark = "✗" // ✗
			allOK = false
		}
		fmt.Fprintf(out, "%s  %-*s  %s\n", mark, nameWidth, r.Name, r.Detail)
		if !r.OK && r.Remediation != "" {
			fmt.Fprintf(out, "   %-*s  -> %s\n", nameWidth, "", r.Remediation)
		}
	}
	if !allOK {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}
