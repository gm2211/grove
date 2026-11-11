package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// testControlPlaneIP is what testWorkerOptions' controller (cp.tailnet.ts.net) resolves to in
// these tests, and testNomad the firewall's Nomad RPC targets that follow from it.
const testControlPlaneIP = "100.64.0.10"

var testNomad = nomadTargets{netip.MustParseAddr(testControlPlaneIP)}

// dscacheutilHost is the Runner call that resolves host.
func dscacheutilHost(host string) string {
	return "/usr/bin/dscacheutil -q host -a name " + host
}

// scriptControlPlaneDNS makes the control plane's names these tests use (cp.tailnet.ts.net, and
// the short "cp") resolve, printed as the Mac's resolver prints them.
func scriptControlPlaneDNS(r *FakeRunner) {
	for _, host := range []string{"cp.tailnet.ts.net", "cp"} {
		r.Script(dscacheutilHost(host), FakeResult{Stdout: "name: " + host + "\nipv6_address: fd7a:115c:a1e0::1\n\nname: " + host + "\nip_address: " + testControlPlaneIP + "\n\n"})
	}
}

// The firewall's LaunchDaemon runs only root's /bin/sh and pfctl, with the whole loader inline,
// and the loader lets VMs reach exactly DHCP, DNS and the broker on the vmnet gateway, and Nomad
// RPC, before it closes this Mac and every private network.
func TestHostFirewallPlist_RunsOnlyRootsToolsWithTheRulesInline(t *testing.T) {
	gw := netip.MustParseAddr("192.168.70.1")
	plist := hostFirewallPlist(gw, testNomad)
	args := plistStrings(t, plist)
	if len(args) != 4 || args[0] != hostFirewallLabel || args[1] != "/bin/sh" || args[2] != "-c" {
		t.Fatalf("label and program = %q", args)
	}
	script := args[3]
	if script != hostFirewallScript(gw, testNomad) {
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
		`grove_nomad = "{ 100.64.0.10 }"`,
		"pass in quick on $grove_vm_if inet proto tcp from any to $grove_nomad port 4647 keep state",
		"block return in quick on $grove_vm_if inet from any to { <grove_private> self }",
		"block return in quick on $grove_vm_if inet6 all",
		"/sbin/pfctl -q -a com.apple/grove-vm -f -",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("loader lacks %q", want)
		}
	}
	// Nomad RPC is open towards the control plane only: no pass names port 4647 on any address.
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "pass in ") && strings.Contains(line, " to any ") && !strings.HasSuffix(line, "from any port 68 to any port 67 keep state") {
			t.Errorf("a pass lets a VM reach a port on any address: %s", line)
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
	scriptControlPlaneDNS(r)

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
	if string(got) != hostFirewallPlist(netip.MustParseAddr("192.168.70.1"), testNomad) {
		t.Errorf("staged plist isn't the one for this Mac's vmnet gateway and control plane:\n%s", got)
	}
	var manual bool
	for _, res := range results {
		if res.Step == "host-firewall" {
			manual = res.Skipped && !res.Applied
			if !strings.Contains(res.Desc, "VM firewall") {
				t.Errorf("root step doesn't describe itself: %s", res.Desc)
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
	scriptControlPlaneDNS(r2)
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
	want := hostFirewallPlist(netip.MustParseAddr("192.168.64.1"), testNomad)
	loaded := "system/com.grove.pf-vm = {\n\tlast exit code = 0\n}\n"

	for name, tc := range map[string]struct {
		installed string // "" for none
		print     FakeResult
		ok        bool
		detail    string
	}{
		"not installed": {"", FakeResult{Stdout: loaded}, false, "not installed"},
		"another plist": {strings.Replace(want, "4647", "22", 1), FakeResult{Stdout: loaded}, false, "is not the one"},
		// The control plane moved (or this plist is from before the pass named an address).
		"another control plane": {hostFirewallPlist(netip.MustParseAddr("192.168.64.1"), nomadTargets{netip.MustParseAddr("100.64.0.99")}), FakeResult{Stdout: loaded}, false, "is not the one"},
		"not loaded":            {want, FakeResult{Err: errors.New("exit 113")}, false, "is not loaded"},
		"loader failed":         {want, FakeResult{Stdout: "\tlast exit code = 1\n"}, false, "failed to load the anchor"},
		"installed and live":    {want, FakeResult{Stdout: loaded}, true, "anchor com.apple/grove-vm loaded"},
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
			scriptControlPlaneDNS(r)
			r.Script("launchctl print system/com.grove.pf-vm", tc.print)
			got := checkHostFirewall(context.Background(), r, opts)
			if got.OK != tc.ok || !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("= %+v, want OK=%v and %q", got, tc.ok, tc.detail)
			}
			if !got.OK && !strings.Contains(got.Remediation, "grove setup") {
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
	if err := os.WriteFile(script, []byte(hostFirewallScript(netip.MustParseAddr("192.168.64.1"), nomadTargets{netip.MustParseAddr("100.64.0.10"), netip.MustParseAddr("100.64.0.11")})), 0o644); err != nil {
		t.Fatal(err)
	}
	// The control plane on this same Mac: pf must take `self` as the Nomad RPC destination too.
	selfScript := filepath.Join(t.TempDir(), "loader-self.sh")
	if err := os.WriteFile(selfScript, []byte(hostFirewallScript(netip.MustParseAddr("192.168.64.1"), nil)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sudo", "-n", "/bin/sh", selfScript).CombinedOutput(); err != nil {
		t.Fatalf("loader with the control plane on this Mac failed: %v\n%s", err, out)
	}
	if out, err := exec.Command("sudo", "-n", "/sbin/pfctl", "-a", hostFirewallAnchor, "-s", "rules").CombinedOutput(); err != nil {
		t.Fatalf("reading the anchor: %v\n%s", err, out)
	} else {
		t.Logf("anchor %s with the control plane on this Mac:\n%s", hostFirewallAnchor, out)
		if !strings.Contains(string(out), "port = 4647") || strings.Contains(string(out), "to any port = 4647") {
			t.Errorf("Nomad RPC is not passed to this Mac only:\n%s", out)
		}
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
	for _, want := range []string{"port = 3128", "to 100.64.0.10 port = 4647", "to 100.64.0.11 port = 4647", "port = 53", "port = 67", "block return in quick on bridge100 inet from any to <grove_private>", "block return in quick on bridge109 inet6 all"} {
		if !strings.Contains(rules, want) {
			t.Errorf("live anchor lacks %q", want)
		}
	}
	if strings.Contains(rules, "to any port = 4647") {
		t.Errorf("the live anchor passes Nomad RPC to any address:\n%s", rules)
	}
	if info, _ := exec.Command("sudo", "-n", "/sbin/pfctl", "-s", "info").CombinedOutput(); !strings.Contains(string(info), "Status: Enabled") {
		t.Error("pf is not enabled after the loader ran")
	}
}

func TestHostFirewallRootScript_PinsTheInstalledCopy(t *testing.T) {
	r := NewFakeRunner()
	r.Script("/usr/bin/defaults read /Library/Preferences/SystemConfiguration/com.apple.vmnet Shared_Net_Address", FakeResult{Stdout: "192.168.70.1\n"})
	scriptControlPlaneDNS(r)
	opts := testWorkerOptions(t.TempDir())
	staged, installed := hostFirewallPaths(opts)
	var step Step
	for _, s := range hostFirewallSteps(r, opts) {
		if s.Name == "host-firewall" {
			step = s
		}
	}
	script, err := step.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(hostFirewallPlist(netip.MustParseAddr("192.168.70.1"), testNomad)))
	for _, want := range []string{
		"/usr/bin/install -o root -g wheel -m 0600 " + shQuote(staged) + " " + shQuote(installed),
		"/bin/chmod 0644 " + shQuote(installed),
		"/usr/bin/shasum -a 256 < " + shQuote(installed),
		shQuote(hex.EncodeToString(sum[:])),
		"/bin/rm -f " + shQuote(installed) + "; echo",
		"/bin/launchctl bootout system/" + hostFirewallLabel,
		"/bin/launchctl bootstrap system " + shQuote(installed),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "sudo") {
		t.Errorf("sudo inside the root script:\n%s", script)
	}
}

// The Nomad RPC pass names the control plane's address: nomad.url in this user's config.yaml, else
// the grove server, else the Orchard controller; a name is resolved at plan time through Runner,
// this Mac itself is pf's `self`, and a name that does not resolve fails rather than open the port
// to every address.
func TestResolveNomadTargets(t *testing.T) {
	t.Setenv("GROVE_CONFIG", "")
	dns := func(lines ...string) FakeResult { return FakeResult{Stdout: strings.Join(lines, "\n") + "\n"} }
	for name, tc := range map[string]struct {
		opts    func(o *Options)
		config  string
		script  map[string]FakeResult
		want    string // nomadTargets.pf()
		wantErr string
		lookup  string // the one dscacheutil call expected, "" for none
	}{
		"controller name": {
			script: map[string]FakeResult{dscacheutilHost("cp.tailnet.ts.net"): dns("name: cp.tailnet.ts.net", "ip_address: 100.64.0.10")},
			want:   "100.64.0.10", lookup: dscacheutilHost("cp.tailnet.ts.net"),
		},
		"server before controller": {
			opts:   func(o *Options) { o.ServerURL = "https://grove.tailnet.ts.net:6130" },
			script: map[string]FakeResult{dscacheutilHost("grove.tailnet.ts.net"): dns("name: grove.tailnet.ts.net", "ip_address: 100.64.0.20")},
			want:   "100.64.0.20", lookup: dscacheutilHost("grove.tailnet.ts.net"),
		},
		"nomad.url in config.yaml first": {
			opts:   func(o *Options) { o.ServerURL = "https://grove.tailnet.ts.net:6130" },
			config: "nomad:\n  url: https://100.64.0.30:4646\n",
			want:   "100.64.0.30",
		},
		"an address needs no lookup": {
			opts: func(o *Options) { o.Controller = "https://100.101.229.60:6120" },
			want: "100.101.229.60",
		},
		"several addresses, sorted, IPv4 only": {
			script: map[string]FakeResult{dscacheutilHost("cp.tailnet.ts.net"): dns("name: cp.tailnet.ts.net", "ipv6_address: fd7a:115c:a1e0::1", "", "name: cp.tailnet.ts.net", "ip_address: 100.64.0.11", "ip_address: 100.64.0.10", "ip_address: 100.64.0.11")},
			want:   "100.64.0.10 100.64.0.11", lookup: dscacheutilHost("cp.tailnet.ts.net"),
		},
		"this Mac by loopback address": {
			opts: func(o *Options) { o.Controller = "http://127.0.0.1:6120" },
			want: "self",
		},
		"this Mac by name": {
			opts: func(o *Options) { o.Controller = "http://localhost:6120" },
			want: "self",
		},
		"a name that resolves to loopback": {
			script: map[string]FakeResult{dscacheutilHost("cp.tailnet.ts.net"): dns("name: cp.tailnet.ts.net", "ip_address: 127.0.0.1")},
			want:   "self", lookup: dscacheutilHost("cp.tailnet.ts.net"),
		},
		"a name that does not resolve": {
			wantErr: "does not resolve to an IPv4 address", lookup: dscacheutilHost("cp.tailnet.ts.net"),
		},
		"the resolver fails": {
			script:  map[string]FakeResult{dscacheutilHost("cp.tailnet.ts.net"): {Err: errors.New("exit 1")}},
			wantErr: "resolve the control plane's host cp.tailnet.ts.net", lookup: dscacheutilHost("cp.tailnet.ts.net"),
		},
		"IPv6 only": {
			opts:    func(o *Options) { o.Controller = "https://[fd7a:115c:a1e0::1]:6120" },
			wantErr: "not IPv4",
		},
		"not a host name": {
			opts:    func(o *Options) { o.Controller = "https://cp_$(id).example:6120" },
			wantErr: "not a host name",
		},
		"the admin's placeholder names no control plane": {
			opts:    func(o *Options) { o.Controller = rootStepsControllerPlaceholder },
			wantErr: "knows no control plane",
		},
		"the admin's repair uses the server": {
			opts: func(o *Options) {
				o.Controller = rootStepsControllerPlaceholder
				o.ServerURL = "https://100.64.0.20:6130"
			},
			want: "100.64.0.20",
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testWorkerOptions(t.TempDir())
			if tc.opts != nil {
				tc.opts(&opts)
			}
			if tc.config != "" {
				if err := os.MkdirAll(opts.ConfigDir(), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(configFilePath(opts), []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r := NewFakeRunner()
			for cmd, res := range tc.script {
				r.Script(cmd, res)
			}
			got, err := resolveNomadTargets(context.Background(), r, opts)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("= %v, %v; want an error with %q", got, err, tc.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			case got.pf() != tc.want:
				t.Errorf("targets = %q, want %q", got.pf(), tc.want)
			}
			for _, c := range r.Calls {
				if c.String() != tc.lookup {
					t.Errorf("unexpected command %q", c.String())
				}
			}
			if tc.lookup != "" && !r.CalledWith(tc.lookup) {
				t.Errorf("did not resolve through Runner: %v", r.Calls)
			}
		})
	}
}

// With no worker flags (grove doctor), the control plane comes from what is installed: the broker
// LaunchDaemon's --server.
func TestResolveNomadTargets_FromTheInstalledBroker(t *testing.T) {
	t.Setenv("GROVE_CONFIG", "")
	opts := testWorkerOptions(t.TempDir())
	opts.Controller = ""
	_, installed := daemonPaths(opts, brokerLabel)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := "<plist><array>\n<string>broker</string>\n<string>--server</string>\n<string>https://100.64.0.40:6130</string>\n</array></plist>\n"
	if err := os.WriteFile(installed, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveNomadTargets(context.Background(), NewFakeRunner(), opts)
	if err != nil || got.pf() != "100.64.0.40" {
		t.Fatalf("= %q, %v; want the installed broker's server", got.pf(), err)
	}
}

// The control plane on this same Mac: the pass names pf's `self`, and still comes before the block.
func TestHostFirewallRules_ControlPlaneOnThisMac(t *testing.T) {
	rules := hostFirewallRules(netip.MustParseAddr("192.168.64.1"), nil)
	if !strings.Contains(rules, `grove_nomad = "{ self }"`) {
		t.Errorf("rules don't pass Nomad RPC to this Mac:\n%s", rules)
	}
	if strings.Contains(rules, "to any port 4647") {
		t.Errorf("rules pass Nomad RPC to any address:\n%s", rules)
	}
}

// A control plane whose name does not resolve fails the firewall steps; nothing is staged that
// would open port 4647 to every address.
func TestHostFirewallSteps_UnresolvableControlPlaneFailsClosed(t *testing.T) {
	t.Setenv("GROVE_CONFIG", "")
	opts := testWorkerOptions(t.TempDir())
	r := NewFakeRunner() // dscacheutil prints nothing
	staged, _ := hostFirewallPaths(opts)
	for _, s := range hostFirewallSteps(r, opts) {
		if s.Name != "host-firewall-stage" {
			continue
		}
		if err := s.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "does not resolve") {
			t.Fatalf("Apply = %v, want it to fail on the name", err)
		}
	}
	if _, err := os.Stat(staged); err == nil {
		t.Error("staged a firewall plist without the control plane's address")
	}
	got := checkHostFirewall(context.Background(), r, opts)
	if got.OK || !strings.Contains(got.Detail, "does not resolve") || got.Remediation == "" {
		t.Errorf("doctor = %+v, want the unresolved name reported", got)
	}
}
