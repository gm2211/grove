package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/secrets"
)

// `grove secret set` has no --value flag, for the same reason `grove github enable` has no
// --token: argv lands in shell history and in `ps` on the control plane. The value comes from
// stdin or a named environment variable.
func init() {
	secret := &cobra.Command{
		Use:   "secret",
		Short: "Manage named secrets jobs can request by name.",
		Long: "A named secret lives on the control plane. A job lists the names it needs\n" +
			"(\"secrets\": [\"CLAUDE_CODE_OAUTH_TOKEN\"]) and grove exports each value under its name\n" +
			"when the job is dispatched, so the submitter never sends or sees it. --allow names who\n" +
			"may request it: a device credential id, a Tailscale identity (tailscale:<stable id>), or a\n" +
			"Tailscale tag (tag:argos). Operators may request any secret.",
	}

	secret.AddCommand(&cobra.Command{
		Use: "list", Short: "List named secrets (names, grants and fingerprints, never values).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			list, err := client.Secrets(cmd.Context())
			if err != nil {
				return err
			}
			printSecrets(cmd.OutOrStdout(), list)
			return nil
		},
	})

	var fromEnv string
	var allow []string
	set := &cobra.Command{
		Use:   "set NAME",
		Short: "Create or replace a named secret; the value is read from stdin unless --from-env is set.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := readSecretValue(cmd.InOrStdin(), fromEnv)
			if err != nil {
				return err
			}
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			out, err := client.SetSecret(cmd.Context(), args[0], secrets.SetRequest{Value: value, Allow: allow})
			if err != nil {
				return err
			}
			who := "operators only"
			if len(out.Allow) > 0 {
				who = strings.Join(out.Allow, ", ")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Secret %s set (sha256:%s); may be requested by %s.\n", out.Name, out.Fingerprint, who)
			return nil
		},
	}
	set.Flags().StringVar(&fromEnv, "from-env", "", "read the value from this environment variable instead of stdin")
	set.Flags().StringArrayVar(&allow, "allow", nil, "principal id or tag:<name> that may request this secret (repeatable; none = operators only)")
	secret.AddCommand(set)

	secret.AddCommand(&cobra.Command{
		Use: "rm NAME", Aliases: []string{"delete"}, Short: "Delete a named secret.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			if err := client.DeleteSecret(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Secret %s deleted.\n", args[0])
			return nil
		},
	})

	Root.AddCommand(secret)
}

// readSecretValue reads the value from an environment variable or stdin, never argv. Stdin keeps
// inner newlines (a credential file is a valid value) but drops one trailing newline, which is
// what `echo` and most pastes add.
func readSecretValue(stdin io.Reader, fromEnv string) (string, error) {
	if fromEnv != "" {
		value := os.Getenv(fromEnv)
		if value == "" {
			return "", fmt.Errorf("$%s is empty", fromEnv)
		}
		return value, nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, secrets.MaxValueLen+1))
	if err != nil {
		return "", fmt.Errorf("read secret from stdin: %w", err)
	}
	if len(data) > secrets.MaxValueLen {
		return "", fmt.Errorf("secret on stdin is longer than %d bytes", secrets.MaxValueLen)
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if value == "" {
		return "", errors.New("no secret on stdin (pipe it in, or use --from-env)")
	}
	return value, nil
}

func printSecrets(out io.Writer, list []secrets.Secret) {
	if len(list) == 0 {
		fmt.Fprintln(out, "no named secrets")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tALLOW\tFINGERPRINT\tUPDATED\tLAST USED")
	for _, s := range list {
		who := "(operators only)"
		if len(s.Allow) > 0 {
			who = strings.Join(s.Allow, ",")
		}
		used := "never"
		if s.LastUsedAt != nil {
			used = s.LastUsedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\tsha256:%s\t%s\t%s\n", s.Name, who, s.Fingerprint, s.UpdatedAt.Format(time.RFC3339), used)
	}
	_ = tw.Flush()
}
