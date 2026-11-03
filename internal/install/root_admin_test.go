package install

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

const suLine = "/usr/bin/su gm2211 -c /usr/bin/sudo /bin/sh -e"

func adminAccountRunner() *FakeRunner {
	r := NewFakeRunner()
	r.Script("/usr/bin/id -Gn", FakeResult{Stdout: "staff everyone\n"}) // the caller has no sudo
	return r
}

// Pending root steps go through one `su <admin> -c 'sudo sh -e'` with the same script the printed
// block would hold on stdin, and no sudo of this account's own.
func TestRunRootSteps_AdminRunsTheScriptThroughSu(t *testing.T) {
	ok := false
	steps := rootSteps(&ok)
	r := adminAccountRunner()
	// The script "runs": the steps' checks pass afterwards.
	r.Script(suLine, FakeResult{})
	var out strings.Builder
	for i := range steps {
		steps[i].Check = func(context.Context) (bool, error) { return r.CalledWith(suLine), nil }
	}
	outcome, err := RunRootSteps(context.Background(), r, steps, RootOptions{Admin: "gm2211", Interactive: true, In: strings.NewReader(""), Out: &out})
	if err != nil || outcome != RootRan {
		t.Fatalf("= %v, %v\n%s", outcome, err, out.String())
	}
	stdin, called := r.StdinFor(suLine)
	if !called {
		t.Fatalf("no su call: %v", r.Calls)
	}
	for _, want := range []string{"set -e\n", "echo 'grove: disable-sleep'", "/usr/bin/pmset -a disablesleep 1", "bootstrap system"} {
		if !strings.Contains(stdin, want) {
			t.Errorf("script lacks %q:\n%s", want, stdin)
		}
	}
	for _, c := range r.Calls {
		if c.Name == "/usr/bin/sudo" || c.Name == "sudo" || c.Attached {
			t.Errorf("unexpected own-sudo call %q", c.String())
		}
		if c.String() == suLine && !c.Streamed {
			t.Error("su must be streamed so it keeps the terminal for the password")
		}
	}
}

func TestRunRootSteps_AdminWithNothingPendingRunsNothing(t *testing.T) {
	r := adminAccountRunner()
	outcome, err := RunRootSteps(context.Background(), r, nil, RootOptions{Admin: "gm2211", Out: io.Discard})
	if err != nil || outcome != RootNone || len(r.Calls) != 0 {
		t.Errorf("= %v, %v, calls %v", outcome, err, r.Calls)
	}
}

func TestRunRootSteps_AdminRefusals(t *testing.T) {
	ok := false
	t.Run("not an admin", func(t *testing.T) {
		r := adminAccountRunner()
		r.Script("/usr/sbin/dseditgroup -o checkmember -m gm2211 admin", FakeResult{Err: errors.New("no")})
		_, err := RunRootSteps(context.Background(), r, rootSteps(&ok), RootOptions{Admin: "gm2211", Interactive: true, Out: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "not an admin") || r.CalledWith(suLine) {
			t.Errorf("err %v, calls %v", err, r.Calls)
		}
	})
	t.Run("no such account", func(t *testing.T) {
		r := adminAccountRunner()
		r.Script("/usr/bin/dscl . -read /Users/gm2211 UniqueID", FakeResult{Err: errors.New("no")})
		_, err := RunRootSteps(context.Background(), r, rootSteps(&ok), RootOptions{Admin: "gm2211", Interactive: true, Out: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "no such account") || r.CalledWith(suLine) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("no terminal", func(t *testing.T) {
		r := adminAccountRunner()
		_, err := RunRootSteps(context.Background(), r, rootSteps(&ok), RootOptions{Admin: "gm2211", Yes: true, Out: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "ssh -t") || r.CalledWith(suLine) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("no-sudo", func(t *testing.T) {
		r := adminAccountRunner()
		_, err := RunRootSteps(context.Background(), r, rootSteps(&ok), RootOptions{Admin: "gm2211", NoSudo: true, Interactive: true, Out: io.Discard})
		if err == nil || len(r.Calls) != 0 {
			t.Errorf("err %v calls %v", err, r.Calls)
		}
	})
	t.Run("odd names", func(t *testing.T) {
		for _, n := range []string{"root", "-c", "a b", "a;b"} {
			if err := CheckAdminAccount(context.Background(), adminAccountRunner(), n); err == nil {
				t.Errorf("%q accepted", n)
			}
		}
	})
}

// A rootless worker's config.yaml is read as the worker user, through sudo.
func TestWorkerRootless_ReadsAsTheUser(t *testing.T) {
	opts := adminOptionsFor(t, "worker-user")
	path := configFilePath(opts)
	line := "/usr/bin/sudo -n -u worker-user /bin/sh -c " +
		`if [ -e "$1" ]; then exec /bin/cat -- "$1"; fi; echo ` + noConfigMarker + " sh " + path
	for name, tc := range map[string]struct {
		res  FakeResult
		want bool
		err  bool
	}{
		"rootless":   {FakeResult{Stdout: "worker:\n  rootless: true\n"}, true, false},
		"root":       {FakeResult{Stdout: "worker:\n  rootless: false\n"}, false, false},
		"no worker":  {FakeResult{Stdout: "orchard: {}\n"}, false, false},
		"no file":    {FakeResult{Stdout: noConfigMarker + "\n"}, false, false},
		"sudo fails": {FakeResult{Err: errors.New("sudo: a password is required")}, false, true},
		"garbage":    {FakeResult{Stdout: "worker: [1\n"}, false, true},
	} {
		r := NewFakeRunner()
		r.Script(line, tc.res)
		got, err := WorkerRootless(context.Background(), r, opts)
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("%s: = %v, %v", name, got, err)
		}
	}
}

// An unreadable config on the install path is an error, not "not rootless".
func TestApplyRootlessConfig_UnreadableIsAnError(t *testing.T) {
	opts := adminOptionsFor(t, "worker-user")
	opts.Home = t.TempDir()
	if err := writeFile(configFilePath(opts), "worker: [1\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := applyRootlessConfig(opts); err == nil {
		t.Error("a config that cannot be parsed was treated as not rootless")
	}
	opts.NoAutoRootless = true
	if _, err := applyRootlessConfig(opts); err != nil {
		t.Errorf("--rootless=false never reads it: %v", err)
	}
}
