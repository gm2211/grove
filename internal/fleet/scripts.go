package fleet

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// defaultShutdownTimeout is used when a Pool doesn't set ShutdownTimeout.
const defaultShutdownTimeout = 2*time.Hour + 30*time.Minute

// nomadDrainDeadline bounds how long Nomad itself waits for allocations to finish once told to
// drain, independent of grove's own poll loop below (which is bounded by ShutdownTimeout).
const nomadDrainDeadline = "2h"

// BuildStartupScript renders the guest StartupScript for a VM: it records which pool/worker/VM
// it belongs to (so the Nomad client inside can advertise it as node meta), optionally joins the
// tailnet, and restarts the Nomad client so the new meta takes effect. pool.StartupScript, if
// set, is appended verbatim after the generated part (see ARCHITECTURE.md → "Recycling").
func BuildStartupScript(pool Pool, worker, vmName, tailscaleAuthKey string) string {
	return buildStartupScript(pool, worker, vmName, Options{TailscaleAuthKey: tailscaleAuthKey})
}

// buildStartupScript is BuildStartupScript with the Reconciler's full Options. An isolated pool's
// section needs the auth key, tags and Nomad RPC address that Options.isolationProblem has
// already vetted; Plan never builds a spec for a blocked pool.
func buildStartupScript(pool Pool, worker, vmName string, opts Options) string {
	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eu\n\n")
	fmt.Fprintf(&b, "grove_pool=%s\n", shQuote(pool.Name))
	fmt.Fprintf(&b, "grove_host=%s\n", shQuote(worker))
	fmt.Fprintf(&b, "grove_vm=%s\n\n", shQuote(vmName))
	if pool.HostXcode != nil {
		b.WriteString(buildHostXcodeStartupScript(*pool.HostXcode))
		b.WriteString("\n")
	}

	b.WriteString(nomadMetaScript)
	b.WriteString("\n")
	if opts.NodeTokens != nil {
		b.WriteString(nomadClientACLScript)
		b.WriteString("\n")
	}

	if pool.Isolated() {
		b.WriteString(isolatedNetworkScript(opts.TailscaleAuthKey, opts.tailscaleTags(), opts.NomadRPCAddress))
		b.WriteString("\n")
	} else if opts.TailscaleAuthKey != "" {
		fmt.Fprintf(&b, "tailscale up --authkey=%s --hostname=\"$grove_vm\" --ssh --accept-routes\n\n",
			shQuote(opts.TailscaleAuthKey))
	}

	if pool.Name == "macos" {
		b.WriteString(macosJobUserScript)
		b.WriteString("\n")
		// The image binds Nomad to loopback. Nomad rejects implicit loopback
		// advertisement, even for a client, so declare it before restarting.
		b.WriteString(macosNomadAdvertiseScript)
		b.WriteString("\n")
		if opts.NomadRPCAddress != "" {
			fmt.Fprintf(&b, "grove_priv mkdir -p /usr/local/etc/nomad.d\ngrove_priv tee /usr/local/etc/nomad.d/grove-rpc.hcl >/dev/null <<'GROVE_NOMAD_RPC'\nclient {\n  servers = [%s]\n}\nGROVE_NOMAD_RPC\n\n", strconv.Quote(opts.NomadRPCAddress))
		}
		b.WriteString(nomadClientReadinessScript)
		b.WriteString("\n")
	}

	if pool.HostXcode != nil {
		b.WriteString(nomadRestartWithHostXcodeScript)
	} else if pool.Name == "macos" {
		b.WriteString(nomadRestartScript)
	} else {
		b.WriteString(nomadRestartScriptV010)
	}

	appendUserScript(&b, "pool.startupScript", pool.StartupScript)

	return b.String()
}

const guestHostXcodeMountPath = "/Volumes/My Shared Files/grove-xcode.app"

const macosNomadAdvertiseScript = `grove_priv tee /usr/local/etc/nomad.d/grove-advertise.hcl >/dev/null <<'GROVE_NOMAD_ADVERTISE'
advertise {
  http = "127.0.0.1:4646"
  rpc = "127.0.0.1:4647"
  serf = "127.0.0.1:4648"
}
GROVE_NOMAD_ADVERTISE
`

func buildHostXcodeStartupScript(config HostXcodeConfig) string {
	return strings.NewReplacer(
		"@GUEST_XCODE_MOUNT@", shQuote(guestHostXcodeMountPath),
		"@XCODE_BUILD_VERSION@", shQuote(config.BuildVersion),
	).Replace(hostXcodeStartupScript)
}

const hostXcodeStartupScript = `# Shared host Xcode must be ready before this guest can accept Nomad jobs.
if [ "$(uname -s)" != "Darwin" ]; then
  echo "grove: hostXcode is configured, but this guest is not macOS" >&2
  exit 1
fi

grove_priv() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  else
    sudo -n "$@"
  fi
}

# Keep this client out of service while the shared app and guest compatibility are checked.
if ! grove_priv true; then
  echo "grove: passwordless privilege is required for shared Xcode setup" >&2
  exit 1
fi
grove_nomad_status=0
grove_priv launchctl print system/com.grove.nomad >/dev/null 2>&1 || grove_nomad_status=$?
if [ "$grove_nomad_status" -eq 0 ]; then
  if ! grove_priv launchctl bootout system/com.grove.nomad; then
    echo "grove: could not stop the Nomad client before shared Xcode setup" >&2
    exit 1
  fi
elif [ "$grove_nomad_status" -ne 113 ]; then
  echo "grove: could not inspect the Nomad launch service (launchctl status $grove_nomad_status)" >&2
  exit 1
fi
grove_nomad_status=0
grove_priv launchctl print system/com.grove.nomad >/dev/null 2>&1 || grove_nomad_status=$?
if [ "$grove_nomad_status" -ne 113 ]; then
  echo "grove: Nomad launch service remained available during shared Xcode setup (launchctl status $grove_nomad_status)" >&2
  exit 1
fi

grove_xcode_app=@GUEST_XCODE_MOUNT@
grove_xcode_expected_build=@XCODE_BUILD_VERSION@
grove_xcode_waited=0
while [ ! -d "$grove_xcode_app" ]; do
  if [ "$grove_xcode_waited" -ge 120 ]; then
    echo "grove: timed out waiting for shared Xcode at $grove_xcode_app" >&2
    exit 1
  fi
  sleep 2
  grove_xcode_waited=$((grove_xcode_waited + 2))
done

grove_xcode_version_plist="$grove_xcode_app/Contents/version.plist"
grove_xcode_info_plist="$grove_xcode_app/Contents/Info.plist"
if [ ! -r "$grove_xcode_version_plist" ] || [ ! -r "$grove_xcode_info_plist" ]; then
  echo "grove: shared Xcode is missing Contents/version.plist or Contents/Info.plist" >&2
  exit 1
fi
if ! grove_xcode_build=$(/usr/libexec/PlistBuddy -c 'Print :ProductBuildVersion' "$grove_xcode_version_plist" 2>/dev/null); then
  echo "grove: could not read ProductBuildVersion from shared Xcode" >&2
  exit 1
fi
if [ "$grove_xcode_build" != "$grove_xcode_expected_build" ]; then
  echo "grove: shared Xcode build mismatch: expected $grove_xcode_expected_build, found $grove_xcode_build" >&2
  exit 1
fi
if ! grove_xcode_min_os=$(/usr/libexec/PlistBuddy -c 'Print :LSMinimumSystemVersion' "$grove_xcode_info_plist" 2>/dev/null); then
  echo "grove: could not read LSMinimumSystemVersion from shared Xcode" >&2
  exit 1
fi
grove_guest_os=$(sw_vers -productVersion)
grove_valid_version() {
  case "$1" in
    ''|*[!0-9.]*|.*|*..*|*.) return 1 ;;
    *.*) return 0 ;;
    *) return 1 ;;
  esac
}
case "$grove_guest_os" in
  *.*.*.*) echo "grove: invalid guest macOS version component count" >&2; exit 1 ;;
esac
case "$grove_xcode_min_os" in
  *.*.*.*) echo "grove: invalid Xcode minimum macOS version component count" >&2; exit 1 ;;
esac
if ! grove_valid_version "$grove_guest_os" || ! grove_valid_version "$grove_xcode_min_os"; then
  echo "grove: invalid macOS version: guest=$grove_guest_os Xcode minimum=$grove_xcode_min_os" >&2
  exit 1
fi
if ! awk -v guest="$grove_guest_os" -v minimum="$grove_xcode_min_os" 'BEGIN {
  ng = split(guest, g, /[.]/); nm = split(minimum, m, /[.]/);
  for (i = 1; i <= 3; i++) {
    have = (i <= ng ? g[i] + 0 : 0);
    need = (i <= nm ? m[i] + 0 : 0);
    if (have > need) exit 0;
    if (have < need) exit 1;
  }
  exit 0;
}'; then
  echo "grove: guest macOS $grove_guest_os is older than shared Xcode minimum $grove_xcode_min_os" >&2
  exit 1
fi

grove_priv xcode-select --switch "$grove_xcode_app/Contents/Developer"
grove_priv xcodebuild -license accept
grove_priv xcodebuild -runFirstLaunch
grove_xcode_version_output=$(grove_priv xcodebuild -version)
case "$grove_xcode_version_output" in
  *"Build version $grove_xcode_expected_build"*) ;;
  *) echo "grove: selected Xcode reported an unexpected build version" >&2; exit 1 ;;
esac
grove_macos_sdk=$(grove_priv xcrun --sdk macosx --show-sdk-path)
if [ ! -d "$grove_macos_sdk" ]; then
  echo "grove: shared Xcode did not provide a usable macOS SDK" >&2
  exit 1
fi
`

// BuildShutdownScript renders the default guest ShutdownScript: drain the local Nomad client,
// then wait (bounded by the returned timeout, defaulting to defaultShutdownTimeout) for zero
// non-terminal allocations before Orchard deletes the VM. pool.ShutdownScript, if set, is
// appended verbatim after the generated part.
func BuildShutdownScript(pool Pool) (script string, timeout time.Duration) {
	return buildShutdownScript(pool, false)
}

// buildShutdownScript is BuildShutdownScript, optionally with the node-token block whose
// placeholders Reconciler.Apply fills in with a freshly minted per-VM Nomad token. With Nomad ACLs
// on, `nomad node drain -self` needs node:write; without a token it fails and the VM is deleted
// under its running jobs.
func buildShutdownScript(pool Pool, nodeToken bool) (script string, timeout time.Duration) {
	timeout = pool.ShutdownTimeout.Std()
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}

	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eu\n\n")
	if nodeToken {
		b.WriteString(nodeTokenBlock)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "nomad node drain -self -enable -deadline %s -m %s\n\n",
		nomadDrainDeadline, shQuote("grove recycle"))
	fmt.Fprintf(&b, "grove_deadline=$(( $(date +%%s) + %d ))\n", int(timeout.Seconds()))
	b.WriteString(pollAllocsScript)

	appendUserScript(&b, "pool.shutdownScript", pool.ShutdownScript)

	return b.String(), timeout
}

func appendUserScript(b *strings.Builder, label, script string) {
	if strings.TrimSpace(script) == "" {
		return
	}

	fmt.Fprintf(b, "\n# --- %s ---\n", label)
	b.WriteString(script)

	if !strings.HasSuffix(script, "\n") {
		b.WriteString("\n")
	}
}

// shQuote wraps s in single quotes for safe interpolation into a POSIX shell script.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Placeholders in nodeTokenBlock. Plan leaves them in place, so a planned spec never holds a
// secret; Apply replaces them right before CreateVM.
const (
	nodeTokenSecretPlaceholder   = "@GROVE_NODE_TOKEN@"
	nodeTokenAccessorPlaceholder = "@GROVE_NODE_TOKEN_ACCESSOR@"
)

// nodeTokenBlock puts the VM's own node-scoped Nomad token (see nomad.NodeTokenPolicy) in the
// shutdown script's environment, and nowhere on the VM's disk, so jobs inside the VM never see it.
// The begin/end markers let specDrifted ignore the block: every VM gets a different token, and a VM
// made before node tokens existed isn't recreated just to get one.
const nodeTokenBlock = `# grove:node-token begin
# grove node token accessor ` + nodeTokenAccessorPlaceholder + ` (node-scoped; revoked once this VM is gone)
NOMAD_TOKEN=` + nodeTokenSecretPlaceholder + `
export NOMAD_TOKEN
# grove:node-token end
`

// nomadClientACLScript turns on ACL enforcement in the VM's own Nomad client (appended to the
// grove-meta.hcl nomadMetaScript just wrote). Without it, anything running in the VM could read
// other allocations' files and logs through the local client API on 127.0.0.1:4646. Only written
// when the control plane runs Nomad with ACLs (Options.NodeTokens set).
const nomadClientACLScript = `# grove:client-acl begin
grove_meta_append <<GROVE_META_ACL
acl {
  enabled = true
}
GROVE_META_ACL
# grove:client-acl end
`

// nomadMetaScript writes the Nomad client meta file naming this VM's pool/host/vm, at the path
// appropriate for the guest OS. On macOS it also overrides cpu_total_compute — see docs/IMAGES.md
// "The two-file config contract" and docs/OPERATIONS.md "jobs pending with DimensionExhausted cpu
// on macOS" for why this is necessary and computed here rather than baked into the image. Orchard
// executes StartupScript as the configured guest user, so writes under /etc and /usr/local use the
// noninteractive privilege helper below rather than assuming the guest process is root.
// MacOSJobUser is the unprivileged account macOS jobs run as (the `user` of the "main" task in
// nomad/jobs/build.nomad.hcl and agent.nomad.hcl), and MacOSJobHome its home directory, which
// run.sh exports as HOME.
const (
	MacOSJobUser = "_grovejob"
	MacOSJobHome = "/private/var/grove-job"
)

// macosJobUserScript creates MacOSJobUser on a macOS VM before its Nomad client can take work:
// a hidden account with no password and its own group, so a job's script cannot read root's
// files, other jobs' prestart inputs, or the VM's Nomad and Tailscale state. Idempotent, since it
// runs on every boot.
const macosJobUserScript = `# Jobs run as ` + MacOSJobUser + `, never root (see nomad/jobs/*.nomad.hcl).
if ! dscl . -read /Groups/` + MacOSJobUser + ` >/dev/null 2>&1; then
  grove_priv dscl . -create /Groups/` + MacOSJobUser + `
  grove_priv dscl . -create /Groups/` + MacOSJobUser + ` PrimaryGroupID 7460
  grove_priv dscl . -create /Groups/` + MacOSJobUser + ` Password '*'
fi
if ! dscl . -read /Users/` + MacOSJobUser + ` >/dev/null 2>&1; then
  grove_priv dscl . -create /Users/` + MacOSJobUser + `
  grove_priv dscl . -create /Users/` + MacOSJobUser + ` UniqueID 7460
  grove_priv dscl . -create /Users/` + MacOSJobUser + ` PrimaryGroupID 7460
  grove_priv dscl . -create /Users/` + MacOSJobUser + ` UserShell /bin/bash
  grove_priv dscl . -create /Users/` + MacOSJobUser + ` NFSHomeDirectory ` + MacOSJobHome + `
  grove_priv dscl . -create /Users/` + MacOSJobUser + ` RealName "grove job"
  grove_priv dscl . -create /Users/` + MacOSJobUser + ` Password '*'
  grove_priv dscl . -create /Users/` + MacOSJobUser + ` IsHidden 1
fi
grove_priv mkdir -p ` + MacOSJobHome + `
grove_priv chown ` + MacOSJobUser + `:` + MacOSJobUser + ` ` + MacOSJobHome + `
grove_priv chmod 0700 ` + MacOSJobHome + `
`

const nomadMetaScript = `if [ "$(uname -s)" = "Darwin" ]; then
  grove_meta_file="/usr/local/etc/nomad.d/grove-meta.hcl"
  # Apple Silicon's stock Nomad fingerprinter reports cpu.totalcompute in the single digits of MHz
  # (a real M5 Max fingerprinted cpu.totalcompute=24, cpu.frequency=4, cpu.numcores=18 — it's
  # treating GHz as MHz-per-core rather than deriving a usable total), which fails placement for
  # any job that requests a realistic CPU MHz value (DimensionExhausted cpu). The image is generic
  # across Mac models, so this can't be a fixed value baked into images/macos-worker/files/client.hcl
  # — it's computed here, per boot, from this guest's actual core count: ncpu * 2000 MHz/core,
  # the same 2000-MHz-per-core convention Nomad's own fingerprinter uses on Intel/Linux.
  grove_cpu_total_compute=$(( $(sysctl -n hw.ncpu) * 2000 ))
else
  grove_meta_file="/etc/nomad.d/grove-meta.hcl"
  grove_cpu_total_compute=""
fi

grove_priv() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  else
    sudo -n "$@"
  fi
}

grove_priv mkdir -p "$(dirname "$grove_meta_file")"
grove_meta_header() {
  grove_priv tee "$grove_meta_file" >/dev/null
}
grove_meta_append() {
  grove_priv tee -a "$grove_meta_file" >/dev/null
}

grove_meta_header <<GROVE_META
client {
  meta {
    pool = "$grove_pool"
    host = "$grove_host"
    vm = "$grove_vm"
  }
GROVE_META
if [ -n "$grove_cpu_total_compute" ]; then
  grove_meta_append <<GROVE_META_CPU
  cpu_total_compute = $grove_cpu_total_compute
GROVE_META_CPU
fi
grove_meta_append <<GROVE_META_END
}
GROVE_META_END
`

// nomadRestartScript restarts the Nomad client so a freshly written meta file takes effect.
const nomadRestartScript = `if [ "$(uname -s)" = "Darwin" ]; then
  grove_nomad_status=0
  grove_priv launchctl print system/com.grove.nomad >/dev/null 2>&1 || grove_nomad_status=$?
  if [ "$grove_nomad_status" -eq 113 ]; then
    grove_priv launchctl bootstrap system /Library/LaunchDaemons/com.grove.nomad.plist
  elif [ "$grove_nomad_status" -ne 0 ]; then
    echo "grove: could not inspect the Nomad launch service (launchctl status $grove_nomad_status)" >&2
    exit 1
  fi
  grove_priv launchctl kickstart -k system/com.grove.nomad
  groveWaitForNomadClient
else
  grove_priv systemctl restart nomad
fi
`

// nomadRestartScriptV010 preserves the exact generated Linux startup script used in v0.1.10.
// StartupScript is part of Orchard's VM spec, so changing this text would recreate healthy Linux
// workers during a macOS-only rollout.
const nomadRestartScriptV010 = `if [ "$(uname -s)" = "Darwin" ]; then
  grove_priv launchctl kickstart -k system/com.grove.nomad
else
  grove_priv systemctl restart nomad
fi
`

// nomadClientReadinessScript waits for the local Nomad client to report healthy after a macOS
// launchd restart. The agent health endpoint has no ACL requirement. Inspecting client.ok in the
// response deliberately ignores the server field: an isolated client can be locally ready even
// while it cannot reach the Nomad servers. A fixed retry count and per-request timeout bound this
// wait and prevent a successful launchctl command from being mistaken for a ready worker.
const nomadClientReadinessScript = `groveWaitForNomadClient() {
  if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    echo "grove: curl and jq are required to verify local Nomad client readiness" >&2
    return 1
  fi
  grove_nomad_attempt=0
  while [ "$grove_nomad_attempt" -lt 40 ]; do
    grove_nomad_health=$(curl --silent --max-time 2 http://127.0.0.1:4646/v1/agent/health 2>/dev/null || true)
    if printf '%s' "$grove_nomad_health" | jq -e '.client.ok == true' >/dev/null 2>&1; then
      return 0
    fi
    grove_nomad_attempt=$((grove_nomad_attempt + 1))
    sleep 1
  done
  echo "grove: Nomad client did not become healthy within 120 seconds" >&2
  return 1
}
`

// A host-shared Xcode setup deliberately bootouts Nomad first; reload its launch daemon only after
// mount, version, guest-OS, license, and first-launch checks all succeeded.
const nomadRestartWithHostXcodeScript = `if [ "$(uname -s)" = "Darwin" ]; then
  grove_priv launchctl bootstrap system /Library/LaunchDaemons/com.grove.nomad.plist
  grove_priv launchctl kickstart -k system/com.grove.nomad
  groveWaitForNomadClient
else
  grove_priv systemctl restart nomad
fi
`

// pollAllocsScript polls "nomad node status -self -json" until no allocation on this node is
// still pending/running, or until grove_deadline (set by the caller) passes. It prefers jq for
// robust JSON parsing but falls back to a grep-based count so it still degrades gracefully on
// images that don't ship jq.
const pollAllocsScript = `while :; do
  grove_status_json=$(nomad node status -self -json 2>/dev/null || echo '{}')
  if command -v jq >/dev/null 2>&1; then
    grove_remaining=$(printf '%s' "$grove_status_json" | \
      jq '[(.Allocations // [])[] | select(.ClientStatus=="pending" or .ClientStatus=="running")] | length' \
      2>/dev/null || echo "")
  else
    grove_remaining=$(printf '%s' "$grove_status_json" | \
      grep -o '"ClientStatus":"\(pending\|running\)"' | wc -l | tr -d ' ')
  fi
  [ -z "$grove_remaining" ] && grove_remaining=0

  if [ "$grove_remaining" -eq 0 ] 2>/dev/null; then
    break
  fi

  if [ "$(date +%s)" -ge "$grove_deadline" ]; then
    echo "grove: shutdown timeout reached with $grove_remaining allocation(s) still non-terminal" >&2
    break
  fi

  sleep 5
done
`
