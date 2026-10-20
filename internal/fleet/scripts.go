package fleet

import (
	"fmt"
	"strings"
	"time"
)

// defaultShutdownTimeout is used when a Pool doesn't set ShutdownTimeout.
const defaultShutdownTimeout = 2*time.Hour + 30*time.Minute

// BuildStartupScript renders the guest StartupScript for a VM: it records which pool/worker/VM it
// belongs to and seals the guest (see sealedStartupScript): public DNS, the in-guest egress fence
// on Linux, the unprivileged job user on macOS, and nothing of the image's that would reach home.
// pool.StartupScript, if set, is appended verbatim after the generated part (see ARCHITECTURE.md
// → "Recycling").
func BuildStartupScript(pool Pool, worker, vmName string) string {
	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eu\n\n")
	fmt.Fprintf(&b, "grove_pool=%s\n", shQuote(pool.Name))
	fmt.Fprintf(&b, "grove_host=%s\n", shQuote(worker))
	fmt.Fprintf(&b, "grove_vm=%s\n\n", shQuote(vmName))
	b.WriteString(sealedStartupScript(pool))
	if pool.MacOS() && pool.HostXcode != nil {
		b.WriteString("\n")
		b.WriteString(buildHostXcodeStartupScript(*pool.HostXcode))
	}
	appendUserScript(&b, "pool.startupScript", pool.StartupScript)
	return b.String()
}

const guestHostXcodeMountPath = "/Volumes/My Shared Files/grove-xcode.app"

func buildHostXcodeStartupScript(config HostXcodeConfig) string {
	return strings.NewReplacer(
		"@GUEST_XCODE_MOUNT@", shQuote(guestHostXcodeMountPath),
		"@XCODE_BUILD_VERSION@", shQuote(config.BuildVersion),
	).Replace(hostXcodeStartupScript)
}

const hostXcodeStartupScript = `# Shared host Xcode must be ready before this guest can take a job.
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

if ! grove_priv true; then
  echo "grove: passwordless privilege is required for shared Xcode setup" >&2
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

// BuildShutdownScript renders the default guest ShutdownScript: stop new jobs landing in the VM,
// then wait (bounded by the returned timeout, defaulting to defaultShutdownTimeout) for the jobs
// still running in it to finish before Orchard deletes the VM. pool.ShutdownScript, if set, is
// appended verbatim after the generated part.
func BuildShutdownScript(pool Pool) (script string, timeout time.Duration) {
	timeout = pool.ShutdownTimeout.Std()
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}

	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eu\n\n")
	fmt.Fprintf(&b, "grove_deadline=$(( $(date +%%s) + %d ))\n", int(timeout.Seconds()))
	if pool.MacOS() {
		b.WriteString(sealedMacOSShutdownScriptBody)
	} else {
		b.WriteString(sealedShutdownScriptBody)
	}
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

// MacOSJobUser is the unprivileged account a macOS VM's job runs as (see internal/sandbox's macOS
// backend), and MacOSJobHome its home directory, which the job's run.sh exports as HOME.
const (
	MacOSJobUser = "_grovejob"
	MacOSJobHome = "/private/var/grove-job"
)

// macosJobUserScript creates MacOSJobUser on a macOS VM before it can take a job: a hidden account
// with no password and its own group, so a job's script cannot read root's files or grove's
// per-job control directory. Idempotent, since it runs on every boot. (Its first line is part of
// every sealed macOS VM's startup script; changing it recreates every such VM.)
const macosJobUserScript = `# Jobs run as ` + MacOSJobUser + `, never root (see internal/sandbox).
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
