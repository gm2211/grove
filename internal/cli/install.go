package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/install"
)

var installFlags struct {
	role          string
	controller    string
	nomad         string
	server        string
	token         string
	registry      string
	registryUser  string
	registryToken string
	yes           bool
	dryRun        bool
}

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Bootstrap this machine into one of grove's roles (worker, control-plane, client).",
	Long: `grove install renders an idempotent plan for the given --role and applies it: Homebrew/
Tart/Orchard/Nomad/MinIO dependencies, launchd agents or systemd --user units, and ~/.config/grove/
config.yaml. Re-running it is safe — steps that are already satisfied are skipped.

Steps that need sudo or touch system-wide settings (disabling sleep, the macOS 15+ local-network
permission, loading a LaunchAgent) are never applied automatically: grove prints the exact command
for a human (or Claude Code) to run. Use --dry-run to see the whole plan without changing anything.`,
	RunE: runInstall,
}

func init() {
	installCmd.Flags().StringVar(&installFlags.role, "role", "", "worker | control-plane | client (required)")
	installCmd.Flags().StringVar(&installFlags.controller, "controller", "", "Orchard controller URL")
	installCmd.Flags().StringVar(&installFlags.nomad, "nomad", "", "Nomad HTTP API URL")
	installCmd.Flags().StringVar(&installFlags.server, "server", "", "grove server URL")
	installCmd.Flags().StringVar(&installFlags.token, "token", "", "bootstrap/server token (meaning depends on --role)")
	installCmd.Flags().StringVar(&installFlags.registry, "registry", "ghcr.io", "container registry a worker authenticates to for private image pulls (worker role; see docs/INSTALL.md)")
	installCmd.Flags().StringVar(&installFlags.registryUser, "registry-user", "", "registry username for `tart login` (worker role)")
	installCmd.Flags().StringVar(&installFlags.registryToken, "registry-token", "", "registry password/PAT for `tart login` (worker role); also read from $GROVE_REGISTRY_TOKEN so it needn't be in shell history")
	installCmd.Flags().BoolVar(&installFlags.yes, "yes", false, "apply privileged steps too, when running as root")
	installCmd.Flags().BoolVar(&installFlags.dryRun, "dry-run", false, "print the plan; change nothing")
	_ = installCmd.MarkFlagRequired("role")
	Root.AddCommand(installCmd)
}

func runInstall(cmd *cobra.Command, args []string) error {
	role := install.Role(installFlags.role)
	switch role {
	case install.RoleWorker, install.RoleControlPlane, install.RoleClient:
	default:
		return fmt.Errorf("--role must be %q, %q or %q (got %q)", install.RoleWorker, install.RoleControlPlane, install.RoleClient, installFlags.role)
	}

	registryToken := installFlags.registryToken
	if registryToken == "" {
		// Falling back to the environment lets the token skip shell history/process-listing
		// exposure entirely; the flag still exists for scripts that manage their own secret
		// handling.
		registryToken = os.Getenv("GROVE_REGISTRY_TOKEN")
	}

	opts := install.Options{
		Role:          role,
		Controller:    installFlags.controller,
		NomadAddr:     installFlags.nomad,
		ServerURL:     installFlags.server,
		Token:         installFlags.token,
		Registry:      installFlags.registry,
		RegistryUser:  installFlags.registryUser,
		RegistryToken: registryToken,
	}

	out := cmd.OutOrStdout()
	runner := install.ExecRunner{}
	steps, err := install.BuildPlan(runner, opts, out)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "grove install --role %s (%d steps%s)\n", role, len(steps), dryRunSuffix(installFlags.dryRun))
	results, err := install.RunPlan(cmd.Context(), steps, install.PlanOptions{
		DryRun: installFlags.dryRun,
		Yes:    installFlags.yes,
		IsRoot: os.Geteuid() == 0,
		Out:    out,
	})
	manual := 0
	for _, r := range results {
		if r.Skipped && !installFlags.dryRun {
			manual++
		}
	}
	if manual > 0 {
		fmt.Fprintf(out, "\n%d step(s) need manual action (see [manual] above); re-run with --yes as root once you've done them, or do them and re-run grove install to pick up where it left off.\n", manual)
	}
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintln(out, "\nrun `grove doctor` to verify.")
	return nil
}

func dryRunSuffix(dryRun bool) string {
	if dryRun {
		return ", dry-run"
	}
	return ""
}
