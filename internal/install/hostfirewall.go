package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/gm2211/grove/internal/broker"
	"github.com/gm2211/grove/internal/config"
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

// nomadRPCPort is Nomad's RPC port. The control plane hands every VM <its Nomad host>:4647
// (fleet.NomadRPCAddressFromHTTPURL), so that is the one port the firewall opens towards it.
const nomadRPCPort = 4647

// rootStepsControllerPlaceholder stands in for a controller URL PendingRootSteps does not know (an
// admin repairing another user's worker). It names no control plane, so nomadRPCSource skips it.
const rootStepsControllerPlaceholder = "https://controller.not-used-by-root-steps.invalid"

// nomadTargets are the IPv4 addresses a VM on this Mac may reach Nomad RPC at: the control plane's.
// Empty means the control plane is this Mac, which pf's `self` names (read when the anchor loads,
// so it follows the Mac's addresses).
type nomadTargets []netip.Addr

// pf renders the targets as the inside of a pf address list.
func (n nomadTargets) pf() string {
	if len(n) == 0 {
		return "self"
	}
	out := make([]string, len(n))
	for i, a := range n {
		out[i] = a.String()
	}
	return strings.Join(out, " ")
}

// newNomadTargets keeps the IPv4 addresses of addrs, sorted and without duplicates, so the rendered
// plist does not depend on the order a resolver answered in. A loopback or unspecified address
// means the control plane is this Mac. ok is false when there is no IPv4 address at all: the
// firewall closes IPv6 to the guests, so an IPv6-only control plane cannot be reached anyway.
func newNomadTargets(addrs []netip.Addr) (targets nomadTargets, ok bool) {
	for _, a := range addrs {
		a = a.Unmap()
		if !a.Is4() {
			continue
		}
		if a.IsLoopback() || a.IsUnspecified() {
			return nil, true
		}
		targets = append(targets, a)
	}
	if len(targets) == 0 {
		return nil, false
	}
	slices.SortFunc(targets, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(targets), true
}

// nomadRPCSource is the URL whose host this worker takes to be the control plane's Nomad, and
// where it came from. The control plane derives the address it gives the VMs from its own
// nomad.url (internal/cli fleetOptions); a worker is told only the grove server and the Orchard
// controller, which grove's control-plane install runs on that same host. In order:
//
//  1. nomad.url in this user's config.yaml: the control plane's own Mac, or an operator's override
//     on a worker whose Nomad is not on the control plane's host;
//  2. the grove server (--server or the join result, else what the installed broker runs with);
//  3. the Orchard controller (--controller or the join result, else the installed worker's).
//
// The server comes before the controller because an admin's `grove setup --user` knows the server
// (the root-owned broker plist names it) but, on a repair, not the controller: both sides must
// render the same plist.
func nomadRPCSource(opts Options) (rawURL, from string) {
	if cfg, err := config.LoadFrom(configFilePath(opts)); err == nil && cfg.Nomad.URL != "" {
		return cfg.Nomad.URL, "nomad.url in " + configFilePath(opts)
	}
	if opts.ServerURL != "" {
		return opts.ServerURL, "the grove server URL"
	}
	if server := installedBrokerServer(opts); server != "" {
		return server, "the installed broker's --server"
	}
	if opts.Controller != "" && opts.Controller != rootStepsControllerPlaceholder {
		return opts.Controller, "the Orchard controller URL"
	}
	if controller := installedWorkerController(opts); controller != "" {
		return controller, "the installed worker's controller URL"
	}
	return "", ""
}

// installedBrokerServer is the --server of the broker job installed on this Mac (the root-owned
// LaunchDaemon, or this user's LaunchAgent when rootless), or "".
func installedBrokerServer(opts Options) string {
	_, path := daemonPaths(opts, brokerLabel)
	if opts.Rootless {
		path = agentPlistPath(opts, brokerLabel)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := plistServerRE.FindSubmatch(b)
	if m == nil {
		return ""
	}
	return html.UnescapeString(string(m[1]))
}

// lookupHostRE is what a name handed to the resolver may look like.
var lookupHostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252}[A-Za-z0-9])?\.?$`)

// resolveNomadTargets finds the control plane's addresses for the firewall's Nomad RPC pass. A
// name is resolved here, at plan time, through Runner and the Mac's own resolver (dscacheutil,
// which knows a tailnet's MagicDNS names): the root LaunchDaemon gets addresses and never looks
// anything up. A name that does not resolve is an error, never a fall back to "any address": that
// would quietly reopen port 4647 on every private network, and make the rendered plist flap with
// DNS.
func resolveNomadTargets(ctx context.Context, r Runner, opts Options) (nomadTargets, error) {
	rawURL, from := nomadRPCSource(opts)
	if rawURL == "" {
		return nil, fmt.Errorf("the VM firewall pins Nomad RPC to the control plane's address, and this install knows no control plane (no --server or --controller, and no worker installed here)")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("the VM firewall pins Nomad RPC to the control plane's address, but %s (%q) names no host", from, rawURL)
	}
	host := u.Hostname()
	if a, err := netip.ParseAddr(host); err == nil {
		targets, ok := newNomadTargets([]netip.Addr{a})
		if !ok {
			return nil, fmt.Errorf("the control plane's address %s (from %s) is not IPv4, and VMs reach Nomad RPC over IPv4 only", host, from)
		}
		return targets, nil
	}
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return nil, nil
	}
	if !lookupHostRE.MatchString(host) {
		return nil, fmt.Errorf("the control plane's host %q (from %s) is not a host name", host, from)
	}
	stdout, _, err := r.Run(ctx, "/usr/bin/dscacheutil", "-q", "host", "-a", "name", host)
	if err != nil {
		return nil, fmt.Errorf("resolve the control plane's host %s (from %s) for the VM firewall: %w", host, from, err)
	}
	var addrs []netip.Addr
	for _, line := range strings.Split(stdout, "\n") {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), "ip_address:"); found {
			if a, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
				addrs = append(addrs, a)
			}
		}
	}
	targets, ok := newNomadTargets(addrs)
	if !ok {
		return nil, fmt.Errorf("the control plane's host %s (from %s) does not resolve to an IPv4 address on this Mac, so the VM firewall cannot pin Nomad RPC to it; check Tailscale and DNS here, then run this again", host, from)
	}
	return targets, nil
}

// hostFirewallRules is the pf anchor that holds outside the guests what their own fences hold
// inside: a VM reaches the internet through vmnet's NAT, and of this Mac and its networks only
// DHCP and DNS on the gateway, the credential broker on the gateway, and Nomad RPC on the control
// plane's address (nomad: this Mac, or a tailnet address behind it), never port 4647 anywhere
// else. Nothing a VM's root can change in the VM loosens it. The Mac's own connections to its VMs
// (Orchard's SSH, port forwards) are stateful passes, so their replies come back. IPv6 is closed
// but for neighbour discovery: grove's guests use IPv4, and IPv6 would give a VM the Mac's global
// address to reach. `self`, this Mac's addresses, is read when the anchor loads; the LaunchDaemon
// reloads it every five minutes.
func hostFirewallRules(gateway netip.Addr, nomad nomadTargets) string {
	return fmt.Sprintf(`# Installed by grove install --role worker. VMs on this Mac reach the internet, and of this Mac
# and its networks only DHCP, DNS and the credential broker on the vmnet gateway, and Nomad RPC
# on the control plane.
grove_vm_if = "{ %s }"
grove_gateway = "%s"
grove_nomad = "{ %s }"
table <grove_private> const { 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.168.0.0/16 198.18.0.0/15 224.0.0.0/4 240.0.0.0/4 }
pass out quick on $grove_vm_if all keep state
pass in quick on $grove_vm_if inet proto udp from any port 68 to any port 67 keep state
pass in quick on $grove_vm_if inet proto { udp tcp } from any to $grove_gateway port 53 keep state
pass in quick on $grove_vm_if inet proto tcp from any to $grove_gateway port %d keep state
pass in quick on $grove_vm_if inet proto tcp from any to $grove_nomad port %d keep state
block return in quick on $grove_vm_if inet from any to { <grove_private> self }
pass in quick on $grove_vm_if inet6 proto ipv6-icmp all keep state
block return in quick on $grove_vm_if inet6 all
`, strings.Join(vmnetBridges, " "), gateway, nomad.pf(), broker.Port, nomadRPCPort)
}

// hostFirewallScript loads the anchor: /etc/pf.conf first only if its com.apple hook isn't in
// pf's main ruleset (so the Mac's own rules aren't reloaded every five minutes), then grove's
// anchor, then pf on if it's off. It fails unless the anchor's rules are live and pf is on.
func hostFirewallScript(gateway netip.Addr, nomad nomadTargets) string {
	return `set -eu
if ! /sbin/pfctl -s rules 2>/dev/null | /usr/bin/grep -q 'anchor "com.apple/\*"'; then
  /sbin/pfctl -q -f /etc/pf.conf
fi
/sbin/pfctl -q -a ` + hostFirewallAnchor + ` -f - <<'GROVE_PF_VM'
` + hostFirewallRules(gateway, nomad) + `GROVE_PF_VM
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
func hostFirewallPlist(gateway netip.Addr, nomad nomadTargets) string {
	script := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(hostFirewallScript(gateway, nomad))
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
// pinned by SHA-256 like the daemons, and the copy stays 0600 until it passes; a mismatch removes it
// and stops), and (re)load it.
func hostFirewallRootScript(staged, installed, plist string) string {
	sum := sha256.Sum256([]byte(plist))
	return fmt.Sprintf("/usr/bin/install -o root -g wheel -m 0600 %s %s\n"+
		"[ \"$(/usr/bin/shasum -a 256 < %s | /usr/bin/cut -d' ' -f1)\" = %s ] || { /bin/rm -f %s; echo %s >&2; exit 1; }\n"+
		"/bin/chmod 0644 %s\n"+
		strings.Join(systemReloadRoot(hostFirewallLabel, installed), "\n")+"\n",
		shQuote(staged), shQuote(installed),
		shQuote(installed), shQuote(hex.EncodeToString(sum[:])), shQuote(installed),
		shQuote(installed+" is not the plist grove renders; removed it rather than load it"),
		shQuote(installed))
}

// renderHostFirewall renders the LaunchDaemon for this Mac: its vmnet gateway and the control
// plane's addresses, both found at plan time through Runner. The install steps and `grove doctor`
// share it, so a control plane that moved shows up as an installed plist that is no longer the one
// grove renders.
func renderHostFirewall(ctx context.Context, r Runner, opts Options) (string, error) {
	gw, err := VMNetGateway(ctx, r)
	if err != nil {
		return "", err
	}
	if !gw.Is4() {
		return "", fmt.Errorf("vmnet gateway %s is not an IPv4 address", gw)
	}
	nomad, err := resolveNomadTargets(ctx, r, opts)
	if err != nil {
		return "", err
	}
	return hostFirewallPlist(gw, nomad), nil
}

// hostFirewallSteps stage the VM firewall's LaunchDaemon and then install it. Installing is
// privileged, so it goes into the root script like pmset; the staged copy is only ever an input to
// that command, and both the install step's check and `grove doctor` compare what is installed
// with what grove renders, never with the staged file.
func hostFirewallSteps(r Runner, opts Options) []Step {
	staged, installed := hostFirewallPaths(opts)
	render := func(ctx context.Context) (string, error) { return renderHostFirewall(ctx, r, opts) }
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
			Description: "Install and load the VM firewall (pinned by SHA-256) so VMs reach only DHCP, DNS and the broker on this Mac, and Nomad RPC on the control plane.",
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
				waitSystemUnloaded(ctx, r, hostFirewallLabel)
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
	render := func(ctx context.Context) (string, error) { return renderHostFirewall(ctx, r, opts) }
	ok, detail, err := hostFirewallInstalled(ctx, r, opts, render)
	if err != nil {
		return CheckResult{Name: name, OK: false, Detail: err.Error(),
			Remediation: "the VM firewall is rendered for this Mac's vmnet gateway and the control plane's address: fix what the message names (Tailscale and DNS for a name that does not resolve), then run `grove doctor` again"}
	}
	result := CheckResult{Name: name, OK: ok, Detail: detail}
	if !ok {
		result.Detail += ", so a VM whose root removes its own fence reaches every port on this Mac"
		result.Remediation = "run `grove setup` (or `grove install --role worker`) again: it runs the host-firewall root step with one sudo prompt, or prints one block for an admin"
	}
	return result
}
