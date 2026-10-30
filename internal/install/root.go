package install

import (
	"bufio"
	"context"
	"fmt"
	"io"
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
	In     io.Reader
	Out    io.Writer
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
			return RootNone, fmt.Errorf("check %s after the root steps: %w", s.Name, err)
		}
		if !ok {
			return RootNone, fmt.Errorf("%s is still not satisfied after the root steps ran", s.Name)
		}
	}
	fmt.Fprintf(opts.Out, "root steps done (%d).\n", len(steps))
	return RootRan, nil
}

// sudoUsable reports whether the root script can go through sudo, and if not, why in a sentence.
func sudoUsable(ctx context.Context, r Runner, opts RootOptions) (bool, string) {
	if opts.NoSudo {
		return false, "--no-sudo: not running sudo."
	}
	stdout, _, err := r.Run(ctx, "/usr/bin/id", "-Gn")
	admin := false
	if err == nil {
		for _, g := range strings.Fields(stdout) {
			if g == "admin" {
				admin = true
			}
		}
	}
	if !admin {
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
