package cli

// `grove uninstall --role worker`: undo a worker install on this Mac. The root steps (the three
// LaunchDaemons, the pf anchor, optionally sleep) run first, as one root script through one sudo or
// a block printed for an admin; then the worker user's own files go. With --user, an admin does it
// for a standard worker user like `grove setup --user`: this process renders the root script itself
// (it names only fixed system paths), then runs the user's half (`grove uninstall --from-admin`)
// through `sudo -n -u`. Nothing the worker user's process prints or writes becomes root script text.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/install"
)

var uninstallFlags struct {
	role         string
	user         string
	purge        bool
	restoreSleep bool
	dryRun       bool
	yes          bool
	noSudo       bool
	fromAdmin    bool
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove a worker install from this Mac (LaunchDaemons, VM firewall, the worker's files).",
	Long: `grove uninstall --role worker undoes grove install --role worker / grove setup on this Mac.

As root, in ONE script through a single sudo (or printed as one block for an admin, after which you
re-run the same command): boot out and remove the Orchard worker, broker and VM firewall
LaunchDaemons from /Library/LaunchDaemons, and flush the pf anchor com.apple/grove-vm. With
--restore-sleep it also runs pmset -a disablesleep 0.

Then, as the worker user: the staged plists, the credential files, the Orchard wrapper, the Grove
Orchard Worker app, the ~/.local/bin/orchard symlink into it, and the worker's logs. config.yaml
stays unless --purge. Homebrew, Tart and Tart's VM images are left alone.

From an admin account, --user <name> does all of it for a standard worker user with one sudo
password prompt. --dry-run lists every step and changes nothing; re-running after a full uninstall
does nothing. The control plane is not told: grove prints the commands to run there.`,
	RunE: runUninstall,
}

func init() {
	uninstallCmd.Flags().StringVar(&uninstallFlags.role, "role", "", "worker (the only role uninstall supports)")
	uninstallCmd.Flags().StringVar(&uninstallFlags.user, "user", "", "from an admin account: uninstall the worker that runs as this standard user")
	uninstallCmd.Flags().BoolVar(&uninstallFlags.purge, "purge", false, "also remove config.yaml (the joined control plane and this Mac's client credential)")
	uninstallCmd.Flags().BoolVar(&uninstallFlags.restoreSleep, "restore-sleep", false, "also let the Mac sleep again (pmset -a disablesleep 0)")
	uninstallCmd.Flags().BoolVar(&uninstallFlags.dryRun, "dry-run", false, "list the steps; change nothing")
	uninstallCmd.Flags().BoolVar(&uninstallFlags.yes, "yes", false, "run the root steps through sudo without asking")
	uninstallCmd.Flags().BoolVar(&uninstallFlags.noSudo, "no-sudo", false, "never run sudo; print the root steps for an admin")
	// --from-admin is how `grove uninstall --user` runs the worker user's half.
	uninstallCmd.Flags().BoolVar(&uninstallFlags.fromAdmin, "from-admin", false, "")
	_ = uninstallCmd.Flags().MarkHidden("from-admin")
	_ = uninstallCmd.MarkFlagRequired("role")
	Root.AddCommand(uninstallCmd)
}

func runUninstall(cmd *cobra.Command, args []string) error {
	if install.Role(uninstallFlags.role) != install.RoleWorker {
		return fmt.Errorf("--role must be %q: only a worker install can be uninstalled (got %q)", install.RoleWorker, uninstallFlags.role)
	}
	if runtimeOS() != "darwin" {
		return errors.New("`grove uninstall --role worker` removes a worker Mac's install; this is not a Mac")
	}
	u := install.UninstallOptions{RestoreSleep: uninstallFlags.restoreSleep, Purge: uninstallFlags.purge}
	r := install.ExecRunner{}
	if uninstallFlags.fromAdmin {
		return runUninstallFromAdmin(cmd, r, u)
	}
	if uninstallFlags.user != "" {
		if uninstallFlags.noSudo {
			return errors.New("--user runs the root steps through sudo; drop --no-sudo, or run `grove uninstall --role worker` as that user")
		}
		t, err := lookupWorkerTarget(cmd.Context(), r, uninstallFlags.user)
		if err != nil {
			return err
		}
		return uninstallForUser(cmd, r, t, u)
	}
	if os.Geteuid() == 0 {
		return errors.New("run `grove uninstall --role worker` as the worker user, or from an admin account with --user <name>, not as root")
	}
	opts := install.Options{Role: install.RoleWorker}
	name := install.InstalledWorkerName(opts) // before the wrapper that records it is removed
	outcome, err := uninstallHere(cmd.Context(), r, opts, u, uninstallFlags.dryRun, install.RootOptions{
		Interactive: stdinIsTerminal(), Yes: uninstallFlags.yes, NoSudo: uninstallFlags.noSudo,
		In: cmd.InOrStdin(), Out: cmd.OutOrStdout(),
	})
	if err != nil || outcome == install.RootPrinted {
		return err
	}
	printForgetWorker(cmd.OutOrStdout(), name)
	return nil
}

// uninstallHere runs the plan as the worker user: the root steps first (one sudo, or a printed
// block), then, once they are done, the user's own steps. While the root block is still to be run
// the user's files stay, so the daemons don't restart into a missing wrapper or credential.
func uninstallHere(ctx context.Context, r install.Runner, opts install.Options, u install.UninstallOptions, dryRun bool, ro install.RootOptions) (install.RootOutcome, error) {
	steps, err := install.BuildUninstallPlan(r, opts, u)
	if err != nil {
		return install.RootNone, err
	}
	fmt.Fprintf(ro.Out, "grove uninstall --role worker (%d steps%s)\n", len(steps), dryRunSuffix(dryRun))
	if dryRun {
		_, err := install.RunPlan(ctx, steps, install.PlanOptions{DryRun: true, Out: ro.Out})
		return install.RootNone, err
	}
	pending, err := pendingPrivileged(ctx, steps)
	if err != nil {
		return install.RootNone, err
	}
	outcome, err := install.RunRootSteps(ctx, r, pending, ro)
	if err != nil {
		return outcome, fmt.Errorf("uninstall: %w", err)
	}
	if outcome == install.RootPrinted {
		fmt.Fprintln(ro.Out, "The worker's own files stay until the root steps are done.")
		return outcome, nil
	}
	if _, err := install.RunPlan(ctx, unprivileged(steps), install.PlanOptions{Out: ro.Out}); err != nil {
		return outcome, fmt.Errorf("uninstall: %w", err)
	}
	fmt.Fprintln(ro.Out, "\nThe worker is removed from this Mac.")
	return outcome, nil
}

// pendingPrivileged is steps' privileged steps whose check fails, in order.
func pendingPrivileged(ctx context.Context, steps []install.Step) ([]install.Step, error) {
	var pending []install.Step
	for _, s := range steps {
		if !s.Privileged {
			continue
		}
		ok, err := s.Check(ctx)
		if err != nil {
			return nil, fmt.Errorf("check %s: %w", s.Name, err)
		}
		if !ok {
			pending = append(pending, s)
		}
	}
	return pending, nil
}

func unprivileged(steps []install.Step) []install.Step {
	var out []install.Step
	for _, s := range steps {
		if !s.Privileged {
			out = append(out, s)
		}
	}
	return out
}

func privileged(steps []install.Step) []install.Step {
	var out []install.Step
	for _, s := range steps {
		if s.Privileged {
			out = append(out, s)
		}
	}
	return out
}

// uninstallForUser is the admin's side of --user: one sudo prompt, the root steps this process
// renders, then the worker user's half through `sudo -n -u`.
func uninstallForUser(cmd *cobra.Command, r install.Runner, t *workerTarget, u install.UninstallOptions) error {
	stop, err := startSudoSession(cmd.Context(), r, stdinIsTerminal(), uninstallFlags.yes)
	if err != nil {
		return err
	}
	defer stop()
	outcome, err := uninstallForUserSteps(cmd.Context(), r, t, u, uninstallFlags.dryRun, install.RootOptions{
		Interactive: stdinIsTerminal(), Yes: uninstallFlags.yes, In: cmd.InOrStdin(), Out: cmd.OutOrStdout(),
	})
	if err != nil {
		return err
	}
	if outcome == install.RootPrinted {
		return fmt.Errorf("the root steps are not done yet: run the block printed above, then `grove uninstall --role worker --user %s` again", t.User)
	}
	return nil
}

func uninstallForUserSteps(ctx context.Context, r install.Runner, t *workerTarget, u install.UninstallOptions, dryRun bool, ro install.RootOptions) (install.RootOutcome, error) {
	if t.User == "" || t.User == "root" || t.GroveBin == "" {
		return install.RootNone, errors.New("uninstall --user: no worker user or grove binary")
	}
	opts := t.options()
	opts.Role = install.RoleWorker
	steps, err := install.BuildUninstallPlan(r, opts, u)
	if err != nil {
		return install.RootNone, err
	}
	outcome := install.RootNone
	if dryRun {
		fmt.Fprintf(ro.Out, "grove uninstall --role worker --user %s: root steps (dry-run)\n", t.User)
		if _, err := install.RunPlan(ctx, privileged(steps), install.PlanOptions{DryRun: true, Out: ro.Out}); err != nil {
			return install.RootNone, err
		}
	} else {
		pending, err := pendingPrivileged(ctx, steps)
		if err != nil {
			return install.RootNone, err
		}
		if outcome, err = install.RunRootSteps(ctx, r, pending, ro); err != nil {
			return outcome, fmt.Errorf("uninstall: %w", err)
		}
		if outcome == install.RootPrinted {
			return outcome, nil
		}
	}
	args := []string{"-n", "-u", t.User, "-H", "--", t.GroveBin, "uninstall", "--role", "worker", "--from-admin"}
	if u.Purge {
		args = append(args, "--purge")
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	fmt.Fprintf(ro.Out, "\nRemoving the worker's files as %s:\n\n", t.User)
	if err := r.RunStreaming(ctx, "", "/usr/bin/sudo", args...); err != nil {
		return outcome, fmt.Errorf("removing the worker's files as %s failed (see above): %w", t.User, err)
	}
	return outcome, nil
}

// runUninstallFromAdmin is the worker user's half of --user: only the unprivileged steps, which the
// admin's grove runs after its own root steps. It names the worker for the control plane, since
// only this user can read the wrapper that records the name.
func runUninstallFromAdmin(cmd *cobra.Command, r install.Runner, u install.UninstallOptions) error {
	if os.Geteuid() == 0 {
		return errors.New("--from-admin runs as the worker user, never root")
	}
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Chdir(home) // sudo leaves the admin's working directory, which this user may not read
	}
	opts := install.Options{Role: install.RoleWorker}
	name := install.InstalledWorkerName(opts)
	steps, err := install.BuildUninstallPlan(r, opts, u)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if _, err := install.RunPlan(cmd.Context(), unprivileged(steps), install.PlanOptions{DryRun: uninstallFlags.dryRun, Out: out}); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	if !uninstallFlags.dryRun {
		fmt.Fprintln(out, "\nThe worker is removed from this Mac.")
	}
	printForgetWorker(out, name)
	return nil
}

// printForgetWorker says how to make the control plane forget this worker. There is no API a
// worker can call for it (revoking device credentials and deleting Orchard records need the
// operator), so it is printed for the operator to run on the control plane.
func printForgetWorker(out io.Writer, name string) {
	if name == "" {
		name = "<worker-name>"
	}
	fmt.Fprintf(out, `
The control plane still knows this worker. To forget it, run on the control plane as its operator:

  orchard delete worker %[1]s
  orchard list service-accounts          # then: orchard delete service-account grove-worker-<name>-<suffix>
  grove access list                      # then: grove access revoke <id>, for each device named %[1]s
`, shQuoteArg(name))
}

// shQuoteArg quotes name for a shell line only when it needs it.
func shQuoteArg(s string) string {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
