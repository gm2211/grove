package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	// tapMinIOStable serves the MinIO server binary.
	tapMinIOStable = thirdPartyTap{
		Name:   "minio/stable",
		Reason: "MinIO, the official minio/stable tap",
	}
)

// requiredTaps is every third-party tap grove might install from, across all roles. Used by
// `grove doctor` to audit trust status; each build*Steps planner only wires up the subset it
// actually installs from.
var requiredTaps = []thirdPartyTap{tapOpenAITools, tapCirruslabsCLI, tapHashicorp, tapMinIOStable}

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
	stdout, _, err := r.Run(ctx, "brew", "tap-info", "--json=v1", tap)
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

// trustTap taps (if needed — `brew tap` on an already-tapped tap is a no-op) then trusts tap,
// printing an explanation to out first so a human reading `grove install`'s output understands why
// grove is about to mutate Homebrew's trust store. It treats `brew trust` being unknown entirely
// (older Homebrew, no trust gate to satisfy) as success rather than a failure.
func trustTap(ctx context.Context, r Runner, tap thirdPartyTap, out io.Writer) error {
	fmt.Fprintf(out, "        Homebrew requires explicit trust for third-party taps; trusting %s (%s).\n",
		tap.Name, tap.Reason)
	if _, _, err := r.Run(ctx, "brew", "tap", tap.Name); err != nil {
		return fmt.Errorf("brew tap %s: %w", tap.Name, err)
	}
	if _, _, err := r.Run(ctx, "brew", "trust", tap.Name); err != nil {
		if isUnknownBrewCommand(err) {
			return nil
		}
		return fmt.Errorf("brew trust %s: %w", tap.Name, err)
	}
	return nil
}

// trustTapStep returns the idempotent "trust this tap" Step that a planner inserts immediately
// before whatever step installs from tap. Check treats an inconclusive tap-info result (e.g. brew
// not on PATH yet, or an ancient brew whose tap-info output this can't parse) as "not yet
// satisfied" rather than erroring the whole plan — Apply then gets a chance to run and, on a brew
// old enough not to have `trust` at all, self-heals via the "Unknown command" fallback above.
func trustTapStep(r Runner, tap thirdPartyTap, out io.Writer) Step {
	return Step{
		Name: "brew-trust:" + tap.Name,
		Description: fmt.Sprintf(
			"Homebrew requires explicit trust for third-party taps; trust %s (%s) so `brew install` from it "+
				"isn't refused (`brew tap %s` if not already tapped, then `brew trust %s`).",
			tap.Name, tap.Reason, tap.Name, tap.Name,
		),
		Check: func(ctx context.Context) (bool, error) {
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
