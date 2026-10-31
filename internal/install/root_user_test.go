package install

import (
	"context"
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"
)

// otherUser is an account that exists on every macOS and Linux CI runner and is not this one.
func otherUser(t *testing.T) *user.User {
	t.Helper()
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no user nobody: %v", err)
	}
	return u
}

// adminOptionsFor is what an admin's `grove setup --user` hands PendingRootSteps: the worker user,
// their home, the grove binary, and the URLs the join returned. No secret.
func adminOptionsFor(t *testing.T, name string) Options {
	opts := testWorkerOptions(t.TempDir())
	opts.User = name
	opts.Token = ""
	opts.ServerURL = "http://100.64.0.1:6130"
	opts.GroveBin = "/opt/test/bin/grove"
	return opts
}

// The admin's grove renders the root script for another user from its own state: that user's uid
// for the bootout of an old agent, that user's staged paths, and pins for the plists this process
// renders (with the grove binary both sides name). What the user staged never becomes script text.
func TestPendingRootSteps_RendersForAnotherUser(t *testing.T) {
	nobody := otherUser(t)
	opts := adminOptionsFor(t, nobody.Username)
	ds := workerDaemons(opts)
	for _, d := range ds {
		staged, _ := daemonPaths(opts, d.Label)
		if err := writeFile(staged, "<plist>EVIL_ROOT_COMMAND</plist>\n"); err != nil {
			t.Fatal(err)
		}
	}
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	steps, err := PendingRootSteps(context.Background(), r, opts)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		if !s.Privileged {
			t.Errorf("unprivileged step %s returned", s.Name)
		}
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "disable-sleep,worker-daemons,host-firewall" {
		t.Fatalf("pending root steps = %v", names)
	}
	script, err := stepNamed(t, steps, "worker-daemons").Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	broker, err := brokerDaemonPlist(opts, "/opt/test/bin/grove")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(broker, "<string>"+nobody.Username+"</string>") || !strings.Contains(broker, "<string>/opt/test/bin/grove</string>") {
		t.Fatalf("broker plist is not the worker user's, with the named grove:\n%s", broker)
	}
	for _, want := range []string{
		"/bin/launchctl bootout gui/" + nobody.Uid + "/" + brokerLabel,
		shQuote(plistSHA256(ds[1])),
		shQuote(opts.ConfigDir() + "/launchd/" + brokerLabel + ".plist"),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("root script lacks %q:\n%s", want, script)
		}
	}
	if me := strconv.Itoa(os.Getuid()); me != nobody.Uid && strings.Contains(script, "gui/"+me+"/") {
		t.Errorf("root script boots out this process's uid, not the worker user's:\n%s", script)
	}
	if strings.Contains(script, "EVIL_ROOT_COMMAND") || strings.Contains(script, "worker-secret") {
		t.Errorf("staged content or a secret in the root script:\n%s", script)
	}

	// Once the installed plists are the ones grove renders and loaded, worker-daemons is no longer
	// pending.
	for _, d := range ds {
		_, installed := daemonPaths(opts, d.Label)
		content, _ := d.Render()
		if err := writeFile(installed, content); err != nil {
			t.Fatal(err)
		}
		r.Script("launchctl print system/"+d.Label, FakeResult{Stdout: "program = " + d.Program + "\n"})
	}
	steps, err = PendingRootSteps(context.Background(), r, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Name == "worker-daemons" {
			t.Error("worker-daemons still pending once installed and loaded")
		}
	}
}

// A staged plist can be a symlink to a file only root reads: the root-owned copy is 0600 until it
// passes the hash check, and only then 0644.
func TestRootScripts_CopyIsPrivateUntilChecked(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	script, err := daemonsRootScript(opts, "worker-user", workerDaemons(opts))
	if err != nil {
		t.Fatal(err)
	}
	r := NewFakeRunner()
	r.Script("/usr/bin/defaults read /Library/Preferences/SystemConfiguration/com.apple.vmnet Shared_Net_Address", FakeResult{Stdout: "192.168.70.1\n"})
	for _, s := range hostFirewallSteps(r, opts) {
		if s.Name == "host-firewall" {
			fw, err := s.Root(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			script += fw
		}
	}
	if strings.Contains(script, "-m 0644") {
		t.Errorf("a copy is world-readable before its check:\n%s", script)
	}
	lines := strings.Split(script, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "/bin/chmod 0644 ") {
			continue
		}
		if i == 0 || !strings.Contains(lines[i-1], "/usr/bin/shasum -a 256 < "+strings.TrimPrefix(l, "/bin/chmod 0644 ")) {
			t.Errorf("chmod 0644 not right after the hash check of the same file:\n%s", script)
		}
	}
	if strings.Count(script, "/bin/chmod 0644 ") != 3 {
		t.Errorf("want a chmod after each of the 3 checks:\n%s", script)
	}
}

// The repair path trusts only the root-owned, installed plists: both must be there, for the same
// ordinary user, and the broker's --server is read back as the control plane.
func TestFindInstalledWorker(t *testing.T) {
	opts := adminOptionsFor(t, "worker-user")
	if _, ok := FindInstalledWorker(opts); ok {
		t.Fatal("nothing installed, yet found a worker")
	}
	install := func(o Options) {
		for _, d := range workerDaemons(o) {
			_, installed := daemonPaths(o, d.Label)
			content, err := d.Render()
			if err != nil {
				t.Fatal(err)
			}
			if err := writeFile(installed, content); err != nil {
				t.Fatal(err)
			}
		}
	}
	install(opts)
	got, ok := FindInstalledWorker(opts)
	if !ok || got.User != "worker-user" || got.ServerURL != "http://100.64.0.1:6130" {
		t.Fatalf("= %+v, %v", got, ok)
	}

	// The two daemons disagree about the user.
	other := opts
	other.User = "someone-else"
	d := workerDaemons(other)[1]
	_, installed := daemonPaths(other, d.Label)
	content, _ := d.Render()
	if err := writeFile(installed, content); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindInstalledWorker(opts); ok {
		t.Error("daemons for different users accepted")
	}

	// Only one daemon installed.
	install(opts)
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindInstalledWorker(opts); ok {
		t.Error("a single daemon accepted")
	}

	// A broker installed without --server is still a worker, with no known control plane.
	noServer := opts
	noServer.ServerURL = ""
	install(noServer)
	if got, ok := FindInstalledWorker(opts); !ok || got.ServerURL != "" {
		t.Errorf("no --server: %+v, %v", got, ok)
	}
}
