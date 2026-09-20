// Package install implements `grove install`'s role bootstrap planner and the checks behind
// `grove doctor`. Every command that could touch the host (brew, launchctl, sudo, tailscale, …)
// goes through the Runner interface so the planner can be exercised in tests (FakeRunner) and so
// --dry-run never executes anything.
package install

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes external commands. The real implementation shells out; tests use FakeRunner.
type Runner interface {
	// Run executes name with args and returns captured stdout/stderr. err is non-nil on a
	// non-zero exit or if the command could not be started.
	Run(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)
	// RunWithStdin is like Run but feeds stdin to the process's standard input, for commands that
	// read a secret from stdin rather than accept it as an argument (e.g. `tart login <registry>
	// --password-stdin`) so the secret never lands in argv, a process listing, or a shell history —
	// and, by construction, never in the *args passed to a logged/rendered command line either.
	RunWithStdin(ctx context.Context, stdin string, name string, args ...string) (stdout string, stderr string, err error)
	// RunWithStdinAttached is reserved for a trusted terminal bridge that must remain in the
	// caller's macOS authorization session. The bridge must prevent stdin from reaching or being
	// echoed by the child command directly. StoreKeychainSecret supplies that bridge with expect.
	RunWithStdinAttached(ctx context.Context, stdin string, name string, args ...string) (stdout string, stderr string, err error)
}

// ExecRunner is the real Runner, shelling out via os/exec.
type ExecRunner struct{}

var _ Runner = ExecRunner{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	return execRun(ctx, "", false, name, args...)
}

func (ExecRunner) RunWithStdin(ctx context.Context, stdin string, name string, args ...string) (string, string, error) {
	return execRun(ctx, stdin, true, name, args...)
}

func (ExecRunner) RunWithStdinAttached(ctx context.Context, stdin string, name string, args ...string) (string, string, error) {
	// Keychain Authorization Services requires the child to remain attached to a real terminal.
	// expect disables child output before receiving stdin, so attaching its output to /dev/tty
	// cannot echo the supplied secret. This path is intentionally separate from captured runners.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", "", fmt.Errorf("open interactive terminal for Keychain authorization: %w", err)
	}
	defer tty.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout = tty
	cmd.Stderr = tty
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return "", "", nil
}

func execRun(ctx context.Context, stdin string, detach bool, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		// Some macOS tools (notably `security ... -w`) prefer the controlling terminal over
		// cmd.Stdin. Detach secret-consuming children from that terminal so they must consume the
		// provided pipe. This also prevents interactive prompts from hanging unattended setup.
		if detach {
			configureSecretProcess(cmd)
		}
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), err
}

// Call records one Run/RunWithStdin invocation, for assertions in tests. Stdin is empty for a
// plain Run call. String() (and so FakeRunner's Script/CalledWith keying) deliberately ignores
// Stdin, so a secret fed via RunWithStdin never needs to appear in a scripted command line.
type Call struct {
	Name  string
	Args  []string
	Stdin string
}

// String renders the call the way it would appear on a shell line, e.g. "brew install tart".
func (c Call) String() string {
	if len(c.Args) == 0 {
		return c.Name
	}
	return c.Name + " " + strings.Join(c.Args, " ")
}

// FakeResult is a canned response for one command (matched by name+args prefix, see FakeRunner).
type FakeResult struct {
	Stdout string
	Stderr string
	Err    error
}

// FakeRunner is a Runner for tests: it records every call and returns a scripted result, keyed by
// the command line joined with spaces. Unscripted commands succeed with empty output by default,
// unless NoDefault is set, in which case they return an error.
type FakeRunner struct {
	Calls     []Call
	Results   map[string]FakeResult
	NoDefault bool
}

var _ Runner = (*FakeRunner)(nil)

// NewFakeRunner returns an empty FakeRunner.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{Results: map[string]FakeResult{}}
}

// Script registers the result for an exact "name arg1 arg2 ..." command line.
func (f *FakeRunner) Script(cmdLine string, result FakeResult) {
	if f.Results == nil {
		f.Results = map[string]FakeResult{}
	}
	f.Results[cmdLine] = result
}

func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	return f.run(Call{Name: name, Args: args})
}

func (f *FakeRunner) RunWithStdin(_ context.Context, stdin string, name string, args ...string) (string, string, error) {
	return f.run(Call{Name: name, Args: args, Stdin: stdin})
}

func (f *FakeRunner) RunWithStdinAttached(_ context.Context, stdin string, name string, args ...string) (string, string, error) {
	return f.run(Call{Name: name, Args: args, Stdin: stdin})
}

func (f *FakeRunner) run(call Call) (string, string, error) {
	f.Calls = append(f.Calls, call)
	if res, ok := f.Results[call.String()]; ok {
		return res.Stdout, res.Stderr, res.Err
	}
	if f.NoDefault {
		return "", "", fmt.Errorf("fake runner: no script for %q", call.String())
	}
	return "", "", nil
}

// CalledWith reports whether any recorded call's rendered command line equals cmdLine.
func (f *FakeRunner) CalledWith(cmdLine string) bool {
	for _, c := range f.Calls {
		if c.String() == cmdLine {
			return true
		}
	}
	return false
}

// StdinFor returns the stdin fed to the most recent recorded call whose rendered command line
// equals cmdLine, and whether any such call was recorded at all. Useful for asserting a secret
// went over stdin (RunWithStdin) rather than appearing in Call.Args.
func (f *FakeRunner) StdinFor(cmdLine string) (string, bool) {
	for i := len(f.Calls) - 1; i >= 0; i-- {
		if f.Calls[i].String() == cmdLine {
			return f.Calls[i].Stdin, true
		}
	}
	return "", false
}
