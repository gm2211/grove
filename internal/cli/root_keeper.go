package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/install"
)

// `grove root-keeper run --user <name>`: the root LaunchDaemon com.grove.root-keeper runs it from
// its root-owned copy of grove (install.RootKeeperBin). See internal/install/rootkeeper.go.

const (
	rootKeeperFirst = time.Minute
	rootKeeperTick  = 15 * time.Minute
	// rootKeeperUpdateEvery ticks between look-ups of a newer release for the keeper's own binary.
	rootKeeperUpdateEvery = 4
)

var rootKeeperUser string

var rootKeeperCmd = &cobra.Command{
	Use:    "root-keeper",
	Short:  "Do a worker Mac's root steps from a root LaunchDaemon",
	Hidden: true,
}

var rootKeeperRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the root keeper (as root, from its LaunchDaemon)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if os.Geteuid() != 0 {
			return errors.New("the root keeper runs as root, from " + install.RootKeeperLabel)
		}
		u, err := rootKeeperTarget()
		if err != nil {
			return err
		}
		runRootKeeper(cmd.Context(), u.Username, u.HomeDir)
		return nil
	},
}

var rootKeeperInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the root keeper and do every pending root step now (run once, with sudo, from an admin account)",
	Long: "The last root step an admin runs on a worker Mac: `sudo grove root-keeper install --user <worker user>`.\n" +
		"It does the root steps that are pending now (a newer VM firewall, the worker daemons) and installs\n" +
		"com.grove.root-keeper, which does them by itself from then on, from its own root-owned grove that\n" +
		"only ever updates to a release whose checksum and build provenance verify.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if os.Geteuid() != 0 {
			return fmt.Errorf("run it with sudo: sudo %s root-keeper install --user %s", os.Args[0], orUnknown(rootKeeperUser))
		}
		u, err := rootKeeperTarget()
		if err != nil {
			return err
		}
		r := install.ExecRunner{}
		opts, err := install.RootKeeperOptions(cmd.Context(), r, u.Username, u.HomeDir)
		if err != nil {
			return err
		}
		ran, err := install.InstallRootKeeper(cmd.Context(), r, opts)
		if err != nil {
			return err
		}
		if len(ran) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "Nothing needed root: the root keeper and every root step were already in place.")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Done: %s. From now on %s does this Mac's root steps by itself.\n", strings.Join(ran, ", "), install.RootKeeperLabel)
		return nil
	},
}

func rootKeeperTarget() (*user.User, error) {
	u, err := user.Lookup(rootKeeperUser)
	if err != nil || rootKeeperUser == "" || u.Uid == "0" || !filepath.IsAbs(u.HomeDir) {
		return nil, fmt.Errorf("--user %q must name an ordinary account with a home directory", rootKeeperUser)
	}
	return u, nil
}

func init() {
	for _, c := range []*cobra.Command{rootKeeperRunCmd, rootKeeperInstallCmd} {
		c.Flags().StringVar(&rootKeeperUser, "user", "", "the worker user whose Mac this keeps")
		rootKeeperCmd.AddCommand(c)
	}
	Root.AddCommand(rootKeeperCmd)
}

// runRootKeeper keeps the worker user's root steps done until ctx ends, and keeps its own binary
// on the latest verified release (then exits, so launchd starts the new one).
func runRootKeeper(ctx context.Context, user, home string) {
	r := install.ExecRunner{}
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	var warned repeatedWarning
	tick := func(n int) bool {
		if n%rootKeeperUpdateEvery == 0 && rootKeeperUpdatable(exe) {
			if updated, err := rootKeeperUpdate(ctx, r, exe); err != nil {
				slog.Warn("root-keeper: could not update its own grove; trying again later", "err", err)
			} else if updated {
				return false // launchd starts the new binary
			}
		}
		opts, err := install.RootKeeperOptions(ctx, r, user, home)
		if err == nil {
			var ran []string
			ran, err = install.KeepRootSteps(ctx, r, opts)
			if len(ran) > 0 && err == nil {
				slog.Info("root-keeper: did root steps", "user", user, "steps", strings.Join(ran, ", "))
			}
		}
		if err != nil {
			if warned.due(err.Error(), time.Now()) {
				slog.Warn("root-keeper: root steps not done", "user", user, "err", err)
			}
			return true
		}
		warned.clear()
		return true
	}
	timer := time.NewTimer(rootKeeperFirst)
	defer timer.Stop()
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !tick(n) {
			return
		}
		timer.Reset(rootKeeperTick)
	}
}

// rootKeeperUpdatable: only the installed keeper, a release build, replaces itself.
func rootKeeperUpdatable(exe string) bool {
	return exe == install.RootKeeperBin() && install.NewerVersion("v999999.0.0", Version)
}

// rootKeeperUpdate installs a newer release over the keeper's own binary, through the same
// verified path as every self-update (install.SelfUpdate: checksums, build provenance, version).
func rootKeeperUpdate(ctx context.Context, r install.Runner, exe string) (bool, error) {
	latest, err := install.LatestGroveRelease(ctx, r)
	if err != nil {
		return false, err
	}
	if !install.NewerVersion(latest, Version) {
		return false, nil
	}
	var log strings.Builder
	if err := install.SelfUpdate(ctx, r, install.Options{GroveVersion: Version}, exe, latest, &log); err != nil {
		return false, fmt.Errorf("%w (%s)", err, strings.TrimSpace(log.String()))
	}
	slog.Info("root-keeper: updated its own grove", "from", Version, "to", latest)
	return true, nil
}
