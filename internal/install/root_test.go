package install

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func init() { rootRecheckDelay = 0 }

// rootSteps are two privileged steps with a known Root text; ok is what their Check reports.
func rootSteps(ok *bool) []Step {
	check := func(context.Context) (bool, error) { return *ok, nil }
	return []Step{
		{Name: "disable-sleep", Privileged: true, Check: check,
			Root: func(context.Context) (string, error) { return "/usr/bin/pmset -a disablesleep 1\n", nil }},
		{Name: "worker-daemons", Privileged: true, Check: check,
			Root: func(context.Context) (string, error) { return "/bin/launchctl bootstrap system '/Library/LaunchDaemons/x.plist'\n", nil }},
	}
}

func adminRunner() *FakeRunner {
	r := NewFakeRunner()
	r.Script("/usr/bin/id -Gn", FakeResult{Stdout: "staff admin everyone\n"})
	return r
}

func sudoCalls(r *FakeRunner) (attached []Call, other []Call) {
	for _, c := range r.Calls {
		switch {
		case c.Attached:
			attached = append(attached, c)
		case c.Name == "/usr/bin/sudo" || c.Name == "sudo":
			other = append(other, c)
		}
	}
	return
}

func TestRunRootSteps_SudoAfterOneYes(t *testing.T) {
	for _, answer := range []string{"y\n", "\n", "yes\n"} {
		ok := false
		r := adminRunner()
		var out strings.Builder
		ranCheck := false
		steps := rootSteps(&ok)
		for i := range steps {
			steps[i].Check = func(context.Context) (bool, error) { ranCheck = true; return true, nil }
		}
		got, err := RunRootSteps(context.Background(), r, steps, RootOptions{Interactive: true, In: strings.NewReader(answer), Out: &out})
		if err != nil || got != RootRan {
			t.Fatalf("answer %q: = %v, %v; want RootRan", answer, got, err)
		}
		attached, _ := sudoCalls(r)
		if len(attached) != 1 {
			t.Fatalf("want exactly one sudo, got %v", r.Calls)
		}
		c := attached[0]
		if c.Name != "/usr/bin/sudo" || len(c.Args) != 4 || c.Args[0] != "/bin/sh" || c.Args[1] != "-e" || c.Args[2] != "-c" {
			t.Fatalf("sudo call = %s", c)
		}
		script := c.Args[3]
		for _, want := range []string{"set -e\n", "echo 'grove: disable-sleep'", "/usr/bin/pmset -a disablesleep 1", "echo 'grove: worker-daemons'"} {
			if !strings.Contains(script, want) {
				t.Errorf("script lacks %q:\n%s", want, script)
			}
		}
		if strings.Contains(script, "sudo") {
			t.Errorf("sudo inside the script:\n%s", script)
		}
		if !ranCheck {
			t.Error("checks were not re-run")
		}
		if !strings.Contains(out.String(), "[Y/n]") {
			t.Errorf("no prompt: %s", out.String())
		}
	}
}

func TestRunRootSteps_DeclinePrintsOneBlock(t *testing.T) {
	ok := false
	r := adminRunner()
	var out strings.Builder
	got, err := RunRootSteps(context.Background(), r, rootSteps(&ok), RootOptions{Interactive: true, In: strings.NewReader("n\n"), Out: &out})
	if err != nil || got != RootPrinted {
		t.Fatalf("= %v, %v; want RootPrinted", got, err)
	}
	if a, o := sudoCalls(r); len(a)+len(o) != 0 {
		t.Errorf("sudo ran: %v %v", a, o)
	}
	if n := strings.Count(out.String(), "<<'GROVE_ROOT_STEPS'"); n != 1 {
		t.Errorf("heredoc block printed %d times:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "re-run the same grove command") {
		t.Errorf("no re-run hint:\n%s", out.String())
	}
}

func TestRunRootSteps_PrintsWhenSudoIsNotPossible(t *testing.T) {
	notAdmin := NewFakeRunner()
	notAdmin.Script("/usr/bin/id -Gn", FakeResult{Stdout: "staff everyone\n"})
	needsPassword := adminRunner()
	needsPassword.Script("/usr/bin/sudo -n /usr/bin/true", FakeResult{Err: errors.New("a password is required")})
	for name, tc := range map[string]struct {
		r    *FakeRunner
		opts RootOptions
	}{
		"not an admin":        {notAdmin, RootOptions{Interactive: true}},
		"no tty, no --yes":    {adminRunner(), RootOptions{}},
		"--yes, no cached pw": {needsPassword, RootOptions{Yes: true}},
		"--no-sudo":           {adminRunner(), RootOptions{Interactive: true, NoSudo: true}},
	} {
		t.Run(name, func(t *testing.T) {
			ok := false
			var out strings.Builder
			tc.opts.Out = &out
			tc.opts.In = errReader{} // a prompt read would fail the test
			got, err := RunRootSteps(context.Background(), tc.r, rootSteps(&ok), tc.opts)
			if err != nil || got != RootPrinted {
				t.Fatalf("= %v, %v; want RootPrinted", got, err)
			}
			if a, _ := sudoCalls(tc.r); len(a) != 0 {
				t.Errorf("sudo ran attached: %v", a)
			}
			if strings.Count(out.String(), "<<'GROVE_ROOT_STEPS'") != 1 || strings.Contains(out.String(), "[Y/n]") {
				t.Errorf("output:\n%s", out.String())
			}
		})
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("the prompt must not be read") }

func TestRunRootSteps_YesWithCachedSudoRunsWithoutPrompt(t *testing.T) {
	ok := true
	r := adminRunner()
	var out strings.Builder
	got, err := RunRootSteps(context.Background(), r, rootSteps(&ok), RootOptions{Yes: true, In: errReader{}, Out: &out})
	if err != nil || got != RootRan {
		t.Fatalf("= %v, %v; want RootRan", got, err)
	}
	if a, _ := sudoCalls(r); len(a) != 1 {
		t.Errorf("want one sudo, got %v", r.Calls)
	}
	if strings.Contains(out.String(), "[Y/n]") {
		t.Error("--yes must not prompt")
	}
}

func TestRunRootSteps_FailsWhenAStepIsStillUnsatisfied(t *testing.T) {
	ok := false
	r := adminRunner()
	_, err := RunRootSteps(context.Background(), r, rootSteps(&ok), RootOptions{Interactive: true, Yes: true, In: errReader{}, Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "disable-sleep") {
		t.Errorf("err = %v, want one naming disable-sleep", err)
	}
}

func TestRunRootSteps_SudoFailureIsAnError(t *testing.T) {
	ok := true
	r := adminRunner()
	r.Script("/usr/bin/sudo /bin/sh -e -c x", FakeResult{})
	steps := rootSteps(&ok)
	steps[0].Root = func(context.Context) (string, error) { return "false\n", nil }
	script := "set -e\necho 'grove: disable-sleep'\nfalse\necho 'grove: worker-daemons'\n/bin/launchctl bootstrap system '/Library/LaunchDaemons/x.plist'\n"
	r.Script("/usr/bin/sudo /bin/sh -e -c "+script, FakeResult{Err: errors.New("exit 1")})
	if _, err := RunRootSteps(context.Background(), r, steps, RootOptions{Interactive: true, Yes: true, Out: io.Discard}); err == nil {
		t.Error("a failing sudo script must be an error")
	}
}

func TestRunRootSteps_NothingToDo(t *testing.T) {
	r := adminRunner()
	got, err := RunRootSteps(context.Background(), r, nil, RootOptions{Out: io.Discard})
	if err != nil || got != RootNone || len(r.Calls) != 0 {
		t.Errorf("= %v, %v, calls %v", got, err, r.Calls)
	}
}

func TestRunPlan_DryRunNeverTouchesSudoOrID(t *testing.T) {
	ok := false
	r := adminRunner()
	var out strings.Builder
	results, err := RunPlan(context.Background(), rootSteps(&ok), PlanOptions{DryRun: true, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Calls) != 0 || !strings.Contains(out.String(), "[plan]") || len(results) != 2 {
		t.Errorf("dry-run: calls %v out %s", r.Calls, out.String())
	}
}

func TestRunPlan_ListsRootStepsWithoutApplyingThem(t *testing.T) {
	ok := false
	steps := rootSteps(&ok)
	applied := false
	for i := range steps {
		steps[i].Apply = func(context.Context) error { applied = true; return nil }
	}
	var out strings.Builder
	results, err := RunPlan(context.Background(), steps, PlanOptions{Out: &out})
	if err != nil || applied {
		t.Fatalf("err %v applied %v", err, applied)
	}
	if !strings.Contains(out.String(), "[root]") || strings.Contains(out.String(), "[manual]") || !results[0].Skipped {
		t.Errorf("out: %s", out.String())
	}
}

func TestShQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/plain/path":    `'/plain/path'`,
		"it's $HOME `x`": `'it'\''s $HOME ` + "`x`" + `'`,
		"":               `''`,
	} {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// The real worker plan's root script carries no secret from Options, and no sudo.
func TestWorkerPlan_RootScriptHasNoSecrets(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	opts.BrokerToken = "grove_broker-secret-3c1d"
	opts.RegistryUser, opts.RegistryToken = "gm2211", "ghp_registry-secret-77"
	r := NewFakeRunner()
	r.Script("/usr/bin/defaults read /Library/Preferences/SystemConfiguration/com.apple.vmnet Shared_Net_Address", FakeResult{Stdout: "192.168.70.1\n"})
	var root []Step
	for _, s := range planWorker(t, r, opts) {
		if s.Privileged {
			root = append(root, s)
		}
	}
	if len(root) < 3 {
		t.Fatalf("expected the pmset, daemons and firewall steps, got %d", len(root))
	}
	var out strings.Builder
	if _, err := RunRootSteps(context.Background(), r, root, RootOptions{Out: &out}); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{opts.Token, opts.BrokerToken, opts.RegistryToken} {
		if secret != "" && strings.Contains(out.String(), secret) {
			t.Errorf("secret in the root script:\n%s", out.String())
		}
	}
}
