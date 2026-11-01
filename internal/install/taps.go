package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/user"
	"strings"
)

// Recent Homebrew (see `brew trust --help`) refuses to load any formula/cask from a non-official
// tap until that tap (or the specific formula) has been explicitly trusted:
//
//	Error: Refusing to load formula openai/tools/softnet from untrusted tap openai/tools.
//	Run `brew trust --formula openai/tools/softnet` or `brew trust openai/tools` to trust it.
//
// grove installs from three such taps. thirdPartyTap + trustTapStep/trustTap below make trusting
// each one an idempotent, Runner-mediated step so `grove install` doesn't hit this gate on any
// recent Homebrew, and so --dry-run/tests can see it happen without ever shelling out for real.
type thirdPartyTap struct {
	// Name is the tap's "user/repo" reference, e.g. "openai/tools" — what both `brew tap` and
	// `brew trust` accept as a bare positional argument.
	Name string
	// Reason is a short human-readable explanation of what the tap is for for the log line printed
	// before trusting it, e.g. "Tart, official tap of the Cirrus Labs / OpenAI virtualization
	// tools".
	Reason string
}

var (
	// tapOpenAITools is Tart's canonical tap since Cirrus Labs joined OpenAI.
	tapOpenAITools = thirdPartyTap{
		Name:   "openai/tools",
		Reason: "Tart, official tap of the Cirrus Labs / OpenAI virtualization tools",
	}
	// tapCirruslabsCLI is Tart's original tap, used as a fallback if openai/tools doesn't have it
	// yet on a given host's Homebrew.
	tapCirruslabsCLI = thirdPartyTap{
		Name:   "cirruslabs/cli",
		Reason: "Tart, legacy Cirrus Labs tap kept as a fallback",
	}
	// tapHashicorp serves Nomad (and Packer, if grove ever installs that too).
	tapHashicorp = thirdPartyTap{
		Name:   "hashicorp/tap",
		Reason: "Nomad, HashiCorp's official tap",
	}
)

// requiredTaps is every third-party tap grove might install from, across all roles. Used by
// `grove doctor` to audit trust status; each build*Steps planner only wires up the subset it
// actually installs from.
var requiredTaps = []thirdPartyTap{tapOpenAITools, tapCirruslabsCLI, tapHashicorp}

// tapInfo is the subset of `brew tap-info --json=v1 <tap>`'s per-tap object grove cares about.
// Unlike `brew trust --list` (which doesn't exist — see `brew trust --help`; the closest is bare
// `brew trust`, which prints a human-readable listing, not JSON, of every trusted tap/formula
// across ALL taps), tap-info's own "trusted" field reports the resolved trust state for exactly
// the one tap asked about, official-tap implicit trust included. That's the more precise and
// simpler signal, so Check below queries tap-info instead of parsing `brew trust`'s listing.
type tapInfo struct {
	Installed bool `json:"installed"`
	Trusted   bool `json:"trusted"`
}

// fetchTapInfo runs `brew tap-info --json=v1 <tap>` and decodes the first (only) result. It
// returns an error for anything that stops it from getting a confident answer — brew missing,
// unexpected output, and so on — callers decide how to treat that.
func fetchTapInfo(ctx context.Context, r Runner, tap string) (tapInfo, error) {
	// A read-only query must not try to refresh Homebrew's API data or its taps: on a shared
	// Homebrew (owned by another user) that needs write access to the prefix's locks and fails for
	// everyone but the owner, which made a trusted tap look untrusted.
	stdout, _, err := r.Run(ctx, "env", "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALL_FROM_API=1",
		"brew", "tap-info", "--json=v1", tap)
	if err != nil {
		return tapInfo{}, fmt.Errorf("brew tap-info --json=v1 %s: %w", tap, err)
	}
	var infos []tapInfo
	if err := json.Unmarshal([]byte(stdout), &infos); err != nil {
		return tapInfo{}, fmt.Errorf("parse brew tap-info --json=v1 %s output: %w", tap, err)
	}
	if len(infos) == 0 {
		return tapInfo{}, fmt.Errorf("brew tap-info --json=v1 %s: no results", tap)
	}
	return infos[0], nil
}

// isUnknownBrewCommand reports whether err looks like Homebrew rejecting `trust` as a command it
// doesn't know, i.e. this Homebrew predates the trust gate entirely and there's nothing to do.
// ExecRunner folds stderr into the returned error's text (see runner.go), and Homebrew's own
// message for this is "Error: Invalid usage: Unknown command: brew trust ..." — "Unknown command"
// is the stable substring across Homebrew versions/wording tweaks around it.
func isUnknownBrewCommand(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Unknown command")
}

// trustTap taps (if needed) then trusts tap, printing an explanation to out first so a human
// reading `grove install`'s output understands why grove is about to mutate Homebrew's trust store.
// It treats `brew trust` being unknown entirely (older Homebrew, no trust gate to satisfy) as
// success rather than a failure.
//
// Trust is per user (~/.homebrew/trust.json), but tapping writes the Homebrew prefix. So a tap that
// is already tapped is only trusted, which works on a Homebrew another user owns (a worker user on
// an admin's Homebrew); only an untapped one needs the prefix to be this user's. And `brew trust`
// can record the trust and still exit non-zero (on a shared Homebrew it then fails to refresh API
// data it has no write access to), so a failure is checked against tap-info before it counts.
func trustTap(ctx context.Context, r Runner, tap thirdPartyTap, out io.Writer) error {
	info, infoErr := fetchTapInfo(ctx, r, tap.Name)
	tapped := infoErr == nil && info.Installed
	if !tapped {
		if err := checkBrewWritable(ctx, r, tap); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "        Homebrew requires explicit trust for third-party taps; trusting %s (%s).\n",
		tap.Name, tap.Reason)
	if !tapped {
		if _, _, err := r.Run(ctx, "brew", "tap", tap.Name); err != nil {
			return fmt.Errorf("brew tap %s: %w", tap.Name, err)
		}
	}
	if _, _, err := r.Run(ctx, "brew", "trust", tap.Name); err != nil {
		if isUnknownBrewCommand(err) {
			return nil
		}
		if after, infoErr := fetchTapInfo(ctx, r, tap.Name); infoErr == nil && after.Installed && after.Trusted {
			return nil
		}
		return fmt.Errorf("brew trust %s: %w", tap.Name, err)
	}
	return nil
}

// checkBrewWritable refuses to run `brew tap` when the Homebrew prefix belongs to another user:
// tapping writes to the prefix and could only fail halfway with a permission error. It is lenient
// the other way too: if the prefix or its owner can't be determined, it lets brew try.
func checkBrewWritable(ctx context.Context, r Runner, tap thirdPartyTap) error {
	prefix, owner, cur, foreign := brewForeignOwner(ctx, r)
	if !foreign {
		return nil
	}
	return fmt.Errorf("Homebrew at %s belongs to %s, not %s, so grove cannot tap %s here; "+
		"as %s run: brew tap %s, then run this again (grove then trusts it for %s)",
		prefix, owner, cur, tap.Name, owner, tap.Name, cur)
}

// brewForeignOwner reports whether the Homebrew prefix belongs to a user other than the current
// one, which is then unable to install into it. Lenient: if the prefix or its owner can't be
// determined, foreign is false and brew gets to try.
func brewForeignOwner(ctx context.Context, r Runner) (prefix, owner, current string, foreign bool) {
	prefixOut, _, err := r.Run(ctx, "brew", "--prefix")
	prefix = strings.TrimSpace(prefixOut)
	if err != nil || prefix == "" {
		return "", "", "", false
	}
	ownerOut, _, err := r.Run(ctx, "stat", "-f", "%Su", prefix)
	owner = strings.TrimSpace(ownerOut)
	if err != nil || owner == "" {
		return "", "", "", false
	}
	cur, err := user.Current()
	if err != nil || cur.Username == owner {
		return "", "", "", false
	}
	return prefix, owner, cur.Username, true
}

// trustTapStep returns the idempotent "trust this tap" Step that a planner inserts immediately
// before whatever step installs from tap. Check treats an inconclusive tap-info result (e.g. brew
// not on PATH yet, or an ancient brew whose tap-info output this can't parse) as "not yet
// satisfied" rather than erroring the whole plan — Apply then gets a chance to run and, on a brew
// old enough not to have `trust` at all, self-heals via the "Unknown command" fallback above.
func trustTapStep(r Runner, tap thirdPartyTap, out io.Writer) Step {
	return trustTapStepWhen(r, tap, out, nil)
}

// trustTapStepWhen is trustTapStep for a tap that is only trusted to install something: needed
// says whether that install is still pending. When it reports false (the binaries the tap gates
// are already on PATH, however they got there) the step is satisfied without asking Homebrew
// anything, so a Mac whose Homebrew another user owns, or whose Tart never came from Homebrew,
// does not fail a step it has no use for. A nil needed means always needed.
func trustTapStepWhen(r Runner, tap thirdPartyTap, out io.Writer, needed func() bool) Step {
	return Step{
		Name: "brew-trust:" + tap.Name,
		Description: fmt.Sprintf(
			"Homebrew requires explicit trust for third-party taps; trust %s (%s) so `brew install` from it "+
				"isn't refused (`brew tap %s` if not already tapped, then `brew trust %s`).",
			tap.Name, tap.Reason, tap.Name, tap.Name,
		),
		Check: func(ctx context.Context) (bool, error) {
			if needed != nil && !needed() {
				return true, nil
			}
			info, err := fetchTapInfo(ctx, r, tap.Name)
			if err != nil {
				return false, nil
			}
			return info.Installed && info.Trusted, nil
		},
		Apply: func(ctx context.Context) error {
			return trustTap(ctx, r, tap, out)
		},
	}
}

// checkTrustedTaps is `grove doctor`'s audit of requiredTaps: it flags any tap that's actually
// tapped on this machine (i.e. something already relies on it) but not trusted, with the exact
// remediation command. A tap that was never tapped isn't "required" yet, so it isn't reported —
// mirroring how the rest of doctor only complains about things that are configured but unhealthy.
// It's a no-op on non-macOS, where Homebrew (and so this whole trust gate) doesn't exist.
func checkTrustedTaps(ctx context.Context, r Runner, opts Options) []CheckResult {
	if opts.goos() != "darwin" {
		return nil
	}
	var results []CheckResult
	for _, tap := range requiredTaps {
		info, err := fetchTapInfo(ctx, r, tap.Name)
		if err != nil || !info.Installed {
			continue
		}
		name := "brew-trust:" + tap.Name
		if info.Trusted {
			results = append(results, CheckResult{Name: name, OK: true, Detail: "trusted"})
			continue
		}
		results = append(results, CheckResult{
			Name:        name,
			OK:          false,
			Detail:      fmt.Sprintf("tap %s is installed but not trusted; Homebrew will refuse to load its formulae", tap.Name),
			Remediation: fmt.Sprintf("brew trust %s", tap.Name),
		})
	}
	return results
}

// onPath reports whether name resolves through lookPath.
func onPath(lookPath func(string) (string, error), name string) bool {
	_, err := lookPath(name)
	return err == nil
}
