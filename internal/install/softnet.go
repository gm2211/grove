package install

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/fleet"
)

// softnetRootCommand is what an admin runs once per worker Mac so Softnet can become root by
// itself, the way `tart run --net-softnet` sets it up when run from a terminal. A
// `brew upgrade softnet` installs a fresh binary without the bit, so it has to be redone then.
const softnetRootCommand = `sudo chown root:wheel "$(realpath "$(command -v softnet)")" && ` +
	`sudo chmod u+s "$(realpath "$(command -v softnet)")"`

// softnetSteps install Softnet, the VM network filter fleet.yaml pools with network: isolated run
// on, and let it start as root. Orchard's worker starts it through Tart with no terminal to ask
// for a password, so without the second step every isolated VM on this Mac fails to boot.
func softnetSteps(r Runner, lookPath func(string) (string, error), out io.Writer) []Step {
	return []Step{
		{
			Name:        "softnet",
			Description: "Install Softnet (`brew install openai/tools/softnet`, falling back to `brew install cirruslabs/cli/softnet`), the VM network filter that fleet.yaml pools with network: isolated run on.",
			Check: func(ctx context.Context) (bool, error) {
				_, err := lookPath("softnet")
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
				if _, _, err := r.Run(ctx, "brew", "install", "openai/tools/softnet"); err == nil {
					return nil
				}
				if err := trustTap(ctx, r, tapCirruslabsCLI, out); err != nil {
					return err
				}
				_, _, err := r.Run(ctx, "brew", "install", "cirruslabs/cli/softnet")
				return err
			},
		},
		{
			Name:        "softnet-root",
			Privileged:  true,
			Description: softnetRootCommand + "   # isolated pools: Orchard starts Softnet with no terminal, so it must become root by itself; redo after `brew upgrade softnet`",
			// Only the SUID bit this step sets counts here: probing sudo belongs to `grove doctor`,
			// and the planner runs no sudo at all unless it is root with --yes.
			Check: func(ctx context.Context) (bool, error) {
				path, err := lookPath("softnet")
				return err == nil && softnetSUIDRoot(ctx, r, path), nil
			},
			Apply: func(ctx context.Context) error {
				path, err := lookPath("softnet")
				if err != nil {
					return fmt.Errorf("softnet is not installed: %w", err)
				}
				resolved, _, err := r.Run(ctx, "realpath", path)
				if err != nil {
					return fmt.Errorf("resolve %s: %w", path, err)
				}
				resolved = strings.TrimSpace(resolved)
				if _, _, err := r.Run(ctx, "sudo", "chown", "root:wheel", resolved); err != nil {
					return err
				}
				_, _, err = r.Run(ctx, "sudo", "chmod", "u+s", resolved)
				return err
			},
		},
	}
}

// softnetReady reports whether Softnet can start as root without a password prompt: either its
// binary is owned by root with the SUID bit set, or this user has a passwordless sudo rule for
// it (Softnet re-runs itself through `sudo --non-interactive` when it isn't root).
func softnetReady(ctx context.Context, r Runner, lookPath func(string) (string, error)) (bool, string) {
	path, err := lookPath("softnet")
	if err != nil {
		return false, "not installed"
	}

	if softnetSUIDRoot(ctx, r, path) {
		return true, path + ": SUID root"
	}
	if _, _, err := r.Run(ctx, "sudo", "-n", "-l", path); err == nil {
		return true, path + ": passwordless sudo"
	}
	return false, path + " can't become root without a password"
}

// softnetSUIDRoot reports whether the binary path resolves to is owned by root with the SUID bit.
func softnetSUIDRoot(ctx context.Context, r Runner, path string) bool {
	stdout, _, err := r.Run(ctx, "stat", "-L", "-f", "%u %p", path)
	return err == nil && suidRoot(stdout)
}

// suidRoot parses BSD `stat -f "%u %p"` output ("<owner uid> <octal mode>").
func suidRoot(statOutput string) bool {
	fields := strings.Fields(statOutput)
	if len(fields) != 2 || fields[0] != "0" {
		return false
	}
	mode, err := strconv.ParseUint(fields[1], 8, 32)
	return err == nil && mode&0o4000 != 0
}

// checkSoftnet reports whether this worker can start VMs for fleet.yaml pools with network:
// isolated. It only fails when this machine's fleet.yaml actually has such a pool; otherwise it's
// informational, since a worker Mac usually has no fleet.yaml of its own to tell.
func checkSoftnet(ctx context.Context, r Runner, opts Options, cfg *config.Config) CheckResult {
	ready, detail := softnetReady(ctx, r, opts.lookPath())
	required := fleetHasIsolatedPool(cfg)

	result := CheckResult{Name: "softnet", OK: ready || !required, Detail: detail}
	if !ready && !required {
		result.Detail += " (only needed for fleet.yaml pools with network: isolated)"
	}
	if !ready {
		result.Remediation = "run `grove install --role worker --softnet` and do its printed softnet-root step " +
			"(an admin password, once per Mac): " + softnetRootCommand + "; redo it after `brew upgrade softnet`"
	}
	return result
}

func fleetHasIsolatedPool(cfg *config.Config) bool {
	if cfg == nil || cfg.Fleet == "" {
		return false
	}
	spec, err := fleet.Load(cfg.Fleet)
	if err != nil {
		return false
	}
	for _, pool := range spec.Pools {
		if pool.Isolated() {
			return true
		}
	}
	return false
}
