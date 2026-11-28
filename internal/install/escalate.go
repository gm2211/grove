package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
)

// Escalation: `grove doctor --fix` run by a worker user who is not an admin (argos) still does the
// root part itself. It asks an admin account for its password through `su`, and as that admin
// fetches this same grove release into a directory only the admin can write, checks the archive
// against the SHA-256 this process verified (the release's checksums.txt and its Sigstore build
// provenance), and runs `sudo ./grove doctor --fix --worker-user <this user>` with it. Root thus
// never runs a binary the worker user can write, and the operator types a password, never a
// command or a script.
//
// What it can't remove: the admin's password is typed into the worker user's terminal, so a
// worker user that is already compromised could capture it. That is the cost of not needing a
// second login; an admin who doesn't accept it runs `sudo grove doctor --fix` from their own.

// RootNeeded lists the privileged steps the worker user's own plan still needs, rendered as that
// user's install renders them.
func RootNeeded(ctx context.Context, r Runner) ([]string, error) {
	opts := Options{Role: RoleWorker}
	if AutoRootless(ctx, r, opts, io.Discard) {
		opts.Rootless = true
	}
	steps, err := PendingRootSteps(ctx, r, opts)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	return names, nil
}

// AdminAccounts are the admin group's members that a person logs in as (not root, not macOS's
// setup account).
func AdminAccounts(ctx context.Context, r Runner) ([]string, error) {
	out, _, err := r.Run(ctx, "/usr/bin/dscl", ".", "-read", "/Groups/admin", "GroupMembership")
	if err != nil {
		return nil, fmt.Errorf("list admin accounts: %w", err)
	}
	var admins []string
	for _, f := range strings.Fields(strings.TrimPrefix(strings.TrimSpace(out), "GroupMembership:")) {
		if f == "root" || strings.HasPrefix(f, "_") {
			continue
		}
		if _, err := user.Lookup(f); err == nil {
			admins = append(admins, f)
		}
	}
	return admins, nil
}

// escalationScript is what the admin's shell runs: fetch the archive, check its SHA-256, run its
// grove as root for worker. Every value is quoted; the script holds no secret.
func escalationScript(url, sum, worker string) string {
	return strings.Join([]string{
		"set -e",
		`d=$(/usr/bin/mktemp -d /private/tmp/grove-fix.XXXXXX)`,
		`trap '/bin/rm -rf "$d"' EXIT`,
		`cd "$d"`,
		"/usr/bin/curl -fsSL --proto =https --tlsv1.2 -o grove.tar.gz " + shQuote(url),
		`[ "$(/usr/bin/shasum -a 256 grove.tar.gz | /usr/bin/cut -d' ' -f1)" = ` + shQuote(sum) + ` ] || { echo 'the downloaded grove is not the release grove verified; stopping' >&2; exit 1; }`,
		"/usr/bin/tar -xzf grove.tar.gz grove",
		"echo 'sudo asks for this admin password once more:'",
		"/usr/bin/sudo ./grove doctor --fix --worker-user " + shQuote(worker),
	}, "\n") + "\n"
}

// EscalateAsAdmin does the root part of `grove doctor --fix` for this (non-admin) worker user
// through admin's account, on this terminal.
func EscalateAsAdmin(ctx context.Context, r Runner, groveVersion, admin string, out io.Writer) error {
	opts := Options{GroveVersion: groveVersion}
	if !hasReleaseVersion(opts) {
		return errors.New("this grove is a development build: run a release's `sudo grove doctor --fix` from an admin account")
	}
	me, err := user.Current()
	if err != nil {
		return err
	}
	asset := fmt.Sprintf("grove_%s_%s.tar.gz", opts.goos(), opts.goarch())
	fmt.Fprintf(out, "Checking grove %s's release (checksums and build provenance)...\n", groveVersion)
	staged, d, err := fetchReleaseAsset(ctx, r, opts, asset)
	if err != nil {
		return err
	}
	_ = os.Remove(staged)
	script := escalationScript(d.URL, d.SHA256, me.Username)
	fmt.Fprintf(out, "\nThe rest needs root, through the admin account %s. Type %s's password (su asks first, then sudo):\n", admin, admin)
	if err := r.RunAttached(ctx, "/usr/bin/su", "-", admin, "-c", "/bin/sh -c "+shQuote(script)); err != nil {
		return fmt.Errorf("the root part through %s: %w", admin, err)
	}
	return nil
}
