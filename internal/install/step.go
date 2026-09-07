package install

import (
	"context"
	"fmt"
	"io"
)

// Step is one idempotent unit of the install plan: check whether it's already satisfied, and if
// not, apply it. Steps are data (not closures over *testing.T etc.) so a planner can be built and
// printed without a live environment.
type Step struct {
	// Name is a short stable identifier, e.g. "homebrew", "tart", "launchagent:orchard-worker".
	Name string
	// Description is a human sentence describing what Apply does, shown in --dry-run and normal
	// output ("Install tart via `brew install openai/tools/tart`.").
	Description string
	// Privileged steps (sudo, launchctl bootstrap of a LaunchDaemon, disablesleep, …) are never
	// run automatically: Apply only prints the commands for a human (or Claude) to run, even with
	// --yes, unless the process is actually root. See Step.Privileged.
	Privileged bool
	// Check reports whether the step is already satisfied (Apply would be a no-op).
	Check func(ctx context.Context) (bool, error)
	// Apply performs the step. Only called when Check returned false and (dry-run is off).
	Apply func(ctx context.Context) error
}

// StepResult records what happened when a Step was planned/run.
type StepResult struct {
	Step    string
	Desc    string
	Needed  bool // Check() returned false, i.e. Apply would do something
	Applied bool // Apply was actually invoked
	Skipped bool // needed but not applied (dry-run, or privileged without --yes/root)
	Err     error
}

// PlanOptions controls how RunPlan executes a list of steps.
type PlanOptions struct {
	DryRun bool
	// Yes allows non-privileged steps to be applied without further confirmation (all Apply calls
	// in this package are already non-interactive; Yes mainly gates privileged steps together
	// with IsRoot).
	Yes bool
	// IsRoot should reflect whether the current process is running as root (uid 0). Privileged
	// steps only ever Apply when both Yes and IsRoot are true; otherwise they print instructions.
	IsRoot bool
	Out    io.Writer
}

// RunPlan walks steps in order, checking then (maybe) applying each, and writes a human-readable
// transcript to opts.Out. It never stops early on a failed step, so the caller gets the full
// picture; the returned error is non-nil if any step failed to apply.
func RunPlan(ctx context.Context, steps []Step, opts PlanOptions) ([]StepResult, error) {
	var results []StepResult
	var firstErr error
	for _, s := range steps {
		r := StepResult{Step: s.Name, Desc: s.Description}
		ok, err := s.Check(ctx)
		if err != nil {
			r.Err = fmt.Errorf("check %s: %w", s.Name, err)
			results = append(results, r)
			if firstErr == nil {
				firstErr = r.Err
			}
			fmt.Fprintf(opts.Out, "[ERROR] %-28s %v\n", s.Name, r.Err)
			continue
		}
		if ok {
			fmt.Fprintf(opts.Out, "[ok]    %-28s already satisfied\n", s.Name)
			continue
		}
		r.Needed = true
		if opts.DryRun {
			r.Skipped = true
			fmt.Fprintf(opts.Out, "[plan]  %-28s %s\n", s.Name, s.Description)
			results = append(results, r)
			continue
		}
		if s.Privileged && !(opts.Yes && opts.IsRoot) {
			r.Skipped = true
			fmt.Fprintf(opts.Out, "[manual]%-28s %s\n", s.Name, s.Description)
			results = append(results, r)
			continue
		}
		fmt.Fprintf(opts.Out, "[apply] %-28s %s\n", s.Name, s.Description)
		if err := s.Apply(ctx); err != nil {
			r.Err = fmt.Errorf("apply %s: %w", s.Name, err)
			if firstErr == nil {
				firstErr = r.Err
			}
			fmt.Fprintf(opts.Out, "[FAIL]  %-28s %v\n", s.Name, r.Err)
			results = append(results, r)
			continue
		}
		r.Applied = true
		results = append(results, r)
	}
	return results, firstErr
}
