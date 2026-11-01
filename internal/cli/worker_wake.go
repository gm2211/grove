package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func init() {
	workerCmd.AddCommand(&cobra.Command{
		Use:   "wake [<worker>]",
		Short: "End a worker's snooze now. It resumes unless something else (an operator or health pause) still holds it.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			worker, err := workerArgOrDefault(cmd.Context(), client, args)
			if err != nil {
				return err
			}
			res, err := client.WakeWorker(cmd.Context(), worker)
			if err != nil {
				return snoozeAPIError(err, worker)
			}
			if res.Resumed {
				fmt.Fprintf(cmd.OutOrStdout(), "woke %s; it is taking work again\n", worker)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "ended the snooze on %s, but it stays paused: %s\n", worker, strings.Join(res.StillPausedBecause, ", "))
			}
			return nil
		},
	})
}
