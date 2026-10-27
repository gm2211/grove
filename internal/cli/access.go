package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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
				switch {
				case device.RevokedAt != nil:
					state = "revoked"
				case device.ExpiresAt != nil && !time.Now().Before(*device.ExpiresAt):
					state = "expired " + device.ExpiresAt.Format(time.DateOnly)
				case device.ExpiresAt != nil:
					state = "active, expires " + device.ExpiresAt.Format(time.DateOnly)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", device.ID, device.Name, strings.Join(device.Scopes, ","), state)
			}
			return nil
		},
	})
	access.AddCommand(newAccessIssueCmd())
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

// newAccessIssueCmd is `grove access issue`: mint a scoped credential without the interactive
// join flow, e.g. for an orchestrator that should dispatch build/agent jobs with its own
// revocable token instead of the operator one. The raw token goes to stdout exactly once (and
// nothing else does, so `TOKEN=$(grove access issue ...)` captures just the token); the device id
// needed to revoke it goes to stderr.
func newAccessIssueCmd() *cobra.Command {
	var name, expiresIn string
	var scopes []string
	cmd := &cobra.Command{
		Use:   "issue --name NAME --scope SCOPE [--scope SCOPE ...]",
		Short: "Mint a scoped device credential and print its token once.",
		Long: "Mint a revocable device credential (operator credential required) and print its raw token\n" +
			"to stdout exactly once; the control plane keeps only a hash, so store it now.\n\n" +
			"Scopes: read, dispatch (build + agent jobs), dispatch:build, dispatch:agent,\n" +
			"dispatch:shell (raw shell jobs), operator. Example, for an orchestrator:\n\n" +
			"  grove access issue --name ci-bot --scope read --scope dispatch\n\n" +
			"--expires-in makes it stop working after that long (\"180d\", \"720h\"); its holder sees\n" +
			"when in GET /api/v1/whoami.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("--name is required")
			}
			if len(scopes) == 0 {
				return fmt.Errorf("at least one --scope is required")
			}
			var ttl time.Duration
			if expiresIn != "" {
				var err error
				if ttl, err = parseTTL(expiresIn); err != nil {
					return fmt.Errorf("--expires-in: %w", err)
				}
			}
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			token, device, err := client.IssueDevice(cmd.Context(), name, scopes, ttl)
			if err != nil {
				return err
			}
			expiry := ""
			if device.ExpiresAt != nil {
				expiry = ", expiring " + device.ExpiresAt.Format(time.DateOnly)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Issued %s (%s) with scopes %s%s; revoke with `grove access revoke %s`.\n",
				device.ID, device.Name, strings.Join(device.Scopes, ","), expiry, device.ID)
			fmt.Fprintln(cmd.OutOrStdout(), token)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "label for the credential, shown by `grove access list`")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "scope to grant (repeatable)")
	cmd.Flags().StringVar(&expiresIn, "expires-in", "", `stop working after this long, e.g. "180d" or "720h" (default: never)`)
	return cmd
}

// parseTTL reads a Go duration, or a whole number of days ("180d").
func parseTTL(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%q: want a number of days like \"180d\" or a duration like \"720h\"", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, err
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q is not in the future", s)
	}
	return d, nil
}
