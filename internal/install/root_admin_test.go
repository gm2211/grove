package install

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func calledSu(r *FakeRunner) bool { _, ok := suCall(r); return ok }

func suCall(r *FakeRunner) (Call, bool) {
	for _, c := range r.Calls {
		if c.Name == "/usr/bin/su" {
			return c, true
		}
	}
	return Call{}, false
}

func adminAccountRunner() *FakeRunner {
	r := NewFakeRunner()
	r.Script("/usr/bin/id -Gn", FakeResult{Stdout: "staff everyone\n"}) // the caller has no sudo
	return r
}

// Pending root steps go through one attached `su <admin> -c '/usr/bin/sudo /bin/sh -e -c <script>'`
// with the same script the printed block would hold, and no sudo of this account's own. The script
// is in argv, not stdin (su reads the password from stdin).
func TestRunRootSteps_AdminRunsTheScriptThroughSu(t *testing.T) {
	ok := false
	steps := rootSteps(&ok)
	r := adminAccountRunner()
	var out strings.Builder
	for i := range steps {
		steps[i].Check = func(context.Context) (bool, error) { _, called := suCall(r); return called, nil }
	}
	outcome, err := RunRootSteps(context.Background(), r, steps, RootOptions{Admin: "gm2211", Interactive: true, In: strings.NewReader(""), Out: &out})
	if err != nil || outcome != RootRan {
		t.Fatalf("= %v, %v\n%s", outcome, err, out.String())
	}
	c, called := suCall(r)
	if !called {
		t.Fatalf("no su call: %v", r.Calls)
	}
	if !c.Attached || c.Streamed || c.Stdin != "" {
		t.Errorf("su must be attached to the terminal with nothing on stdin: %+v", c)
	}
	if len(c.Args) != 3 || c.Args[0] != "gm2211" || c.Args[1] != "-c" {
		t.Fatalf("su args = %q", c.Args)
	}
	cmd := c.Args[2]
	const prefix = "/usr/bin/sudo /bin/sh -e -c "
	if !strings.HasPrefix(cmd, "/usr/bin/sudo /bin/sh -e -c '") {
		t.Errorf("command = %q", cmd)
	}
	script := unquoteSingle(t, strings.TrimPrefix(cmd, prefix))
	for _, want := range []string{"set -e\n", "echo 'grove: disable-sleep'", "/usr/bin/pmset -a disablesleep 1", "bootstrap system"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	for _, c := range r.Calls {
		if c.Name == "/usr/bin/sudo" || c.Name == "sudo" {
			t.Errorf("unexpected own-sudo call %q", c.String())
		}
	}
}

// unquoteSingle undoes shQuote the way a POSIX shell would for one word.
func unquoteSingle(t *testing.T, word string) string {
	var out strings.Builder
	for i := 0; i < len(word); {
		switch {
		case word[i] == '\'':
			j := strings.IndexByte(word[i+1:], '\'')
			if j < 0 {
				t.Fatalf("unterminated quote in %q", word)
			}
			out.WriteString(word[i+1 : i+1+j])
			i += j + 2
		case word[i] == '\\' && i+1 < len(word):
			out.WriteByte(word[i+1])
			i += 2
		default:
			t.Fatalf("unquoted %q at %d in %q", word[i], i, word)
		}
	}
	return out.String()
}

// A quote-, dollar- and newline-heavy script reaches `sh -e -c` byte for byte through su's shell.
func TestAdminSuCommand_RoundTripsAHostileScript(t *testing.T) {
	script := "set -e\necho 'it'\\''s' \"$HOME\" `id` $(id)\nprintf '%s\\n' \"a b\" ; # c\n/bin/sh -c 'x'\\''y'\n"
	cmd := adminSuCommand(script)
	const prefix = "/usr/bin/sudo /bin/sh -e -c "
	if !strings.HasPrefix(cmd, prefix) {
		t.Fatal(cmd)
	}
	if got := unquoteSingle(t, strings.TrimPrefix(cmd, prefix)); got != script {
		t.Errorf("round trip:\n got %q\nwant %q", got, script)
	}
	if sh, err := exec.LookPath("sh"); err == nil {
		out, err := exec.Command(sh, "-c", "printf %s "+strings.TrimPrefix(cmd, prefix)).Output()
		if err != nil || string(out) != script {
			t.Errorf("sh saw %q, %v", out, err)
		}
	}
}

// The script handed to su holds no credential: Root texts are built from paths and names only.
func TestRunRootSteps_AdminScriptHoldsNoSecret(t *testing.T) {
	opts := adminOptionsFor(t, "worker-user")
	opts.Token = "SECRET-BOOTSTRAP-TOKEN"
	opts.BrokerToken = "SECRET-BROKER-TOKEN"
	opts.RegistryToken = "SECRET-REGISTRY-TOKEN"
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	steps, err := PendingRootSteps(context.Background(), r, opts)
	if err != nil {
		t.Fatal(err)
	}
	var script strings.Builder
	for _, s := range steps {
		body, err := s.Root(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		script.WriteString(body)
	}
	for _, secret := range []string{opts.Token, opts.BrokerToken, opts.RegistryToken} {
		if strings.Contains(script.String(), secret) {
			t.Errorf("root script contains %q", secret)
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
		if err == nil || !strings.Contains(err.Error(), "not an admin") || calledSu(r) {
			t.Errorf("err %v, calls %v", err, r.Calls)
		}
	})
	t.Run("no such account", func(t *testing.T) {
		r := adminAccountRunner()
		r.Script("/usr/bin/dscl . -read /Users/gm2211 UniqueID", FakeResult{Err: errors.New("no")})
		_, err := RunRootSteps(context.Background(), r, rootSteps(&ok), RootOptions{Admin: "gm2211", Interactive: true, Out: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "no such account") || calledSu(r) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("no terminal", func(t *testing.T) {
		r := adminAccountRunner()
		_, err := RunRootSteps(context.Background(), r, rootSteps(&ok), RootOptions{Admin: "gm2211", Yes: true, Out: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "ssh -t") && strings.Contains(err.Error(), "interactive terminal") || calledSu(r) {
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
