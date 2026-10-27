package install

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// plistStrings returns every <string> in a plist, unescaped, which is what launchd hands the job.
func plistStrings(t *testing.T, plist string) []string {
	t.Helper()
	var out []string
	d := xml.NewDecoder(strings.NewReader(plist))
	d.Strict = false // the DOCTYPE
	inString := false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, plist)
		}
		switch v := tok.(type) {
		case xml.StartElement:
			inString = v.Name.Local == "string"
			if inString {
				out = append(out, "")
			}
		case xml.EndElement:
			inString = false
		case xml.CharData:
			if inString {
				out[len(out)-1] += string(v)
			}
		}
	}
}

// The firewall's LaunchDaemon runs only root's /bin/sh and pfctl, with the whole loader inline,
// and the loader lets VMs reach exactly DHCP, DNS and the broker on the vmnet gateway, and Nomad
// RPC, before it closes this Mac and every private network.
func TestHostFirewallPlist_RunsOnlyRootsToolsWithTheRulesInline(t *testing.T) {
	gw := netip.MustParseAddr("192.168.70.1")
	plist := hostFirewallPlist(gw)
	args := plistStrings(t, plist)
	if len(args) != 4 || args[0] != hostFirewallLabel || args[1] != "/bin/sh" || args[2] != "-c" {
		t.Fatalf("label and program = %q", args)
	}
	script := args[3]
	if script != hostFirewallScript(gw) {
		t.Fatalf("the plist's script doesn't round-trip:\n%s", script)
	}
	for _, line := range strings.Split(script, "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.Contains(f[0], "/") && !strings.HasPrefix(f[0], "/sbin/") && !strings.HasPrefix(f[0], "/usr/bin/") {
			t.Errorf("runs %s, which root doesn't own: %s", f[0], line)
		}
	}
	for _, want := range []string{
		`grove_gateway = "192.168.70.1"`,
		"pass in quick on $grove_vm_if inet proto udp from any port 68 to any port 67 keep state",
		"pass in quick on $grove_vm_if inet proto { udp tcp } from any to $grove_gateway port 53 keep state",
		"pass in quick on $grove_vm_if inet proto tcp from any to $grove_gateway port 3128 keep state",
		"pass in quick on $grove_vm_if inet proto tcp from any to any port 4647 keep state",
		"block return in quick on $grove_vm_if inet from any to { <grove_private> self }",
		"block return in quick on $grove_vm_if inet6 all",
		"/sbin/pfctl -q -a com.apple/grove-vm -f -",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("loader lacks %q", want)
		}
	}
	// Every pass that lets a VM in comes before the block that closes the Mac.
	if strings.LastIndex(script, "pass in quick on $grove_vm_if inet ") > strings.Index(script, "block return in quick on $grove_vm_if inet ") {
		t.Error("an IPv4 pass comes after the block: pf's quick would never reach it")
	}
	for _, private := range []string{"10.0.0.0/8", "100.64.0.0/10", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8"} {
		if !strings.Contains(script, " "+private+" ") {
			t.Errorf("<grove_private> lacks %s", private)
		}
	}
	if strings.Contains(script, "bridge0 ") || !strings.Contains(script, "bridge100 ") {
		t.Error("the VM interfaces must be vmnet's bridges, never Thunderbolt Bridge")
	}
	if strings.Contains(plist, "<grove_private>") {
		t.Error("the plist carries an unescaped <")
	}
}

func TestWorkerPlan_StagesTheHostFirewallAndLeavesInstallingToAnAdmin(t *testing.T) {
	home := t.TempDir()
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	opts := testWorkerOptions(home)
	scriptPinnedDownloads(r, opts)
	r.Script("/usr/bin/defaults read /Library/Preferences/SystemConfiguration/com.apple.vmnet Shared_Net_Address", FakeResult{Stdout: "192.168.70.1\n"})

	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	results, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	staged, installed := hostFirewallPaths(opts)
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("staged plist: %v", err)
	}
	if string(got) != hostFirewallPlist(netip.MustParseAddr("192.168.70.1")) {
		t.Errorf("staged plist isn't the one for this Mac's vmnet gateway:\n%s", got)
	}
	var manual bool
	for _, res := range results {
		if res.Step == "host-firewall" {
			manual = res.Skipped && !res.Applied
			if !strings.Contains(res.Desc, hostFirewallInstallCommand(staged, installed)) {
				t.Errorf("manual step doesn't print the install command: %s", res.Desc)
			}
		}
	}
	if !manual {
		t.Error("host-firewall should be left to an admin when not root")
	}
	for _, c := range r.Calls {
		if c.Name == "sudo" && strings.Contains(c.String(), hostFirewallLabel) {
			t.Errorf("ran a privileged command without root: %s", c.String())
		}
	}

	// As root with --yes it installs what it staged, and (re)loads it.
	r2 := NewFakeRunner()
	steps = hostFirewallSteps(r2, opts)
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard, Yes: true, IsRoot: true}); err != nil {
		t.Fatalf("RunPlan as root: %v", err)
	}
	for _, want := range []string{
		"sudo install -o root -g wheel -m 0644 " + staged + " " + installed,
		"sudo launchctl bootstrap system " + installed,
	} {
		if !r2.CalledWith(want) {
			t.Errorf("expected %q; calls:\n%v", want, r2.Calls)
		}
	}
}

func TestCheckHostFirewall(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	_, installed := hostFirewallPaths(opts)
	want := hostFirewallPlist(netip.MustParseAddr("192.168.64.1"))
	loaded := "system/com.grove.pf-vm = {\n\tlast exit code = 0\n}\n"

	for name, tc := range map[string]struct {
		installed string // "" for none
		print     FakeResult
		ok        bool
		detail    string
	}{
		"not installed":      {"", FakeResult{Stdout: loaded}, false, "not installed"},
		"another plist":      {strings.Replace(want, "4647", "22", 1), FakeResult{Stdout: loaded}, false, "is not the one"},
		"not loaded":         {want, FakeResult{Err: errors.New("exit 113")}, false, "is not loaded"},
		"loader failed":      {want, FakeResult{Stdout: "\tlast exit code = 1\n"}, false, "failed to load the anchor"},
		"installed and live": {want, FakeResult{Stdout: loaded}, true, "anchor com.apple/grove-vm loaded"},
	} {
		t.Run(name, func(t *testing.T) {
			os.RemoveAll(opts.LaunchDaemons)
			if tc.installed != "" {
				if err := os.MkdirAll(opts.LaunchDaemons, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(installed, []byte(tc.installed), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r := NewFakeRunner()
			r.Script("launchctl print system/com.grove.pf-vm", tc.print)
			got := checkHostFirewall(context.Background(), r, opts)
			if got.OK != tc.ok || !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("= %+v, want OK=%v and %q", got, tc.ok, tc.detail)
			}
			if !got.OK && !strings.Contains(got.Remediation, "sudo install -o root") {
				t.Errorf("remediation doesn't say what to run: %q", got.Remediation)
			}
		})
	}
}

// TestHostFirewallLoader_RealPF runs the LaunchDaemon's loader against the real pf of a macOS
// CI runner (GROVE_PF_TEST=1, passwordless sudo): pfctl must accept every rule, the anchor must
// end up live with pf on, and a second run, as every five minutes, must leave it the same. It
// flushes the anchor and puts pf back as it found it afterwards.
func TestHostFirewallLoader_RealPF(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("GROVE_PF_TEST") != "1" {
		t.Skip("needs macOS and GROVE_PF_TEST=1 (it changes this Mac's pf)")
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		t.Fatal("GROVE_PF_TEST=1 needs passwordless sudo")
	}
	info, _ := exec.Command("sudo", "-n", "/sbin/pfctl", "-s", "info").CombinedOutput()
	wasEnabled := strings.Contains(string(info), "Status: Enabled")
	t.Cleanup(func() {
		_ = exec.Command("sudo", "-n", "/sbin/pfctl", "-q", "-a", hostFirewallAnchor, "-F", "all").Run()
		if !wasEnabled {
			_ = exec.Command("sudo", "-n", "/sbin/pfctl", "-q", "-d").Run()
		}
	})

	script := filepath.Join(t.TempDir(), "loader.sh")
	if err := os.WriteFile(script, []byte(hostFirewallScript(netip.MustParseAddr("192.168.64.1"))), 0o644); err != nil {
		t.Fatal(err)
	}
	var rules string
	for run := 1; run <= 2; run++ {
		if out, err := exec.Command("sudo", "-n", "/bin/sh", script).CombinedOutput(); err != nil {
			t.Fatalf("run %d: loader failed: %v\n%s", run, err, out)
		}
		out, err := exec.Command("sudo", "-n", "/sbin/pfctl", "-a", hostFirewallAnchor, "-s", "rules").CombinedOutput()
		if err != nil {
			t.Fatalf("reading the anchor: %v\n%s", err, out)
		}
		if run == 2 && string(out) != rules {
			t.Errorf("a reload changed the anchor:\n%s\nwas:\n%s", out, rules)
		}
		rules = string(out)
	}
	t.Logf("anchor %s:\n%s", hostFirewallAnchor, rules)
	for _, want := range []string{"port = 3128", "port = 4647", "port = 53", "port = 67", "block return in quick on bridge100 inet from any to <grove_private>", "block return in quick on bridge109 inet6 all"} {
		if !strings.Contains(rules, want) {
			t.Errorf("live anchor lacks %q", want)
		}
	}
	if info, _ := exec.Command("sudo", "-n", "/sbin/pfctl", "-s", "info").CombinedOutput(); !strings.Contains(string(info), "Status: Enabled") {
		t.Error("pf is not enabled after the loader ran")
	}
}
