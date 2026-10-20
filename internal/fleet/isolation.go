package fleet

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gm2211/grove/internal/orchard"
)

// Network modes for Pool.Network. Every pool is isolated: NetworkShared is still accepted in
// fleet.yaml, so an older spec loads, but Spec.Validate turns it into NetworkIsolated with a
// warning.
const (
	NetworkShared   = "shared"
	NetworkIsolated = "isolated"
)

// softnetBlockHost refuses connections an isolated guest opens to its host Mac. Softnet's default
// policy allows the host (the vmnet gateway) on purpose, which would leave every host service
// listening on all interfaces reachable from a job. Blocking only the "out" direction keeps
// host-initiated flows working, which Orchard needs to SSH in, run the startup script and drive
// each job. Directional rules need softnet 0.22.0 or newer.
const softnetBlockHost = "out @host"

// isolatedDNSServers replace the DHCP-provided resolver, which is the host Mac and therefore
// unreachable once softnetBlockHost applies.
var isolatedDNSServers = []string{"1.1.1.1", "8.8.8.8"}

// Isolated reports whether p's VMs are sealed. Always true: grove has no other kind of pool.
//
// Deprecated: kept for callers written when pools could share their host's network.
func (p Pool) Isolated() bool { return true }

// isolationProblem explains why grove can't build VMs for pool, or returns "" when it can. Every
// pool can be sealed today: a Linux VM's jobs run in containers, a macOS VM's as plain processes
// of its job user, one job per VM (see internal/sandbox). Plan still reports a problem here as
// Plan.Blocked, should a pool ever ask for isolation grove can't provide.
func isolationProblem(pool Pool) string {
	return ""
}

// IsolatedVM reports whether an existing VM already runs behind an isolated pool's Softnet
// fence, i.e. it is safe to leave running while its pool is blocked.
func IsolatedVM(vm orchard.VM) bool {
	return vm.Softnet && slices.Contains(vm.SoftnetBlock, softnetBlockHost)
}

// sealedStartupScript renders the generated part of a (sealed) guest's startup script. The macos
// pool gets sealedMacOSStartupScript.
func sealedStartupScript(pool Pool) string {
	if pool.MacOS() {
		return sealedMacOSStartupScript()
	}
	var resolv strings.Builder
	for _, server := range isolatedDNSServers {
		fmt.Fprintf(&resolv, "nameserver %s\n", server)
	}

	return strings.NewReplacer(
		"@GROVE_PRIV@", strings.TrimSuffix(grovePrivFunc, "\n"),
		"@RESOLV_CONF@", strings.TrimSuffix(resolv.String(), "\n"),
		"@EGRESS_FENCE_SCRIPT@", strings.TrimSuffix(egressFenceScript, "\n"),
		"@EGRESS_FENCE_DROPIN@", strings.TrimSuffix(egressFenceDockerDropIn, "\n"),
	).Replace(sealedStartupScriptTemplate)
}

// sealedMacOSStartupScript is sealedStartupScript for a macOS guest.
func sealedMacOSStartupScript() string {
	return strings.NewReplacer(
		"@GROVE_PRIV@", strings.TrimSuffix(grovePrivFunc, "\n"),
		"@DNS_SERVERS@", strings.Join(isolatedDNSServers, " "),
		"@JOB_USER@", strings.TrimSuffix(macosJobUserScript, "\n"),
	).Replace(sealedMacOSStartupScriptTemplate)
}

// grovePrivFunc runs a command as root: directly when the script already is, else via sudo.
const grovePrivFunc = `grove_priv() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  else
    sudo -n "$@"
  fi
}
`

// SealedDrainingMarker tells grove's job runners (internal/sandbox) to place no new job in a
// Linux VM whose shutdown script has started; they check for it right before creating a job's
// container. SealedDrainingMarkerMacOS is the same marker in a macOS VM, which has no /run; the
// runner also sets it itself once a macOS VM's one job is over.
const (
	SealedDrainingMarker      = "/run/grove-draining"
	SealedDrainingMarkerMacOS = "/private/var/run/grove-draining"
)

const sealedStartupScriptTemplate = `# --- network: isolated (sealed) ---
# Softnet keeps this guest off its host Mac, the LAN and the tailnet: it reaches only public
# addresses, and home reaches it only through Orchard. It runs no Nomad client and holds no
# credential for anything at home: grove's control plane drives each job from outside, pushing
# the job into a fresh container here, polling it and pulling its results back out (see
# docs/OPERATIONS.md "Isolating a pool's network").
if [ "$(uname -s)" != "Linux" ]; then
  echo "grove: network: isolated is only supported on Linux guests" >&2
  exit 1
fi

@GROVE_PRIV@

# The image's Nomad client and Tailscale would try to reach home; they stay off here.
for grove_unit in nomad.service tailscaled.service; do
  grove_priv systemctl disable --now "$grove_unit" >/dev/null 2>&1 || true
done
grove_priv rm -f ` + SealedDrainingMarker + `

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
grove_priv systemctl enable --now docker.service
`

// sealedShutdownScriptBody stops new jobs from landing in the VM, then waits (until
// grove_deadline, set by the caller) for the jobs still running in it to finish: their runners on
// the control plane collect each one's output and remove its container.
const sealedShutdownScriptBody = grovePrivFunc + `
grove_priv touch ` + SealedDrainingMarker + `
while [ "$(date +%s)" -lt "$grove_deadline" ]; do
  grove_remaining=$(grove_priv docker ps --quiet --filter label=grove.alloc 2>/dev/null | wc -l | tr -d ' ')
  if [ "$grove_remaining" = "0" ]; then
    break
  fi
  sleep 10
done
`

// sealedMacOSShutdownScriptBody is sealedShutdownScriptBody for a macOS VM, whose job runs as a
// process tree of the job user rather than in a container: it waits until that user runs nothing.
// The runner normally ends and kills the job (and deletes the VM) before this runs.
const sealedMacOSShutdownScriptBody = grovePrivFunc + `
grove_priv touch ` + SealedDrainingMarkerMacOS + `
while [ "$(date +%s)" -lt "$grove_deadline" ]; do
  if ! pgrep -U ` + MacOSJobUser + ` >/dev/null 2>&1; then
    break
  fi
  sleep 10
done
`

// sealedMacOSStartupScriptTemplate is the macOS counterpart of sealedStartupScriptTemplate. Images
// built before grove dropped Nomad carry a Nomad client (launchd job com.grove.nomad) and
// Tailscale's daemon (homebrew.mxcl.tailscale); each is disabled, so launchd doesn't load it again
// at the next boot, and booted out, which stops it now. Current images have neither.
const sealedMacOSStartupScriptTemplate = `# --- network: isolated (sealed, macOS) ---
# Softnet keeps this guest off its host Mac, the LAN and the tailnet: it reaches only public
# addresses, and home reaches it only through Orchard. It runs no Nomad client and holds no
# credential for anything at home: grove's control plane drives its one job from outside, as the
# unprivileged job user, then deletes the VM (see docs/OPERATIONS.md "Isolating a pool's
# network").
if [ "$(uname -s)" != "Darwin" ]; then
  echo "grove: this pool's sealed setup is for macOS guests" >&2
  exit 1
fi

@GROVE_PRIV@

# The image's Nomad client and Tailscale would try to reach home; they stay off here.
for grove_service in com.grove.nomad homebrew.mxcl.tailscale; do
  grove_priv launchctl disable "system/$grove_service" >/dev/null 2>&1 || true
  grove_priv launchctl bootout "system/$grove_service" >/dev/null 2>&1 || true
done
grove_priv pkill -x nomad >/dev/null 2>&1 || true
grove_priv pkill -x tailscaled >/dev/null 2>&1 || true
grove_priv rm -f ` + SealedDrainingMarkerMacOS + `

# DHCP names the host Mac as the DNS server, and Softnet blocks the host. Every network service
# (the first line of the listing is a legend; a leading * marks a disabled service) gets the
# public resolvers.
grove_dns_services=$(networksetup -listallnetworkservices | sed -e 1d -e 's/^\*//')
printf '%s\n' "$grove_dns_services" | while IFS= read -r grove_dns_service; do
  [ -n "$grove_dns_service" ] || continue
  grove_priv networksetup -setdnsservers "$grove_dns_service" @DNS_SERVERS@
done
grove_priv dscacheutil -flushcache >/dev/null 2>&1 || true
grove_priv killall -HUP mDNSResponder >/dev/null 2>&1 || true

@JOB_USER@
`

// egressFenceScript is installed in isolated guests as /usr/local/sbin/grove-egress-fence. It is
// idempotent: it rebuilds grove's own chains from scratch and adds each jump only once.
//
// GROVE-EGRESS sits in DOCKER-USER, the chain Docker evaluates first for every forwarded packet
// and never rewrites. Job containers may still reach each other (docker0 and user-defined br-*
// bridges) and the public internet, but not private, CGNAT (tailnet) or link-local addresses,
// should Softnet ever let them through. GROVE-INGRESS keeps containers from opening connections
// to the guest itself, whose SSH would otherwise be one hop away.
// Creating DOCKER-USER here, if Docker hasn't yet, lets the fence run before Docker starts.
const egressFenceScript = `#!/bin/sh
# Installed by grove's fleet startup script for pools with network: isolated, and re-run before
# every Docker start (see /etc/systemd/system/docker.service.d/grove-egress-fence.conf).
set -eu

iptables -w -N GROVE-EGRESS 2>/dev/null || true
iptables -w -F GROVE-EGRESS
iptables -w -A GROVE-EGRESS -o docker0 -j RETURN
iptables -w -A GROVE-EGRESS -o br-+ -j RETURN
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

// egressFenceDockerDropIn makes Docker run egressFenceScript before every start. A rebooted guest's
// Docker comes back, and grove can place a job in it, before Orchard reruns the startup script, so
// the fence can't wait for that; and if the fence fails, Docker (and so every job) stays down.
const egressFenceDockerDropIn = `# Installed by grove's fleet startup script for pools with network: isolated.
[Service]
ExecStartPre=/usr/local/sbin/grove-egress-fence
`
