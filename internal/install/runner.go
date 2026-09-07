// Package install implements `grove install`'s role bootstrap planner and the checks behind
// `grove doctor`. Every command that could touch the host (brew, launchctl, sudo, tailscale, …)
// goes through the Runner interface so the planner can be exercised in tests (FakeRunner) and so
// --dry-run never executes anything.
package install

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes external commands. The real implementation shells out; tests use FakeRunner.
type Runner interface {
	// Run executes name with args and returns captured stdout/stderr. err is non-nil on a
	// non-zero exit or if the command could not be started.
	Run(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)
}

// ExecRunner is the real Runner, shelling out via os/exec.
type ExecRunner struct{}

var _ Runner = ExecRunner{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), err
}

// Call records one Run invocation, for assertions in tests.
type Call struct {
	Name string
	Args []string
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

func (f *FakeRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	call := Call{Name: name, Args: args}
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
