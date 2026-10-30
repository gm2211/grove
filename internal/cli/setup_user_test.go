package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/install"
)

func nobodyUser(t *testing.T) *user.User {
	t.Helper()
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no user nobody: %v", err)
	}
	return u
}

// userBase is the admin side's options for the worker user, pointed at scratch directories.
func userBase(t *testing.T, name string) install.Options {
	home := t.TempDir()
	return install.Options{
		User: name, Home: home, GroveBin: "/opt/test/bin/grove",
		GOOS: "darwin", GOARCH: "arm64",
		LookPath:      func(string) (string, error) { return "", errors.New("not found") },
		Exists:        func(string) bool { return false },
		LaunchDaemons: filepath.Join(home, "LaunchDaemons"),
	}
}

var testJoin = &joinResult{
	ControllerURL: "https://cp.tailnet.ts.net:6120", ServerURL: "http://100.64.0.1:6130",
	BootstrapToken: "boot-secret-1111", BrokerToken: "broker-secret-2222",
	ClientToken: "client-secret-3333", DeviceID: "device-1",
}

var testSecrets = []string{"boot-secret-1111", "broker-secret-2222", "client-secret-3333", "registry-secret-4444"}

// The worker user's half gets every secret on stdin, none on argv, through `sudo -n -u <user>`,
// before the root script, which this process renders itself and which carries no secret and none
// of what the user staged.
func TestSetupForUser_UserHalfOnStdinThenRootScriptRenderedHere(t *testing.T) {
	nobody := nobodyUser(t)
	base := userBase(t, nobody.Username)
	staged := filepath.Join(base.Home, ".config", "grove", "launchd", "com.grove.broker.plist")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("EVIL_ROOT_COMMAND\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("printed", func(t *testing.T) {
		r := install.NewFakeRunner()
		var out strings.Builder
		got, err := setupForUser(context.Background(), r, base, testJoin, "octo", "registry-secret-4444",
			install.RootOptions{NoSudo: true, In: strings.NewReader(""), Out: &out})
		if err != nil || got != install.RootPrinted {
			t.Fatalf("= %v, %v", got, err)
		}
		if len(r.Calls) == 0 || !r.Calls[0].Streamed {
			t.Fatalf("first call must be the user's half, streamed: %v", r.Calls)
		}
		half := r.Calls[0]
		wantArgs := "/usr/bin/sudo -n -u " + nobody.Username + " -H -- /opt/test/bin/grove setup --from-admin --grove-bin /opt/test/bin/grove --registry-user octo"
		if half.String() != wantArgs {
			t.Errorf("user's half = %q\nwant %q", half.String(), wantArgs)
		}
		var h adminHandoff
		if err := json.Unmarshal([]byte(half.Stdin), &h); err != nil {
			t.Fatal(err)
		}
		if h.BootstrapToken != "boot-secret-1111" || h.BrokerToken != "broker-secret-2222" || h.RegistryToken != "registry-secret-4444" ||
			h.ClientToken != "client-secret-3333" || h.DeviceID != "device-1" || h.ControllerURL != testJoin.ControllerURL {
			t.Errorf("handoff = %+v", h)
		}
		for _, c := range r.Calls {
			for _, s := range testSecrets {
				if strings.Contains(c.String(), s) {
					t.Errorf("secret on argv: %s", c.String())
				}
			}
		}
		script := out.String()
		for _, want := range []string{
			"grove: worker-daemons", "grove: host-firewall",
			"gui/" + nobody.Uid + "/com.grove.broker",
			"'" + staged + "'",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("root script lacks %q:\n%s", want, script)
			}
		}
		for _, bad := range append([]string{"EVIL_ROOT_COMMAND"}, testSecrets...) {
			if strings.Contains(script, bad) {
				t.Errorf("root script contains %q:\n%s", bad, script)
			}
		}
	})

	t.Run("sudo", func(t *testing.T) {
		r := install.NewFakeRunner()
		r.Script("/usr/bin/id -Gn", install.FakeResult{Stdout: "staff admin\n"})
		var out strings.Builder
		// The script "runs" but installs nothing here, so the re-check fails; what matters is
		// the order and that root ran exactly one script, this process's.
		_, _ = setupForUser(context.Background(), r, base, testJoin, "", "",
			install.RootOptions{Interactive: true, Yes: true, In: strings.NewReader(""), Out: &out})
		var streamed, attached []install.Call
		for _, c := range r.Calls {
			switch {
			case c.Streamed:
				streamed = append(streamed, c)
			case c.Attached:
				attached = append(attached, c)
				if len(streamed) == 0 {
					t.Error("root script before the user's half")
				}
			}
		}
		if len(streamed) != 1 || len(attached) != 1 {
			t.Fatalf("streamed %v, attached %v; want one each", streamed, attached)
		}
		a := attached[0]
		if a.Name != "/usr/bin/sudo" || len(a.Args) != 4 || strings.Join(a.Args[:3], " ") != "/bin/sh -e -c" {
			t.Fatalf("root ran %q", a.String())
		}
		if strings.Contains(a.Args[3], "EVIL_ROOT_COMMAND") || !strings.Contains(a.Args[3], "shasum -a 256") {
			t.Errorf("root script:\n%s", a.Args[3])
		}
	})
}

func TestSetupForUser_RefusesRootAndAFailedUserHalf(t *testing.T) {
	r := install.NewFakeRunner()
	base := userBase(t, "root")
	if _, err := setupForUser(context.Background(), r, base, testJoin, "", "", install.RootOptions{Out: &strings.Builder{}}); err == nil || len(r.Calls) != 0 {
		t.Errorf("root: err %v, calls %v", err, r.Calls)
	}

	nobody := nobodyUser(t)
	base = userBase(t, nobody.Username)
	r = install.NewFakeRunner()
	r.Script("/usr/bin/sudo -n -u "+nobody.Username+" -H -- /opt/test/bin/grove setup --from-admin --grove-bin /opt/test/bin/grove",
		install.FakeResult{Err: errors.New("exit status 1")})
	if _, err := setupForUser(context.Background(), r, base, testJoin, "", "", install.RootOptions{Out: &strings.Builder{}}); err == nil {
		t.Fatal("a failed user half must stop setup")
	}
	for _, c := range r.Calls {
		if c.Attached {
			t.Errorf("root ran after a failed user half: %s", c.String())
		}
	}
}

func TestLookupWorkerTarget_Refusals(t *testing.T) {
	ctx := context.Background()
	admin := install.NewFakeRunner()
	admin.Script("/usr/bin/id -Gn", install.FakeResult{Stdout: "staff admin\n"})
	t.Setenv("GROVE_CONFIG", "")
	if _, err := lookupWorkerTarget(ctx, admin, "root"); err == nil {
		t.Error("root accepted")
	}
	if _, err := lookupWorkerTarget(ctx, admin, "no-such-user-grove-test"); err == nil {
		t.Error("unknown user accepted")
	}
	if me, err := user.Current(); err == nil {
		if _, err := lookupWorkerTarget(ctx, admin, me.Username); err == nil {
			t.Error("this account accepted as --user")
		}
	}
	nobody := nobodyUser(t)
	if os.Geteuid() != 0 {
		notAdmin := install.NewFakeRunner()
		notAdmin.Script("/usr/bin/id -Gn", install.FakeResult{Stdout: "staff everyone\n"})
		if _, err := lookupWorkerTarget(ctx, notAdmin, nobody.Username); err == nil || !strings.Contains(err.Error(), "admin") {
			t.Errorf("non-admin: %v", err)
		}
	}
	tgt, err := lookupWorkerTarget(ctx, admin, nobody.Username)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.User != nobody.Username || tgt.Home != nobody.HomeDir || !filepath.IsAbs(tgt.GroveBin) {
		t.Errorf("target = %+v", tgt)
	}
	t.Setenv("GROVE_CONFIG", "/tmp/elsewhere/config.yaml")
	if _, err := lookupWorkerTarget(ctx, admin, nobody.Username); err == nil {
		t.Error("GROVE_CONFIG accepted")
	}
}

// One password prompt (sudo -v on the terminal), then only `sudo -n -v` refreshes until stop.
func TestStartSudoSession_PromptsOnceAndKeepsAlive(t *testing.T) {
	old := sudoKeepAlive
	sudoKeepAlive = 2 * time.Millisecond
	defer func() { sudoKeepAlive = old }()
	r := install.NewFakeRunner()
	stop, err := startSudoSession(context.Background(), r, true, false)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	stop()
	n := len(r.Calls)
	if n < 2 || !r.Calls[0].Attached || r.Calls[0].String() != "/usr/bin/sudo -v" {
		t.Fatalf("calls = %v", r.Calls)
	}
	for _, c := range r.Calls[1:] {
		if c.Attached || c.String() != "/usr/bin/sudo -n -v" {
			t.Errorf("keep-alive call %q", c.String())
		}
	}
	time.Sleep(10 * time.Millisecond)
	if len(r.Calls) != n {
		t.Error("keep-alive still running after stop")
	}

	if _, err := startSudoSession(context.Background(), install.NewFakeRunner(), false, false); err == nil {
		t.Error("no terminal and no --yes must be refused")
	}
	r = install.NewFakeRunner()
	stop, err = startSudoSession(context.Background(), r, false, true)
	if err != nil || r.Calls[0].String() != "/usr/bin/sudo -n -v" || r.Calls[0].Attached {
		t.Fatalf("--yes: %v %v", err, r.Calls)
	}
	stop()
}

func TestRunSetupFromAdmin_NeedsTheJoinOnStdin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("refuses root before reading stdin")
	}
	for _, in := range []string{"", "{}", `{"controllerUrl":"https://cp"}`} {
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader(in))
		if err := runSetupFromAdmin(cmd); err == nil {
			t.Errorf("stdin %q accepted", in)
		}
	}
}
