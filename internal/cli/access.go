package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/config"
)

func newOperatorAPIClient() (*apiclient.Client, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	token, err := config.ResolveOperatorToken(cfg)
	if err != nil {
		return nil, err
	}
	return apiclient.New(cfg.Server.URL, token), nil
}

func init() {
	access := &cobra.Command{Use: "access", Short: "Manage enrolled Grove device credentials."}
	access.AddCommand(&cobra.Command{
		Use: "list", Short: "List enrolled devices and their scopes.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			devices, err := client.Devices(cmd.Context())
			if err != nil {
				return err
			}
			for _, device := range devices {
				state := "active"
				if device.RevokedAt != nil {
					state = "revoked"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", device.ID, device.Name, strings.Join(device.Scopes, ","), state)
			}
			return nil
		},
	})
	access.AddCommand(&cobra.Command{
		Use: "revoke DEVICE_ID", Short: "Revoke one enrolled device.", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			if err := client.RevokeDevice(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Device credential revoked.")
			return nil
		},
	})
	Root.AddCommand(access)
}
