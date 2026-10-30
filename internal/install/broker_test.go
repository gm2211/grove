package install

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func TestWorkerPlan_InstallsTheBrokerWithItsCredentialInAFile(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)
	opts.ServerURL = "http://100.64.0.9:6130"
	opts.BrokerToken = "grove_broker-secret-3c1d"
	opts.LookPath = lookPathOnly("tailscale", "grove")
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	for _, c := range r.Calls {
		if strings.Contains(strings.Join(c.Args, " "), opts.BrokerToken) || strings.Contains(c.Stdin, opts.BrokerToken) {
			t.Errorf("broker credential reached a command: %s", c.String())
		}
		if c.Name == "/usr/bin/expect" {
			t.Errorf("the broker credential no longer goes through Keychain: %s", c.String())
		}
	}
	credFile := credentialPath(opts, brokerCredentialName)
	if b, err := os.ReadFile(credFile); err != nil || string(b) != opts.BrokerToken+"\n" {
		t.Fatalf("broker credential file = %q, %v", b, err)
	}
	if info, err := os.Stat(credFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("broker credential file mode = %v, %v; want 0600", info.Mode(), err)
	}
	// grove broker reads the same file, from where the install wrote it.
	if filepath.Base(credFile) != config.BrokerCredentialFile {
		t.Errorf("install writes %s but grove broker reads %s", filepath.Base(credFile), config.BrokerCredentialFile)
	}

	staged, installed := daemonPaths(opts, brokerLabel)
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("reading staged broker plist: %v", err)
	}
	if strings.Contains(string(got), opts.BrokerToken) {
		t.Fatal("broker credential leaked into its LaunchDaemon")
	}
	for _, want := range []string{
		"<string>/usr/bin/grove</string>", "<string>broker</string>", "<string>--server</string>", "<string>" + opts.ServerURL + "</string>",
		"broker.err.log", "<key>UserName</key>\n\t<string>worker-user</string>",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("broker plist lacks %q:\n%s", want, got)
		}
	}
	if installed != filepath.Join(home, "LaunchDaemons", "com.grove.broker.plist") {
		t.Errorf("broker daemon installs to %s", installed)
	}
	for _, l := range []string{"broker.log", "broker.err.log", "orchard-worker.log", "orchard-worker.err.log"} {
		if _, err := os.Stat(filepath.Join(opts.LogDir(), l)); err != nil {
			t.Errorf("log %s should exist before launchd opens it as root: %v", l, err)
		}
	}
}

func TestWorkerPlan_InstallsTheBrokerWithoutACredential(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)
	scriptPinnedDownloads(r, opts)

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	// The broker runs without a credential too (it says it can't sync): nothing is made up.
	if _, err := os.Stat(credentialPath(opts, brokerCredentialName)); !os.IsNotExist(err) {
		t.Errorf("no broker credential file should exist, stat err = %v", err)
	}
	if staged, _ := daemonPaths(opts, brokerLabel); func() bool { _, err := os.Stat(staged); return err != nil }() {
		t.Error("the broker's LaunchDaemon is staged without a credential too")
	}
}

func TestWorkerPlan_MovesTheBrokerCredentialOutOfKeychain(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)
	scriptPinnedDownloads(r, opts)
	r.Script("/usr/bin/security find-generic-password -a broker -s com.gm2211.grove.broker -w", FakeResult{Stdout: "grove_old-keychain-credential\n"})

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if b, err := os.ReadFile(credentialPath(opts, brokerCredentialName)); err != nil || string(b) != "grove_old-keychain-credential\n" {
		t.Fatalf("broker credential file = %q, %v; want the Keychain item's value", b, err)
	}
	// The Keychain item is left where it is: it is a migration source, never deleted.
	for _, c := range r.Calls {
		if strings.Contains(c.String(), "delete-generic-password") && strings.Contains(c.String(), "broker") {
			t.Errorf("the old Keychain item must be left alone: %s", c.String())
		}
	}
}

func TestCheckBroker(t *testing.T) {
	home := t.TempDir()
	opts := Options{Home: home, GOOS: "darwin", User: "worker-user", LaunchDaemons: filepath.Join(home, "LaunchDaemons"), LookPath: lookPathOnly("grove")}
	r := NewFakeRunner()
	if c := checkBroker(context.Background(), r, opts); c.OK || !strings.Contains(c.Detail, "not installed") {
		t.Fatalf("no LaunchDaemon: %+v", c)
	}

	// An earlier grove's LaunchAgent only runs while the user is logged in.
	if err := os.MkdirAll(opts.LaunchAgentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyAgentPlist(opts, brokerLabel), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkBroker(context.Background(), r, opts); c.OK || !strings.Contains(c.Detail, "LaunchAgent") || !strings.Contains(c.Remediation, "grove setup") {
		t.Fatalf("legacy LaunchAgent: %+v", c)
	}

	_, installed := daemonPaths(opts, brokerLabel)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkBroker(context.Background(), r, opts); c.OK || !strings.Contains(c.Detail, "no broker credential file") {
		t.Fatalf("no credential: %+v", c)
	}

	if err := writeSecretFile(credentialPath(opts, brokerCredentialName), "grove_secret\n"); err != nil {
		t.Fatal(err)
	}
	if c := checkBroker(context.Background(), r, opts); c.OK || !strings.Contains(c.Detail, "not loaded") {
		t.Fatalf("not loaded: %+v", c)
	}
	// Readable by others: refused.
	if err := os.Chmod(credentialPath(opts, brokerCredentialName), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkBroker(context.Background(), r, opts); c.OK || !strings.Contains(c.Detail, "readable by other users") {
		t.Fatalf("mode 0644: %+v", c)
	}
	if err := os.Chmod(credentialPath(opts, brokerCredentialName), 0o600); err != nil {
		t.Fatal(err)
	}

	// Loaded, and this test machine has no vmnet bridge, so the broker is waiting for it.
	r.Script("launchctl print system/com.grove.broker", FakeResult{Stdout: "program = /usr/bin/grove\n"})
	r.Script("/usr/bin/defaults read /Library/Preferences/SystemConfiguration/com.apple.vmnet Shared_Net_Address", FakeResult{Stdout: "192.0.2.77\n"})
	if c := checkBroker(context.Background(), r, opts); !c.OK || !strings.Contains(c.Detail, "192.0.2.77:3128") {
		t.Fatalf("waiting for the bridge: %+v", c)
	}
}

func TestBrokerHealth(t *testing.T) {
	for _, tc := range []struct {
		body   string
		ok     bool
		detail string
	}{
		{`{"ok":true,"entries":3,"synced":true}`, true, "synced, 3 entries"},
		{`{"ok":true,"entries":2,"synced":false}`, false, "last sync with the control plane failed"},
		{`not json`, false, "HTTP 200"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, tc.body)
		}))
		c := brokerHealth(context.Background(), srv.URL)
		srv.Close()
		if c.OK != tc.ok || !strings.Contains(c.Detail, tc.detail) {
			t.Errorf("%s: got %+v, want ok=%v and %q", tc.body, c, tc.ok, tc.detail)
		}
	}
}
