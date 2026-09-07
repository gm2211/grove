package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/config"
)

func init() {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Stream a job's combined stdout+stderr logs.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			return streamLogsToStdout(cmd, client, args[0], follow)
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "keep streaming until the job ends")
	Root.AddCommand(cmd)
}

// newAPIClient builds an apiclient.Client from the loaded grove config.
func newAPIClient() (*apiclient.Client, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.Server.URL == "" {
		return nil, fmt.Errorf("server.url is not set in the grove config; run `grove config init` or set GROVE_CONFIG")
	}
	return apiclient.New(cfg.Server.URL, cfg.Server.Token), nil
}

func streamLogsToStdout(cmd *cobra.Command, client *apiclient.Client, id string, follow bool) error {
	rc, err := client.JobLogs(cmd.Context(), id, follow)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(cmd.OutOrStdout(), rc)
	return err
}
