package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/gm2211/grove/internal/broker"
)

// hostFirewallLabel is the LaunchDaemon that keeps the worker Mac's VM firewall loaded.
const hostFirewallLabel = "com.grove.pf-vm"

// hostFirewallAnchor hangs from /etc/pf.conf's anchor "com.apple/*", so the firewall needs no edit
// to the Mac's own pf.conf.
const hostFirewallAnchor = "com.apple/grove-vm"

// vmnetBridges are the interfaces vmnet's shared (NAT) network uses, whether a VM's traffic came
// through Softnet or not: bridge100 for the first network, the next ones if Internet Sharing or
// another virtualizer got there first. bridge0 (Thunderbolt Bridge) is not one of them. pf matches
// an interface by name, so a rule naming one that doesn't exist yet applies once it appears.
var vmnetBridges = []string{"bridge100", "bridge101", "bridge102", "bridge103", "bridge104",
	"bridge105", "bridge106", "bridge107", "bridge108", "bridge109"}

// hostFirewallRules is the pf anchor that holds outside the guests what their own fences hold
// inside: a VM reaches the internet through vmnet's NAT, and of this Mac and its networks only
// DHCP and DNS on the gateway, the credential broker on the gateway, and Nomad RPC (the control
// plane, which may be this Mac or a tailnet address behind it). Nothing a VM's root can change in
// the VM loosens it. The Mac's own connections to its VMs (Orchard's SSH, port forwards) are
// stateful passes, so their replies come back. IPv6 is closed but for neighbour discovery: grove's
// guests use IPv4, and IPv6 would give a VM the Mac's global address to reach. `self`, this Mac's
// addresses, is read when the anchor loads; the LaunchDaemon reloads it every five minutes.
func hostFirewallRules(gateway netip.Addr) string {
	return fmt.Sprintf(`# Installed by grove install --role worker. VMs on this Mac reach the internet, and of this Mac
# and its networks only DHCP, DNS and the credential broker on the vmnet gateway, and Nomad RPC.
grove_vm_if = "{ %s }"
grove_gateway = "%s"
table <grove_private> const { 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.168.0.0/16 198.18.0.0/15 224.0.0.0/4 240.0.0.0/4 }
pass out quick on $grove_vm_if all keep state
pass in quick on $grove_vm_if inet proto udp from any port 68 to any port 67 keep state
pass in quick on $grove_vm_if inet proto { udp tcp } from any to $grove_gateway port 53 keep state
pass in quick on $grove_vm_if inet proto tcp from any to $grove_gateway port %d keep state
pass in quick on $grove_vm_if inet proto tcp from any to any port 4647 keep state
block return in quick on $grove_vm_if inet from any to { <grove_private> self }
pass in quick on $grove_vm_if inet6 proto ipv6-icmp all keep state
block return in quick on $grove_vm_if inet6 all
`, strings.Join(vmnetBridges, " "), gateway, broker.Port)
}

// hostFirewallScript loads the anchor: /etc/pf.conf first only if its com.apple hook isn't in
// pf's main ruleset (so the Mac's own rules aren't reloaded every five minutes), then grove's
// anchor, then pf on if it's off. It fails unless the anchor's rules are live and pf is on.
func hostFirewallScript(gateway netip.Addr) string {
	return `set -eu
if ! /sbin/pfctl -s rules 2>/dev/null | /usr/bin/grep -q 'anchor "com.apple/\*"'; then
  /sbin/pfctl -q -f /etc/pf.conf
fi
/sbin/pfctl -q -a ` + hostFirewallAnchor + ` -f - <<'GROVE_PF_VM'
` + hostFirewallRules(gateway) + `GROVE_PF_VM
if ! /sbin/pfctl -s info 2>/dev/null | /usr/bin/grep -q 'Status: Enabled'; then
  /sbin/pfctl -q -E >/dev/null 2>&1 || /sbin/pfctl -q -e >/dev/null 2>&1 || true
fi
/sbin/pfctl -q -a ` + hostFirewallAnchor + ` -s rules | /usr/bin/grep -q 'port = ` + fmt.Sprint(broker.Port) + `'
/sbin/pfctl -s info 2>/dev/null | /usr/bin/grep -q 'Status: Enabled'
`
}

// hostFirewallPlist renders the LaunchDaemon. The whole loader, rules included, is inline: a root
// job must not run a script or binary this user could rewrite (Homebrew's grove, anything under
// ~ or a user-owned /usr/local), and /Library/LaunchDaemons is root's.
func hostFirewallPlist(gateway netip.Addr) string {
	script := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(hostFirewallScript(gateway))
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + hostFirewallLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>` + script + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StartInterval</key>
	<integer>300</integer>
</dict>
</plist>
`
}

// hostFirewallPaths are where the LaunchDaemon is staged (this user's, for the admin to copy) and
// where it is installed (root's).
func hostFirewallPaths(opts Options) (staged, installed string) {
	return filepath.Join(opts.ConfigDir(), "pf", hostFirewallLabel+".plist"),
		filepath.Join(opts.launchDaemonsDir(), hostFirewallLabel+".plist")
}

// hostFirewallRootScript is what runs as root, once per worker Mac and again after a grove
// upgrade changes the rules: copy the staged plist into place as root's, check the copy is
// byte-identical to the plist grove renders (the staged file is the user's to change, so it is
// pinned by SHA-256 like the daemons; a mismatch removes it and stops), and (re)load it.
func hostFirewallRootScript(staged, installed, plist string) string {
	sum := sha256.Sum256([]byte(plist))
	return fmt.Sprintf("/usr/bin/install -o root -g wheel -m 0644 %s %s\n"+
		"[ \"$(/usr/bin/shasum -a 256 < %s | /usr/bin/cut -d' ' -f1)\" = %s ] || { /bin/rm -f %s; echo %s >&2; exit 1; }\n"+
		"/bin/launchctl bootout system/%s 2>/dev/null || true\n"+
		"/bin/launchctl bootstrap system %s\n",
		shQuote(staged), shQuote(installed),
		shQuote(installed), shQuote(hex.EncodeToString(sum[:])), shQuote(installed),
		shQuote(installed+" is not the plist grove renders; removed it rather than load it"),
		hostFirewallLabel, shQuote(installed))
}

// hostFirewallSteps stage the VM firewall's LaunchDaemon and then install it. Installing is
// privileged, so it goes into the root script like pmset; the staged copy is only ever an input to
// that command, and both the install step's check and `grove doctor` compare what is installed
// with what grove renders, never with the staged file.
func hostFirewallSteps(r Runner, opts Options) []Step {
	staged, installed := hostFirewallPaths(opts)
	render := func(ctx context.Context) (string, error) {
		gw, err := VMNetGateway(ctx, r)
		if err != nil {
			return "", err
		}
		if !gw.Is4() {
			return "", fmt.Errorf("vmnet gateway %s is not an IPv4 address", gw)
		}
		return hostFirewallPlist(gw), nil
	}
	return []Step{
		{
			Name:        "host-firewall-stage",
			Description: fmt.Sprintf("Render %s, the LaunchDaemon that keeps VMs on this Mac off its other ports and its networks (pf anchor %s).", staged, hostFirewallAnchor),
			Check: func(ctx context.Context) (bool, error) {
				return fileHasContent(staged, func() (string, error) { return render(ctx) })(ctx)
			},
			Apply: func(ctx context.Context) error {
				content, err := render(ctx)
				if err != nil {
					return err
				}
				return writeFile(staged, content)
			},
		},
		{
			Name:        "host-firewall",
			Privileged:  true,
			Description: "Install and load the VM firewall (pinned by SHA-256) so VMs reach only DHCP, DNS, the broker and Nomad RPC on this Mac and its networks.",
			Root: func(ctx context.Context) (string, error) {
				plist, err := render(ctx)
				if err != nil {
					return "", err
				}
				return hostFirewallRootScript(staged, installed, plist), nil
			},
			Check: func(ctx context.Context) (bool, error) {
				ok, _, err := hostFirewallInstalled(ctx, r, opts, render)
				return ok, err
			},
			Apply: func(ctx context.Context) error {
				if _, _, err := r.Run(ctx, "sudo", "install", "-o", "root", "-g", "wheel", "-m", "0644", staged, installed); err != nil {
					return err
				}
				_, _, _ = r.Run(ctx, "sudo", "launchctl", "bootout", "system/"+hostFirewallLabel) // not loaded is fine
				_, _, err := r.Run(ctx, "sudo", "launchctl", "bootstrap", "system", installed)
				return err
			},
		},
	}
}

// hostFirewallInstalled reports whether the installed LaunchDaemon is exactly the one grove renders
// and its last run loaded the anchor. Reading pf's rules needs root, so its exit status is the
// evidence: the loader fails unless the anchor is live and pf is on.
func hostFirewallInstalled(ctx context.Context, r Runner, opts Options, render func(context.Context) (string, error)) (bool, string, error) {
	_, installed := hostFirewallPaths(opts)
	same, err := fileHasContent(installed, func() (string, error) { return render(ctx) })(ctx)
	if err != nil {
		return false, "", err
	}
	if !same {
		if _, statErr := os.Stat(installed); statErr != nil {
			return false, "not installed", nil
		}
		return false, installed + " is not the one this grove renders", nil
	}
	stdout, _, err := r.Run(ctx, "launchctl", "print", "system/"+hostFirewallLabel)
	if err != nil {
		return false, hostFirewallLabel + " is not loaded", nil
	}
	if !strings.Contains(stdout, "last exit code = 0") {
		return false, hostFirewallLabel + "'s last run failed to load the anchor (see `sudo launchctl print system/" + hostFirewallLabel + "`)", nil
	}
	return true, "anchor " + hostFirewallAnchor + " loaded by " + hostFirewallLabel, nil
}

// checkHostFirewall reports on the worker Mac's VM firewall.
func checkHostFirewall(ctx context.Context, r Runner, opts Options) CheckResult {
	const name = "host-firewall"
	render := func(ctx context.Context) (string, error) {
		gw, err := VMNetGateway(ctx, r)
		if err != nil {
			return "", err
		}
		return hostFirewallPlist(gw), nil
	}
	ok, detail, err := hostFirewallInstalled(ctx, r, opts, render)
	if err != nil {
		return CheckResult{Name: name, OK: false, Detail: err.Error()}
	}
	result := CheckResult{Name: name, OK: ok, Detail: detail}
	if !ok {
		result.Detail += ", so a VM whose root removes its own fence reaches every port on this Mac"
		result.Remediation = "run `grove setup` (or `grove install --role worker`) again: it runs the host-firewall root step with one sudo prompt, or prints one block for an admin"
	}
	return result
}
