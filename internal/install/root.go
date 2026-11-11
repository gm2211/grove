package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// shQuote single-quotes s for a POSIX shell: nothing inside is expanded, and an embedded single
// quote is closed, escaped and reopened. (Go's %q makes double quotes, where $ and ` still expand.)
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RootOptions controls RunRootSteps.
type RootOptions struct {
	// Interactive is whether stdin is a terminal, i.e. sudo can ask for a password.
	Interactive bool
	// Yes runs the script without asking first. Without a terminal it needs sudo credentials that
	// are already cached (or a NOPASSWD rule).
	Yes bool
	// NoSudo never runs sudo: the script is only printed, for an admin.
	NoSudo bool
	// Admin names an admin account to run the script through, from an account that has no sudo:
	// `su <Admin> -c '/usr/bin/sudo /bin/sh -e -c <script>'`, attached to the terminal. The script
	// travels in argv, never on stdin: macOS su reads the password from stdin, so a piped script
	// makes PAM fail at once ("su: Sorry") without a prompt, and the inner sudo needs the terminal
	// too. Needs a terminal (stdin is checked up front). Excludes NoSudo.
	Admin string
	In    io.Reader
	Out   io.Writer
}

// RootOutcome is what RunRootSteps did with the root steps.
type RootOutcome int

const (
	// RootNone: there was nothing to do.
	RootNone RootOutcome = iota
	// RootRan: the script ran through sudo and every step's check passes now.
	RootRan
	// RootPrinted: the script was printed for an admin to run; the steps are still to do.
	RootPrinted
)

// rootRecheckDelay is how long to wait between re-checks after the script ran: a LaunchDaemon that
// was just bootstrapped needs a moment to run its loader once.
var rootRecheckDelay = time.Second

const rootHeredocTag = "GROVE_ROOT_STEPS"

// RunRootSteps runs the needed privileged steps as ONE root script. If this user is an admin and
// sudo can ask for a password (a terminal), or already has credentials with --yes, the script is
// shown and, after a yes (or with --yes), run through a single `sudo`. Otherwise the same script
// is printed as one block for an admin to paste. Only the steps' Root text is ever run: Apply is
// for the process-is-root path in RunPlan.
func RunRootSteps(ctx context.Context, r Runner, steps []Step, opts RootOptions) (RootOutcome, error) {
	if len(steps) == 0 {
		return RootNone, nil
	}
	if opts.Admin != "" {
		if opts.NoSudo {
			return RootNone, errors.New("--admin and --no-sudo exclude each other")
		}
		if err := CheckAdminAccount(ctx, r, opts.Admin); err != nil {
			return RootNone, err
		}
		if !opts.Interactive {
			return RootNone, fmt.Errorf("--admin needs an interactive terminal for %s's password: run it from Terminal over `ssh -t`", opts.Admin)
		}
	}
	script := "set -e\n"
	for _, s := range steps {
		if s.Root == nil {
			return RootNone, fmt.Errorf("step %s has no root script", s.Name)
		}
		body, err := s.Root(ctx)
		if err != nil {
			return RootNone, fmt.Errorf("root script for %s: %w", s.Name, err)
		}
		script += "echo " + shQuote("grove: "+s.Name) + "\n" + strings.TrimRight(body, "\n") + "\n"
	}

	if opts.Admin != "" {
		fmt.Fprintf(opts.Out, "\n%d step(s) need root, run through %s's account (su, then sudo; both ask for %s's password):\n\n%s\n", len(steps), opts.Admin, opts.Admin, indent(script))
		if err := r.RunAttached(ctx, "/usr/bin/su", opts.Admin, "-c", adminSuCommand(script)); err != nil {
			return RootNone, fmt.Errorf("root steps through %s: %w", opts.Admin, err)
		}
		if err := recheckRootSteps(ctx, steps); err != nil {
			return RootNone, err
		}
		fmt.Fprintf(opts.Out, "root steps done (%d).\n", len(steps))
		return RootRan, nil
	}

	usable, reason := sudoUsable(ctx, r, opts)
	if !usable {
		printRootBlock(opts.Out, reason, script)
		return RootPrinted, nil
	}

	fmt.Fprintf(opts.Out, "\n%d step(s) need root:\n\n%s\n", len(steps), indent(script))
	if !opts.Yes {
		fmt.Fprintf(opts.Out, "Run these %d steps now with sudo? macOS will ask for your password once. [Y/n] ", len(steps))
		line, err := bufio.NewReader(opts.In).ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if (err != nil && answer == "") || !(answer == "" || answer == "y" || answer == "yes") {
			fmt.Fprintln(opts.Out)
			printRootBlock(opts.Out, "Not running them with sudo.", script)
			return RootPrinted, nil
		}
	}
	if err := r.RunAttached(ctx, "/usr/bin/sudo", "/bin/sh", "-e", "-c", script); err != nil {
		return RootNone, fmt.Errorf("root steps: %w", err)
	}
	if err := recheckRootSteps(ctx, steps); err != nil {
		return RootNone, err
	}
	fmt.Fprintf(opts.Out, "root steps done (%d).\n", len(steps))
	return RootRan, nil
}

// adminSuCommand is the command string for `su <admin> -c`: the admin's login shell parses it, so
// the script is shQuote'd (single quotes mean the same in sh, bash and zsh) and reaches
// `sh -e -c` intact. The script is Root texts only, which never hold a secret, so argv is fine.
func adminSuCommand(script string) string {
	return "/usr/bin/sudo /bin/sh -e -c " + shQuote(script)
}

// recheckRootSteps waits for every step's check to pass after the root script ran.
func recheckRootSteps(ctx context.Context, steps []Step) error {
	for _, s := range steps {
		var ok bool
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				time.Sleep(rootRecheckDelay)
			}
			if ok, err = s.Check(ctx); err != nil || ok {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("check %s after the root steps: %w", s.Name, err)
		}
		if !ok {
			return fmt.Errorf("%s is still not satisfied after the root steps ran", s.Name)
		}
	}
	return nil
}

// adminAccountRE is what an account name may contain; it is passed to su and dscl as an argument.
var adminAccountRE = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)

// CheckAdminAccount refuses an --admin account that does not exist or is not in the admin group.
func CheckAdminAccount(ctx context.Context, r Runner, name string) error {
	if !adminAccountRE.MatchString(name) || name == "root" {
		return fmt.Errorf("--admin %q is not an admin account name", name)
	}
	if _, _, err := r.Run(ctx, "/usr/bin/dscl", ".", "-read", "/Users/"+name, "UniqueID"); err != nil {
		return fmt.Errorf("--admin %s: no such account on this Mac", name)
	}
	if _, _, err := r.Run(ctx, "/usr/sbin/dseditgroup", "-o", "checkmember", "-m", name, "admin"); err != nil {
		return fmt.Errorf("--admin %s is not an admin (not in the admin group), so it cannot use sudo", name)
	}
	return nil
}

// PendingRootSteps builds opts's plan and returns its privileged steps that are not yet satisfied,
// in plan order, for RunRootSteps. It is how an admin's `grove setup --user` gets the root script
// for another user without running anything that user's process produced: every Root text comes
// from this process's own rendering of opts (that user's name and home, the controller and server
// the admin's join returned), and the plists in it are pinned by the SHA-256 of what this process
// renders. Nothing else in the plan is checked or applied.
//
// A worker's controller URL goes into the wrapper, an unprivileged step of the user's own, so opts
// may leave it empty (repairing an install whose wrapper this process can't read). The VM
// firewall's Nomad RPC pass then takes the control plane's address from the grove server URL
// (nomadRPCSource), which the root-owned broker plist names.
func PendingRootSteps(ctx context.Context, r Runner, opts Options) ([]Step, error) {
	if opts.Role == RoleWorker && opts.Controller == "" {
		opts.Controller = rootStepsControllerPlaceholder
	}
	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		return nil, err
	}
	var pending []Step
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

// sudoUsable reports whether the root script can go through sudo, and if not, why in a sentence.
func sudoUsable(ctx context.Context, r Runner, opts RootOptions) (bool, string) {
	if opts.NoSudo {
		return false, "--no-sudo: not running sudo."
	}
	if !userIsAdmin(ctx, r) {
		return false, "This user is not an admin, so it cannot use sudo."
	}
	if opts.Interactive {
		return true, ""
	}
	if !opts.Yes {
		return false, "No terminal to ask for a sudo password on (and no --yes)."
	}
	if _, _, err := r.Run(ctx, "/usr/bin/sudo", "-n", "/usr/bin/true"); err != nil {
		return false, "No terminal to ask for a sudo password on, and sudo has no cached credentials."
	}
	return true, ""
}

func printRootBlock(out io.Writer, reason, script string) {
	fmt.Fprintf(out, "\n%s An admin runs this one block, once:\n\nsudo /bin/sh -e <<'%s'\n%s%s\n\nthen re-run the same grove command.\n",
		reason, rootHeredocTag, script, rootHeredocTag)
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
