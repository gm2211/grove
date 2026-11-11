package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/registryauth"
)

// `grove registry pull` arms the control plane's credential for pulling job images from a private
// registry. Like `grove github enable` it has no --token flag: the token comes from
// $GROVE_REGISTRY_TOKEN or from stdin, never from argv.
func init() {
	pull := &cobra.Command{
		Use:   "pull",
		Short: "Control the credential Linux VMs use to pull private job images.",
		Long: "Linux jobs run in containers a VM's docker pulls: the pool's runner image and the\n" +
			"job's environment image. A private registry (ghcr.io/gm2211/argos-ci) needs a login, which\n" +
			"is off until you arm it here. The control plane then writes it to the VMs of pools whose\n" +
			"images it covers, and uses it to resolve an environment image given as a tag to a digest.\n" +
			"`grove registry pull disable` wipes it from the control plane and from the VMs.\n\n" +
			"This is separate from `grove registry login`, which logs a worker Mac in for VM images.",
	}

	pull.AddCommand(&cobra.Command{
		Use: "status", Short: "Show whether a registry pull credential is armed, and what it covers.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			status, err := client.RegistryPull(cmd.Context())
			if err != nil {
				return err
			}
			printRegistryPullStatus(cmd.OutOrStdout(), status)
			return nil
		},
	})

	var registry, user string
	var repos []string
	enable := &cobra.Command{
		Use:   "enable --user <user> --repo <owner/name> [--repo …]",
		Short: "Arm a registry credential so VMs can pull private images.",
		Long: "Arms (or rotates) the credential. The token is read from $GROVE_REGISTRY_TOKEN, or else\n" +
			"from stdin, never from an argument. On a Mac:\n\n" +
			"  pbpaste | grove registry pull enable --user <github-login> --repo <owner/name>\n\n" +
			"For GHCR use a classic personal access token with only the read:packages scope.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, err := readRegistryToken(os.Getenv("GROVE_REGISTRY_TOKEN"), cmd.InOrStdin(), stdinIsTerminal())
			if err != nil {
				return err
			}
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			status, err := client.EnableRegistryPull(cmd.Context(), registryauth.EnableRequest{
				Registry: registry, User: user, Token: token, Repos: repos,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Registry pull credential enabled. VMs pick it up on the next fleet tick.")
			printRegistryPullStatus(cmd.OutOrStdout(), status)
			return nil
		},
	}
	enable.Flags().StringVar(&registry, "registry", registryauth.DefaultRegistry, "registry host the credential is for")
	enable.Flags().StringVar(&user, "user", "", "registry user name, e.g. your GitHub login (required)")
	enable.Flags().StringArrayVar(&repos, "repo", nil, "owner/name the credential may pull (required, repeatable; \"owner/*\" for every repo of an owner)")
	_ = enable.MarkFlagRequired("user")
	_ = enable.MarkFlagRequired("repo")
	pull.AddCommand(enable)

	var addRepos, removeRepos []string
	scope := &cobra.Command{
		Use:   "scope [--add <owner/name>] [--remove <owner/name>]",
		Short: "Widen or narrow the repositories the armed credential covers, without re-entering the token.",
		Long: "Edits only the repository scope of the armed credential. The token stays where it is and is\n" +
			"never sent or shown. A worker Mac whose pools' VM images are inside the scope pulls them with\n" +
			"this credential (fetched from the control plane when its Orchard worker starts), so no\n" +
			"`grove registry login` is needed on it:\n\n" +
			"  grove registry pull scope --add gm2211/grove-macos-worker",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			status, err := client.UpdateRegistryPullScope(cmd.Context(), registryauth.ScopeRequest{Add: addRepos, Remove: removeRepos})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Registry pull scope updated. Workers pick up a change when their Orchard worker next restarts.")
			printRegistryPullStatus(cmd.OutOrStdout(), status)
			return nil
		},
	}
	scope.Flags().StringArrayVar(&addRepos, "add", nil, "owner/name to add to the scope (repeatable; \"owner/*\" for every repo of an owner)")
	scope.Flags().StringArrayVar(&removeRepos, "remove", nil, "owner/name to remove from the scope (repeatable)")
	pull.AddCommand(scope)

	pull.AddCommand(&cobra.Command{
		Use: "disable", Short: "Disarm the registry pull credential and wipe it from the control plane and VMs.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newOperatorAPIClient()
			if err != nil {
				return err
			}
			if _, err := client.DisableRegistryPull(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Registry pull credential disabled. VMs are given an empty login on the next fleet tick.")
			return nil
		},
	})

	registryCmd.AddCommand(pull)
}

func printRegistryPullStatus(out io.Writer, status *registryauth.Status) {
	if status == nil || !status.Enabled {
		fmt.Fprintln(out, "registry pull credential: off (VMs pull anonymously)")
		return
	}
	fmt.Fprintln(out, "registry pull credential: ON")
	fmt.Fprintf(out, "  registry:    %s\n", status.Registry)
	fmt.Fprintf(out, "  user:        %s\n", status.User)
	fmt.Fprintf(out, "  repos:       %s\n", strings.Join(status.Repos, ", "))
	fmt.Fprintf(out, "  token:       sha256:%s\n", status.TokenFingerprint)
	if status.EnabledBy != "" && status.EnabledAt != nil {
		fmt.Fprintf(out, "  enabled by:  %s at %s\n", status.EnabledBy, status.EnabledAt.Format("2006-01-02T15:04:05Z07:00"))
	}
}
