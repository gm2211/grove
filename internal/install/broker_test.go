package install

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func TestWorkerPlan_InstallsTheBrokerWithItsCredentialInKeychain(t *testing.T) {
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

	var stored bool
	for _, c := range r.Calls {
		if c.Name == "/usr/bin/expect" && c.Stdin == config.BrokerTokenKeychainAccount+"\n"+config.BrokerTokenKeychainService+"\n"+opts.BrokerToken+"\n" {
			stored = true
		}
		if strings.Contains(strings.Join(c.Args, " "), opts.BrokerToken) {
			t.Errorf("broker credential appeared in a command's arguments: %s", c.String())
		}
	}
	if !stored {
		t.Error("expected the broker credential to be stored in Keychain over stdin")
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.grove.broker.plist")
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading broker plist: %v", err)
	}
	if strings.Contains(string(got), opts.BrokerToken) {
		t.Fatal("broker credential leaked into its LaunchAgent")
	}
	for _, want := range []string{"<string>/usr/bin/grove</string>", "<string>broker</string>", "<string>--server</string>", "<string>" + opts.ServerURL + "</string>", "broker.err.log"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("broker plist lacks %q:\n%s", want, got)
		}
	}
	if !r.CalledWith("launchctl bootstrap gui/" + strconv.Itoa(os.Getuid()) + " " + plistPath) {
		t.Error("expected install to load the broker LaunchAgent")
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
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	plan := strings.Join(names, " ")
	if strings.Contains(plan, "broker-keychain") {
		t.Error("with no broker credential there is nothing to store")
	}
	if !strings.Contains(plan, "launchagent:broker") || !strings.Contains(plan, "launchagent-load:broker") {
		t.Errorf("the broker runs without a credential too (it says it can't sync); plan: %s", plan)
	}
}

func TestCheckBroker(t *testing.T) {
	home := t.TempDir()
	opts := Options{Home: home, GOOS: "darwin"}
	r := NewFakeRunner()
	if c := checkBroker(context.Background(), r, opts); c.OK || !strings.Contains(c.Detail, "not installed") {
		t.Fatalf("no LaunchAgent: %+v", c)
	}
	if err := os.MkdirAll(opts.LaunchAgentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opts.LaunchAgentsDir(), "com.grove.broker.plist"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Script("/usr/bin/security find-generic-password -a broker -s com.gm2211.grove.broker", FakeResult{Err: os.ErrNotExist})
	if c := checkBroker(context.Background(), r, opts); c.OK || !strings.Contains(c.Detail, "no broker credential") {
		t.Fatalf("no credential: %+v", c)
	}
	delete(r.Results, "/usr/bin/security find-generic-password -a broker -s com.gm2211.grove.broker")
	// Loaded, and this test machine has no vmnet bridge, so the broker is waiting for it.
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
