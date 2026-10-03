package fleet

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gm2211/grove/internal/orchard"
)

// Network modes for Pool.Network.
const (
	NetworkShared   = "shared"
	NetworkIsolated = "isolated"
)

// DefaultIsolatedTailscaleTag is the ACL tag isolated VMs claim when config.yaml sets no
// tailscale.tags. The tailnet policy must own this tag and grant it nothing but the Nomad
// servers' RPC port (docs/OPERATIONS.md "Isolating a pool's network").
const DefaultIsolatedTailscaleTag = "tag:grove-vm"

// softnetBlockHost refuses connections an isolated guest opens to its host Mac. Softnet's default
// policy allows the host (the vmnet gateway) on purpose, which would leave every host service
// listening on all interfaces reachable from a job. Blocking only the "out" direction keeps
// host-initiated flows working, which Orchard needs to SSH in and run the startup script.
// Directional rules need softnet 0.22.0 or newer.
const softnetBlockHost = "out @host"

// isolatedDNSServers replace the DHCP-provided resolver, which is the host Mac and therefore
// unreachable once softnetBlockHost applies.
var isolatedDNSServers = []string{"1.1.1.1", "8.8.8.8"}

var tailscaleTagRE = regexp.MustCompile(`^tag:[A-Za-z][A-Za-z0-9-]*$`)

// tailnetPrefixes are the address ranges Tailscale assigns to tailnet devices: the only
// non-public destinations an isolated guest can reach, and only through its own tagged login.
var tailnetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// Isolated reports whether p asks for network isolation (Pool.Network "isolated").
func (p Pool) Isolated() bool { return p.Network == NetworkIsolated }

func (o Options) tailscaleTags() []string {
	if len(o.TailscaleTags) == 0 {
		return []string{DefaultIsolatedTailscaleTag}
	}
	return o.TailscaleTags
}

// isolationProblem explains why grove can't build VMs for pool with the current configuration,
// or returns "" when it can (always, for a pool that isn't isolated). An isolated guest reaches
// only public addresses, so it can join Nomad only by logging into the tailnet itself and dialing
// the servers' tailnet IP: without an auth key, or with an RPC address it couldn't reach, the VM
// would boot and sit unusable. Plan refuses such a pool instead (see Plan.Blocked).
func (o Options) isolationProblem(pool Pool) string {
	if !pool.Isolated() {
		return ""
	}
	if o.TailscaleAuthKey == "" {
		return "network: isolated needs tailscale.authKey, because each VM joins the tailnet " +
			"under its own tagged login to reach the Nomad servers " +
			"(see docs/OPERATIONS.md \"Isolating a pool's network\")"
	}
	if o.NomadRPCAddress == "" {
		return "network: isolated needs nomad.url, so each VM knows which Nomad server to join"
	}
	host, _, err := net.SplitHostPort(o.NomadRPCAddress)
	if err != nil {
		return fmt.Sprintf("network: isolated could not read the Nomad RPC address %q: %v", o.NomadRPCAddress, err)
	}
	if !isTailnetIP(host) {
		return fmt.Sprintf("network: isolated needs nomad.url to use the control plane's tailnet IP "+
			"(100.x.y.z), not %q: isolated VMs can reach only public addresses and the tailnet, and "+
			"don't use MagicDNS", host)
	}
	for _, tag := range o.tailscaleTags() {
		if !tailscaleTagRE.MatchString(tag) {
			return fmt.Sprintf("tailscale.tags entry %q is not a Tailscale ACL tag (want tag:<name>)", tag)
		}
	}
	return ""
}

func isTailnetIP(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range tailnetPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// isolatedVM reports whether an existing VM already runs behind an isolated pool's Softnet
// fence, i.e. it is safe to leave running while its pool is blocked.
func isolatedVM(vm orchard.VM) bool {
	return vm.Softnet && slices.Contains(vm.SoftnetBlock, softnetBlockHost)
}

// tailscaleAuthKeyForVM turns a Tailscale OAuth client secret into an auth key for ephemeral,
// pre-approved devices: Tailscale accepts "tskey-client-…?ephemeral=true&preauthorized=true"
// directly as --auth-key, and unlike an auth key the secret doesn't expire after 90 days.
// Ordinary auth keys, and secrets that already carry options, pass through unchanged.
func tailscaleAuthKeyForVM(key string) string {
	if strings.HasPrefix(key, "tskey-client-") && !strings.Contains(key, "?") {
		return key + "?ephemeral=true&preauthorized=true"
	}
	return key
}

// isolatedNetworkScript renders the startup-script section for a network: isolated Linux guest.
// Callers check isolationProblem first, so authKey, tags and nomadRPCAddress are usable here.
func isolatedNetworkScript(authKey string, tags []string, nomadRPCAddress string) string {
	var resolv strings.Builder
	for _, server := range isolatedDNSServers {
		fmt.Fprintf(&resolv, "nameserver %s\n", server)
	}

	return strings.NewReplacer(
		"@RESOLV_CONF@", strings.TrimSuffix(resolv.String(), "\n"),
		"@EGRESS_FENCE_SCRIPT@", strings.TrimSuffix(egressFenceScript, "\n"),
		"@EGRESS_FENCE_DROPIN@", strings.TrimSuffix(egressFenceDockerDropIn, "\n"),
		"@TAILSCALE_AUTH_KEY@", shQuote(tailscaleAuthKeyForVM(authKey)),
		"@TAILSCALE_TAGS@", shQuote(strings.Join(tags, ",")),
		"@NOMAD_RPC_SERVER@", strconv.Quote(nomadRPCAddress),
	).Replace(isolatedNetworkScriptTemplate)
}

// isolatedNetworkScriptTemplate runs after nomadMetaScript (which defines grove_priv) and before
// the Nomad client restart, so the client comes back up already pointed at the tailnet RPC
// address it can reach.
const isolatedNetworkScriptTemplate = `# --- network: isolated ---
# Softnet keeps this guest off its host Mac, the LAN and the host's tailnet: it reaches only
# public addresses. The steps below make the guest work inside that fence, keep job containers
# from borrowing the guest's own tailnet login, and log the guest into the tailnet under a tag
# whose ACL reaches only the Nomad servers. See docs/OPERATIONS.md "Isolating a pool's network".
if [ "$(uname -s)" != "Linux" ]; then
  echo "grove: network: isolated is only supported on Linux guests" >&2
  exit 1
fi

# DHCP names the host Mac as the DNS server, and Softnet blocks the host. A plain file rather
# than systemd-resolved's stub is also what Docker copies into job containers.
grove_priv rm -f /etc/resolv.conf
grove_priv tee /etc/resolv.conf >/dev/null <<'GROVE_RESOLV_CONF'
@RESOLV_CONF@
GROVE_RESOLV_CONF

# The fence runs every time Docker starts (Docker refuses to start if it fails), and right now
# for the Docker that is already running.
grove_priv mkdir -p /usr/local/sbin /etc/systemd/system/docker.service.d
grove_priv tee /usr/local/sbin/grove-egress-fence >/dev/null <<'GROVE_EGRESS_FENCE'
@EGRESS_FENCE_SCRIPT@
GROVE_EGRESS_FENCE
grove_priv chmod 0755 /usr/local/sbin/grove-egress-fence
grove_priv tee /etc/systemd/system/docker.service.d/grove-egress-fence.conf >/dev/null <<'GROVE_EGRESS_FENCE_DROPIN'
@EGRESS_FENCE_DROPIN@
GROVE_EGRESS_FENCE_DROPIN
grove_priv systemctl daemon-reload
grove_priv /usr/local/sbin/grove-egress-fence

if ! command -v tailscale >/dev/null 2>&1; then
  echo "grove: this guest image has no tailscale; installing it the way images/linux-worker does" >&2
  grove_tailscale_installer=$(mktemp)
  curl -fsSL https://tailscale.com/install.sh -o "$grove_tailscale_installer"
  grove_priv sh "$grove_tailscale_installer"
  rm -f "$grove_tailscale_installer"
fi
grove_priv systemctl enable --now tailscaled
# A tagged, ephemeral device with no Tailscale SSH, no subnet routes, no tailnet DNS and no
# inbound tailnet connections. --reset drops any setting an earlier boot or the image left behind.
grove_priv tailscale up --reset --auth-key=@TAILSCALE_AUTH_KEY@ --hostname="$grove_vm" \
  --advertise-tags=@TAILSCALE_TAGS@ --accept-dns=false --accept-routes=false --ssh=false \
  --shields-up --timeout=120s

grove_priv tee /etc/nomad.d/grove-rpc.hcl >/dev/null <<'GROVE_NOMAD_RPC'
client {
  servers = [@NOMAD_RPC_SERVER@]
}
GROVE_NOMAD_RPC
`

// egressFenceScript is installed in isolated guests as /usr/local/sbin/grove-egress-fence. It is
// idempotent: it rebuilds grove's own chains from scratch and adds each jump only once.
//
// GROVE-EGRESS sits in DOCKER-USER, the chain Docker evaluates first for every forwarded packet
// and never rewrites. Job containers may still reach each other (docker0 and user-defined br-*
// bridges) and the public internet, but not private, tailnet (CGNAT) or link-local addresses, and
// nothing through the guest's tailnet link. GROVE-INGRESS keeps containers from opening
// connections to the guest itself, whose SSH and Nomad agent would otherwise be one hop away.
// Creating DOCKER-USER here, if Docker hasn't yet, lets the fence run before Docker starts.
const egressFenceScript = `#!/bin/sh
# Installed by grove's fleet startup script for pools with network: isolated, and re-run before
# every Docker start (see /etc/systemd/system/docker.service.d/grove-egress-fence.conf).
set -eu

iptables -w -N GROVE-EGRESS 2>/dev/null || true
iptables -w -F GROVE-EGRESS
iptables -w -A GROVE-EGRESS -o docker0 -j RETURN
iptables -w -A GROVE-EGRESS -o br-+ -j RETURN
iptables -w -A GROVE-EGRESS -o tailscale0 -j REJECT --reject-with icmp-admin-prohibited
for grove_cidr in 10.0.0.0/8 100.64.0.0/10 169.254.0.0/16 172.16.0.0/12 192.168.0.0/16; do
  iptables -w -A GROVE-EGRESS -d "$grove_cidr" -j REJECT --reject-with icmp-admin-prohibited
done
iptables -w -N DOCKER-USER 2>/dev/null || true
iptables -w -C DOCKER-USER -j GROVE-EGRESS 2>/dev/null || iptables -w -I DOCKER-USER 1 -j GROVE-EGRESS

iptables -w -N GROVE-INGRESS 2>/dev/null || true
iptables -w -F GROVE-INGRESS
iptables -w -A GROVE-INGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -w -A GROVE-INGRESS -i docker0 -j DROP
iptables -w -A GROVE-INGRESS -i br-+ -j DROP
iptables -w -C INPUT -j GROVE-INGRESS 2>/dev/null || iptables -w -I INPUT 1 -j GROVE-INGRESS
`

// egressFenceDockerDropIn makes Docker run egressFenceScript before every start. A rebooted
// guest's Nomad client can rejoin and receive jobs before Orchard reruns the startup script, so
// the fence can't wait for that; and if the fence fails, Docker (and so every job) stays down.
const egressFenceDockerDropIn = `# Installed by grove's fleet startup script for pools with network: isolated.
[Service]
ExecStartPre=/usr/local/sbin/grove-egress-fence
`
