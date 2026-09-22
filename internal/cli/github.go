package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/gitauth"
)

// `grove github enable` deliberately has no --token flag: a credential passed in argv shows up in
// the shell history and in `ps` output on the control plane. The token comes from an environment
// variable (default GH_TOKEN) or from stdin.
func init() {
	github := &cobra.Command{
		Use:   "github",
		Short: "Control whether grove may clone PRIVATE GitHub repositories for jobs.",
		Long: "Private-repo sourcing is off until you turn it on, and applies fleet-wide from the\n" +
			"moment you do. While it is on, grove hands the armed credential to the `git clone` of\n" +
			"every build/agent job whose repo URL is in scope; `grove github disable` (or the TTL\n" +
			"lapsing) wipes the token from the control plane.",
	}

	github.AddCommand(&cobra.Command{
		Use: "status", Short: "Show whether private-repo sourcing is armed, and what it covers.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			status, err := client.GitHubSourcing(cmd.Context())
			if err != nil {
				return err
			}
			printGitHubSourcingStatus(cmd.OutOrStdout(), status)
			return nil
		},
	})

	var tokenEnv string
	var tokenStdin bool
	var repos []string
	var hosts []string
	var ttl time.Duration
	enable := &cobra.Command{
		Use:   "enable",
		Short: "Arm a GitHub credential so jobs can clone private repositories.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, err := readEnableToken(cmd.InOrStdin(), tokenEnv, tokenStdin)
			if err != nil {
				return err
			}
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			req := gitauth.EnableRequest{Token: token, Hosts: hosts, Repos: repos}
			if ttl > 0 {
				req.TTL = ttl.String()
			}
			status, err := client.EnableGitHubSourcing(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Private-repo sourcing enabled.")
			printGitHubSourcingStatus(cmd.OutOrStdout(), status)
			return nil
		},
	}
	enable.Flags().StringVar(&tokenEnv, "token-env", "GH_TOKEN", "environment variable holding the GitHub token")
	enable.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the GitHub token from stdin instead of an environment variable")
	enable.Flags().StringArrayVar(&repos, "repo", nil, "limit the credential to owner/name (repeatable; \"owner/*\" allows every repo of an owner). Default: any repo on the allowed hosts")
	enable.Flags().StringArrayVar(&hosts, "host", nil, "git host the credential may be used against (repeatable, default github.com — set for GitHub Enterprise)")
	enable.Flags().DurationVar(&ttl, "ttl", 0, "auto-disable after this long, e.g. 4h (default: stays on until `grove github disable`)")
	github.AddCommand(enable)

	github.AddCommand(&cobra.Command{
		Use: "disable", Short: "Disarm private-repo sourcing and wipe the stored credential.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			if _, err := client.DisableGitHubSourcing(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Private-repo sourcing disabled. Jobs can clone public repositories only.")
			return nil
		},
	})

	Root.AddCommand(github)
}

// readEnableToken resolves the credential from stdin or an environment variable, never from a
// command-line argument.
func readEnableToken(stdin io.Reader, tokenEnv string, tokenStdin bool) (string, error) {
	if tokenStdin {
		data, err := io.ReadAll(io.LimitReader(stdin, 16<<10))
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", errors.New("no token on stdin")
		}
		return token, nil
	}
	if tokenEnv == "" {
		return "", errors.New("pass --token-stdin or name an environment variable with --token-env")
	}
	token := strings.TrimSpace(os.Getenv(tokenEnv))
	if token == "" {
		return "", fmt.Errorf("$%s is empty — export the GitHub token there, or pipe it in with --token-stdin", tokenEnv)
	}
	return token, nil
}

func printGitHubSourcingStatus(out io.Writer, status *gitauth.Status) {
	if status == nil || !status.Enabled {
		fmt.Fprintln(out, "private-repo sourcing: off (jobs can clone public repositories only)")
		return
	}
	scope := "any repo"
	if len(status.Repos) > 0 {
		scope = strings.Join(status.Repos, ", ")
	}
	fmt.Fprintf(out, "private-repo sourcing: ON\n")
	fmt.Fprintf(out, "  hosts:       %s\n", strings.Join(status.Hosts, ", "))
	fmt.Fprintf(out, "  repos:       %s\n", scope)
	fmt.Fprintf(out, "  token:       sha256:%s\n", status.TokenFingerprint)
	if status.ExpiresAt != nil {
		fmt.Fprintf(out, "  expires:     %s (in %s)\n", status.ExpiresAt.Format(time.RFC3339), time.Until(*status.ExpiresAt).Round(time.Minute))
	} else {
		fmt.Fprintf(out, "  expires:     never — run `grove github disable` when you're done\n")
	}
	if status.EnabledBy != "" && status.EnabledAt != nil {
		fmt.Fprintf(out, "  enabled by:  %s at %s\n", status.EnabledBy, status.EnabledAt.Format(time.RFC3339))
	}
	if status.LastUsedAt != nil {
		fmt.Fprintf(out, "  last used:   %s\n", status.LastUsedAt.Format(time.RFC3339))
	}
}
