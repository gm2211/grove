#!/bin/bash
# Scan an image's root filesystem for anything that identifies the Mac or network it was built on.
#
#   scan-root.sh <root>      <root> is the mounted (or booted, "/") guest filesystem
#
#   GROVE_SCAN_IDENTIFIERS   extra strings that must not appear anywhere, one per line or
#                            comma-separated: the build Mac's host and computer names, the
#                            operator's user name. scan-tart-vm.sh fills it in from the host.
#
# A VM built on a Mac picks up that Mac's DNS settings over vmnet's DHCP and sees its mDNS name:
# a tailnet's search domain ended up in /private/var/run/resolv.conf and in the unified logs
# (/private/var/db/diagnostics/*.tracev3) of a pushed image, and the build Mac's name in the logs
# too. An image may be public, so nothing host-derived may be baked into it.
#
# Exit 0 when clean, 1 with every hit listed, 2 on a usage error. A hit is printed as
# "<what>: <file>", never the matched text, so a token is never echoed.
set -u
export LC_ALL=C

root=${1:-}
[ -n "$root" ] && [ -d "$root" ] || { echo "usage: scan-root.sh <root>" >&2; exit 2; }
[ "$root" = / ] || root=${root%/}

# A booted macOS guest keeps /private/var, /Users and /Library on its Data volume, a separate
# mount that find -xdev would not cross into.
roots=("$root")
[ -d "${root%/}/System/Volumes/Data" ] && roots+=("${root%/}/System/Volumes/Data")

hits=0
report() { # report <what> < <(command listing files); not at the end of a pipe, which would lose hits
  local what=$1 f
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    hits=1
    printf '%s: %s\n' "$what" "$f"
  done
}

# Every regular file, binaries included (the unified logs are binary), never a FIFO or device:
# grep -r on a macOS volume blocks forever on a FIFO. Caches hold Apple's own lists (spelling,
# public suffixes), the same in every image, and are skipped.
all_files() {
  local r
  for r in "${roots[@]}"; do
    find "$r" -xdev \( -type d \( -name Caches -o -name .cache \) -prune \) -o -type f -print0 2>/dev/null
  done
}

# 1. Anywhere on the disk: a tailnet name, a Tailscale auth key, and the operator's identifiers.
#    Matching, not reading, is the slow part: a regex with a leading character class, or -i, over
#    tens of GB of binaries took BSD grep 23 minutes on a real macOS guest. So every file goes
#    through fixed-string greps only (".ts.net", "tskey-", and the identifiers, the only ones that
#    need -i), several at a time, and the few files that matched are read again with the real
#    patterns to say what each holds.
tailnet='[A-Za-z0-9-]+\.ts\.net'
tskey='tskey-[A-Za-z0-9-]{12,}'
ids=$(printf '%s\n' "${GROVE_SCAN_IDENTIFIERS:-}" | tr ',' '\n' | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' | awk 'length($0) >= 4')
idfile=$(mktemp)
candidates=$(mktemp)
trap 'rm -f "$idfile" "$candidates"' EXIT
[ -z "$ids" ] || printf '%s\n' "$ids" > "$idfile"
jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
# shellcheck disable=SC2016
all_files | GROVE_IDFILE=$idfile xargs -0 -P "$jobs" -n 200 sh -c '
  grep -laF -e .ts.net -e tskey- "$@" 2>/dev/null
  [ ! -s "$GROVE_IDFILE" ] || grep -laiF -f "$GROVE_IDFILE" "$@" 2>/dev/null
  exit 0' sh | sort -u | grep -vxF -e "$idfile" > "$candidates"

classify() { # classify <grep options and pattern...> : the candidates that match, one per line
  [ -s "$candidates" ] || return 0
  tr '\n' '\0' < "$candidates" | xargs -0 grep -la "$@" 2>/dev/null
}
report "tailnet name" < <(classify -E -e "$tailnet")
report "Tailscale auth key" < <(classify -E -e "$tskey")
[ -z "$ids" ] || report "operator identifier" < <(classify -iF -f "$idfile")

# 2. Where host-derived state and credentials land (configuration, leases, home directories):
#    a GitHub token, and a tailnet (CGNAT, 100.64.0.0/10) address. Not the whole disk: program
#    binaries hold token-shaped strings and documentation holds arbitrary addresses.
dirs=(etc private/etc private/var/run private/var/db/dhcpclient private/var/db/SystemConfiguration
  Library/Preferences private/var/root Users home root var/lib/dhcp var/lib/NetworkManager
  var/lib/tailscale Library/Tailscale usr/local/etc opt/nomad)
state_files() {
  local r d
  for r in "${roots[@]}"; do
    for d in "${dirs[@]}"; do
      [ -e "${r%/}/$d" ] || continue
      find "${r%/}/$d" -xdev \( -type d \( -name Caches -o -name .cache \) -prune \) -o -type f -print0 2>/dev/null
    done
  done
}
report "GitHub token" < <(state_files | xargs -0 grep -laE -e 'gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{30,}' 2>/dev/null)
report "tailnet address" < <(state_files | xargs -0 grep -lIE -e '(^|[^0-9.])100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3}([^0-9.]|$)' 2>/dev/null)

if [ "$hits" -ne 0 ]; then
  echo "image scan: host-derived identifiers found (above); do not publish this image" >&2
  exit 1
fi
echo "image scan: clean ($root)"
