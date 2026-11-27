package install

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pfRunner is a FakeRunner whose anchor load fails for rules holding failMarker.
type pfRunner struct {
	*FakeRunner
	failMarker string
	loaded     []string
}

func (p *pfRunner) RunWithStdin(ctx context.Context, stdin, name string, args ...string) (string, string, error) {
	if name == "/sbin/pfctl" && p.failMarker != "" && strings.Contains(stdin, p.failMarker) {
		return "", "syntax error", errors.New("exit 1")
	}
	if name == "/sbin/pfctl" {
		p.loaded = append(p.loaded, stdin)
	}
	return p.FakeRunner.RunWithStdin(ctx, stdin, name, args...)
}

// helperEnv stands in for root: a temp rootHelperHome, root ownership checks that accept this
// process's files, euid 0, and a worker uid (this process's, or 4242 when the test runs as root,
// with the inbox chowned to it).
func helperEnv(t *testing.T) (uid uint32, r *pfRunner) {
	t.Helper()
	home := t.TempDir()
	oldHome, oldOwner, oldEUID, oldPoll, oldWait := rootHelperHome, rootHelperOwnerOK, rootHelperEUID, rootHelperPoll, rootHelperWait
	t.Cleanup(func() {
		rootHelperHome, rootHelperOwnerOK, rootHelperEUID, rootHelperPoll, rootHelperWait = oldHome, oldOwner, oldEUID, oldPoll, oldWait
	})
	rootHelperHome = home
	me := uint32(os.Getuid())
	rootHelperOwnerOK = func(st *syscall.Stat_t) bool { return st.Uid == me }
	rootHelperEUID = func() int { return 0 }
	rootHelperPoll, rootHelperWait = 10*time.Millisecond, 3*time.Second
	uid = me
	if me == 0 {
		uid = 4242
	}
	for _, d := range []string{rootHelperState(), rootHelperInbox(), filepath.Dir(rootHelperBin())} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if me == 0 {
		if err := os.Chown(rootHelperInbox(), int(uid), 0); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := json.Marshal(RootHelperConfig{Version: 1, User: "argos", UID: uid})
	if err := os.WriteFile(rootHelperConfigPath(), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	f := NewFakeRunner()
	f.Script("/sbin/pfctl -s rules", FakeResult{Stdout: `anchor "com.apple/*" all`})
	f.Script("/sbin/pfctl -s info", FakeResult{Stdout: "Status: Enabled for 1 days"})
	f.Script("/sbin/pfctl -q -a com.apple/grove-vm -s rules", FakeResult{Stdout: "pass out quick on bridge100 all"})
	return uid, &pfRunner{FakeRunner: f}
}

func writeRequest(t *testing.T, uid uint32, body string) {
	t.Helper()
	if err := os.WriteFile(rootHelperRequest(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() == 0 {
		if err := os.Chown(rootHelperRequest(), int(uid), 0); err != nil {
			t.Fatal(err)
		}
	}
}

func requestJSON(rules string) string {
	b, _ := json.Marshal(RootHelperRequest{Version: 1, Firewall: &RootHelperFirewallV1{Rules: rules}})
	return string(b)
}

func applyStatus(t *testing.T, r Runner) RootHelperStatus {
	t.Helper()
	if err := RootHelperApply(context.Background(), r, "v0.0.0-test", io.Discard); err != nil {
		t.Fatal(err)
	}
	st, err := ReadRootHelperStatus()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var (
	oldRules = hostFirewallRules(netip.MustParseAddr("192.168.64.1"), nil)
	newRules = hostFirewallRules(netip.MustParseAddr("192.168.64.1"), nomadTargets{netip.MustParseAddr("100.64.0.7")})
)

func TestRootHelperKeepsLastGoodLoaded(t *testing.T) {
	_, r := helperEnv(t)
	if err := os.WriteFile(rootHelperLastGood(), []byte(oldRules), 0o600); err != nil {
		t.Fatal(err)
	}
	st := applyStatus(t, r)
	if !st.Loaded || st.RulesSHA256 != sha256Hex(oldRules) || st.Error != "" {
		t.Fatalf("status %+v", st)
	}
	if len(r.loaded) != 1 || r.loaded[0] != oldRules {
		t.Fatalf("loaded %q", r.loaded)
	}
}

func TestRootHelperLoadsAValidRequest(t *testing.T) {
	uid, r := helperEnv(t)
	_ = os.WriteFile(rootHelperLastGood(), []byte(oldRules), 0o600)
	writeRequest(t, uid, requestJSON(newRules))
	st := applyStatus(t, r)
	if !st.Loaded || st.RulesSHA256 != sha256Hex(newRules) || st.RequestSHA256 != sha256Hex(newRules) || st.Error != "" {
		t.Fatalf("status %+v", st)
	}
	if b, _ := os.ReadFile(rootHelperLastGood()); string(b) != newRules {
		t.Fatal("last good not updated")
	}
	// The next pass loads them again (pf may have lost them) and nothing else.
	r.loaded = nil
	if st := applyStatus(t, r); st.RulesSHA256 != sha256Hex(newRules) || len(r.loaded) != 1 {
		t.Fatalf("second pass: %+v, loaded %d", st, len(r.loaded))
	}
}

func TestRootHelperRefusesRulesOutsideTheGrammar(t *testing.T) {
	uid, r := helperEnv(t)
	_ = os.WriteFile(rootHelperLastGood(), []byte(oldRules), 0o600)
	evil := newRules + "rdr pass on en0 inet proto tcp to any port 22 -> 127.0.0.1 port 22\n"
	writeRequest(t, uid, requestJSON(evil))
	st := applyStatus(t, r)
	if !strings.Contains(st.Error, "rules refused") || st.RequestSHA256 != sha256Hex(evil) {
		t.Fatalf("status %+v", st)
	}
	if !st.Loaded || st.RulesSHA256 != sha256Hex(oldRules) {
		t.Fatalf("the last good rules are not what is loaded: %+v", st)
	}
	for _, l := range r.loaded {
		if strings.Contains(l, "rdr") {
			t.Fatal("pfctl saw the refused rules")
		}
	}
	if strings.Contains(mustRead(t, rootHelperStatusPath()), "127.0.0.1 port 22") {
		t.Fatal("status echoes the request")
	}
}

func TestRootHelperGoesBackWhenRulesDontLoad(t *testing.T) {
	uid, r := helperEnv(t)
	_ = os.WriteFile(rootHelperLastGood(), []byte(oldRules), 0o600)
	r.failMarker = "100.64.0.7"
	writeRequest(t, uid, requestJSON(newRules))
	st := applyStatus(t, r)
	if !strings.Contains(st.Error, "did not load") || !st.Loaded || st.RulesSHA256 != sha256Hex(oldRules) {
		t.Fatalf("status %+v", st)
	}
	if b, _ := os.ReadFile(rootHelperLastGood()); string(b) != oldRules {
		t.Fatal("last good replaced by rules that didn't load")
	}
}

func TestRootHelperRefusesBadRequests(t *testing.T) {
	cases := map[string]func(t *testing.T, uid uint32){
		"unknown field": func(t *testing.T, uid uint32) {
			writeRequest(t, uid, `{"version":1,"firewall":{"rules":"x"},"daemons":{}}`)
		},
		"newer version": func(t *testing.T, uid uint32) {
			writeRequest(t, uid, `{"version":2,"firewall":{"rules":"x"}}`)
		},
		"trailing data": func(t *testing.T, uid uint32) {
			writeRequest(t, uid, requestJSON(newRules)+`{}`)
		},
		"symlink": func(t *testing.T, uid uint32) {
			target := filepath.Join(t.TempDir(), "r.json")
			_ = os.WriteFile(target, []byte(requestJSON(newRules)), 0o600)
			if err := os.Symlink(target, rootHelperRequest()); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, uid uint32) {
			if err := syscall.Mkfifo(rootHelperRequest(), 0o600); err != nil {
				t.Skip(err)
			}
		},
		"too big": func(t *testing.T, uid uint32) {
			writeRequest(t, uid, `{"version":1,"firewall":{"rules":"`+strings.Repeat("#", rootHelperMaxRequest)+`"}}`)
		},
	}
	if os.Getuid() == 0 {
		cases["another user's file"] = func(t *testing.T, uid uint32) {
			writeRequest(t, uid, requestJSON(newRules))
			_ = os.Chown(rootHelperRequest(), 0, 0)
		}
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			uid, r := helperEnv(t)
			_ = os.WriteFile(rootHelperLastGood(), []byte(oldRules), 0o600)
			setup(t, uid)
			st := applyStatus(t, r)
			if st.Error == "" || st.RulesSHA256 != sha256Hex(oldRules) || !st.Loaded {
				t.Fatalf("status %+v", st)
			}
		})
	}
}

func TestRootHelperRefusesAForeignConfig(t *testing.T) {
	_, r := helperEnv(t)
	rootHelperOwnerOK = func(*syscall.Stat_t) bool { return false }
	if err := RootHelperApply(context.Background(), r, "v", io.Discard); err == nil {
		t.Fatal("ran with a config root doesn't own")
	}
}

// The user's side: with a helper installed for this user, the firewall is an ordinary step that
// sends the rules and waits for the helper's status, and its check reads that status.
func TestHelperFirewallStep(t *testing.T) {
	uid, r := helperEnv(t)
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(RootHelperConfig{Version: 1, User: u.Username, UID: uid})
	_ = os.WriteFile(rootHelperConfigPath(), cfg, 0o644)
	if u.Username == "root" {
		t.Skip("workerUser refuses root")
	}
	opts := Options{Role: RoleWorker, User: u.Username, Home: t.TempDir(), ServerURL: "https://100.64.0.7:8443"}
	if _, ok := RootHelperFor(opts); !ok {
		t.Fatal("helper not found for this user")
	}
	steps := hostFirewallSteps(r, opts)
	if len(steps) != 1 || steps[0].Privileged {
		t.Fatalf("want one unprivileged step, got %+v", steps)
	}
	ctx := context.Background()
	if ok, _ := steps[0].Check(ctx); ok {
		t.Fatal("check passed before anything loaded")
	}
	done := make(chan error, 1)
	go func() { done <- steps[0].Apply(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(rootHelperRequest()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no request written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := RootHelperApply(ctx, r, "v", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ok, err := steps[0].Check(ctx); !ok || err != nil {
		t.Fatalf("check after apply: %v %v", ok, err)
	}
	if c := checkHostFirewall(ctx, r, opts); !c.OK {
		t.Fatalf("doctor: %+v", c)
	}
}

func TestRootHelperPlist(t *testing.T) {
	p := renderRootHelperPlist()
	for _, want := range []string{"<string>" + RootHelperLabel + "</string>", "root-helper</string>", "apply</string>", "WatchPaths", "Application Support/grove/root-helper/inbox", "StartInterval"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if strings.Contains(p, "UserName") {
		t.Error("the helper runs as root: no UserName")
	}
}

func TestWritableBy(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "grove")
	_ = os.WriteFile(f, []byte("x"), 0o755)
	if who := writableBy(f, u); who == "" {
		t.Fatal("a file the user owns counts as not writable by them")
	}
	other := &user.User{Uid: strconv.Itoa(os.Getuid() + 777), Username: "other"}
	_ = os.Chmod(filepath.Dir(f), 0o755)
	if who := writableBy(f, other); who != "" && !strings.Contains(who, "tmp") {
		t.Fatalf("other user: %s", who)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
