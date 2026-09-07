package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func init() {
	jobsCmd := &cobra.Command{
		Use:   "jobs",
		Short: "List and manage grove jobs.",
	}

	lsCmd := &cobra.Command{
		Use:   "ls",
		Short: "List jobs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			jobs, err := client.ListJobs(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tPOOL\tSTATUS\tSUBMITTED")
			for _, j := range jobs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", j.ID, j.Request.Kind, j.Request.Pool, j.Status, j.SubmittedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}

	getCmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show a job's full status as JSON.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			job, err := client.GetJob(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			data, err := json.MarshalIndent(job, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return nil
		},
	}

	cancelCmd := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a running or pending job.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			if err := client.CancelJob(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "canceled:", args[0])
			return nil
		},
	}

	jobsCmd.AddCommand(lsCmd, getCmd, cancelCmd)
	Root.AddCommand(jobsCmd)
}
