package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/install"
)

// `grove root-helper`: the root LaunchDaemon com.grove.root-helper that does a worker Mac's root
// steps from then on, from requests the worker user's grove writes and the helper checks. It never
// runs new code. See internal/install/roothelper.go.

var rootHelperUser string

var rootHelperCmd = &cobra.Command{
	Use:    "root-helper",
	Short:  "The root helper: an admin types a password once, and the worker Mac's root steps happen by themselves",
	Hidden: true, // reached through `sudo grove doctor --fix`
}

var rootHelperInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the root helper for a worker user (once, with sudo, from an admin account)",
	Long: "sudo grove root-helper install --user <worker user>\n\n" +
		"Installs com.grove.root-helper: a root LaunchDaemon that loads the VM firewall the worker user's\n" +
		"grove asks for, after checking the rules against a fixed grammar, so a grove upgrade that changes\n" +
		"the rules needs no admin. It copies this grove (which the worker user must not be able to write)\n" +
		"as the helper's binary and never updates it. It also retires com.grove.pf-vm and sets the Mac\n" +
		"never to sleep and to start again after a power cut.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if os.Geteuid() != 0 {
			return fmt.Errorf("run it with sudo: sudo %s root-helper install --user %s", os.Args[0], orUnknown(rootHelperUser))
		}
		u, err := rootHelperTarget()
		if err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return install.InstallRootHelper(cmd.Context(), install.ExecRunner{}, install.RootHelperInstall{
			User: u, Exe: exe, GroveVersion: Version, Out: cmd.OutOrStdout(),
		})
	},
}

var rootHelperApplyCmd = &cobra.Command{
	Use:    "apply",
	Short:  "One pass of the root helper (run as root by its LaunchDaemon)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		exe, _ := os.Executable()
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if exe != install.RootHelperBin() {
			return errors.New("only the installed helper (" + install.RootHelperBin() + ") runs apply")
		}
		return install.RootHelperApply(cmd.Context(), install.ExecRunner{}, Version, cmd.OutOrStdout())
	},
}

var rootHelperStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show what the root helper last did",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		st, err := install.ReadRootHelperStatus()
		if err != nil {
			return fmt.Errorf("no root helper status: %w", err)
		}
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	},
}

func rootHelperTarget() (*user.User, error) {
	u, err := user.Lookup(rootHelperUser)
	if err != nil || rootHelperUser == "" || u.Uid == "0" || !filepath.IsAbs(u.HomeDir) {
		return nil, fmt.Errorf("--user %q must name an ordinary account with a home directory", rootHelperUser)
	}
	return u, nil
}

func init() {
	rootHelperInstallCmd.Flags().StringVar(&rootHelperUser, "user", "", "the worker user whose Mac this keeps")
	rootHelperCmd.AddCommand(rootHelperInstallCmd, rootHelperApplyCmd, rootHelperStatusCmd)
	Root.AddCommand(rootHelperCmd)
}
