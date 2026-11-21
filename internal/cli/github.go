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
		Long: "Private-repo sourcing is off until you turn it on. While it is on, grove hands the\n" +
			"armed credential to the `git clone` of a build/agent job whose repo URL is in scope and\n" +
			"whose submitter is an operator or named with --allow; any other submitter's job gets\n" +
			"no token. It always expires (--ttl, 24h by default); `grove github disable` or the TTL\n" +
			"lapsing wipes the token from the control plane.",
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
	var allow []string
	var ttl time.Duration
	var renew string
	enable := &cobra.Command{
		Use:   "enable",
		Short: "Arm a GitHub credential so jobs can clone private repositories.",
		Long: "Arms a GitHub token for the repositories named with --repo, for --ttl (24h by default,\n" +
			"30 days at most). Use a fine-grained, read-only token limited to those repositories.\n\n" +
			"--allow names who, besides an operator, may submit a job that clones with it: a\n" +
			"principal id (a device credential's id from `grove access list`, or a Tailscale\n" +
			"identity's tailscale:<stable id>) or a Tailscale tag (tag:argos), the same syntax as\n" +
			"`grove secret set --allow`. With no --allow only operators' jobs get the token. Don't\n" +
			"name a worker Mac's own credential: a worker can read the token out of a VM it hosts.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, err := readEnableToken(cmd.InOrStdin(), tokenEnv, tokenStdin)
			if err != nil {
				return err
			}
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			if ttl <= 0 {
				return errors.New("--ttl must be positive: a credential is never armed forever")
			}
			req := gitauth.EnableRequest{Token: token, Hosts: hosts, Repos: repos, Allow: allow, TTL: ttl.String(), Renew: renew}
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
	enable.Flags().StringArrayVar(&repos, "repo", nil, "owner/name the credential may clone (required, repeatable; \"owner/*\" allows every repo of an owner)")
	enable.Flags().StringArrayVar(&hosts, "host", nil, "git host the credential may be used against (repeatable, default github.com — set for GitHub Enterprise)")
	enable.Flags().StringArrayVar(&allow, "allow", nil, "principal id or tag:<name> whose jobs may clone with the credential (repeatable; none = operators only)")
	enable.Flags().StringVar(&renew, "renew", "", "\"gh\": before the token expires, the control plane replaces it with the one `gh auth token` prints for the user `grove serve` runs as, keeping the repos, allow list and TTL; it still lapses if that gh login is gone")
	enable.Flags().DurationVar(&ttl, "ttl", gitauth.DefaultTTL, "auto-disable and wipe the token after this long, e.g. 4h (at most 720h)")
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
	scope := "none (re-arm with --repo; nothing is cloned privately until then)"
	if len(status.Repos) > 0 {
		scope = strings.Join(status.Repos, ", ")
	}
	fmt.Fprintf(out, "private-repo sourcing: ON\n")
	fmt.Fprintf(out, "  hosts:       %s\n", strings.Join(status.Hosts, ", "))
	fmt.Fprintf(out, "  repos:       %s\n", scope)
	allowed := "operators only (re-arm with --allow to let another principal's jobs clone privately)"
	switch {
	case status.Legacy:
		allowed = "ANY submitter (armed by an older grove)"
	case len(status.Allow) > 0:
		allowed = "operators, " + strings.Join(status.Allow, ", ")
	}
	fmt.Fprintf(out, "  allowed:     %s\n", allowed)
	fmt.Fprintf(out, "  token:       sha256:%s\n", status.TokenFingerprint)
	if status.ExpiresAt != nil {
		fmt.Fprintf(out, "  expires:     %s (in %s)\n", status.ExpiresAt.Format(time.RFC3339), time.Until(*status.ExpiresAt).Round(time.Minute))
	}
	if status.Renew != "" {
		fmt.Fprintf(out, "  renews:      from %s on the control plane, at half its lifetime\n", status.Renew)
	}
	if status.EnabledBy != "" && status.EnabledAt != nil {
		fmt.Fprintf(out, "  enabled by:  %s at %s\n", status.EnabledBy, status.EnabledAt.Format(time.RFC3339))
	}
	if status.LastUsedAt != nil {
		fmt.Fprintf(out, "  last used:   %s\n", status.LastUsedAt.Format(time.RFC3339))
	}
	if status.Warning != "" {
		fmt.Fprintf(out, "  WARNING:     %s\n", status.Warning)
	}
}
