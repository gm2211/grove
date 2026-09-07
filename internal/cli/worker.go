package cli

import (
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func init() {
	Root.AddCommand(workerCmd)
	workerCmd.AddCommand(workerLsCmd)
	workerCmd.AddCommand(workerPauseCmd)
	workerCmd.AddCommand(workerResumeCmd)
}

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Inspect and control Orchard workers.",
}

var workerLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List workers.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		client, err := newOrchardClient(cfg)
		if err != nil {
			return err
		}

		workers, err := client.ListWorkers(cmd.Context())
		if err != nil {
			return err
		}

		sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tARCH\tRUNTIME\tONLINE\tPAUSED\tLAST SEEN")

		for _, wk := range workers {
			lastSeen := "-"
			if !wk.LastSeen.IsZero() {
				lastSeen = time.Since(wk.LastSeen).Round(time.Second).String() + " ago"
			}

			fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%t\t%s\n",
				wk.Name, wk.Arch, wk.Runtime, !wk.Offline, wk.SchedulingPaused, lastSeen)
		}

		return w.Flush()
	},
}

var workerPauseCmd = &cobra.Command{
	Use:   "pause <worker>",
	Short: "Pause scheduling on a worker (existing VMs keep running).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		client, err := newOrchardClient(cfg)
		if err != nil {
			return err
		}

		if err := client.PauseWorker(cmd.Context(), args[0]); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "paused %s\n", args[0])

		return nil
	},
}

var workerResumeCmd = &cobra.Command{
	Use:   "resume <worker>",
	Short: "Resume scheduling on a worker.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}

		client, err := newOrchardClient(cfg)
		if err != nil {
			return err
		}

		if err := client.ResumeWorker(cmd.Context(), args[0]); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "resumed %s\n", args[0])

		return nil
	},
}
