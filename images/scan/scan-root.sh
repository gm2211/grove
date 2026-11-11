#!/bin/bash
# Scan an image's root filesystem for anything that identifies the Mac or network it was built on.
#
#   scan-root.sh <root>      <root> is the mounted (or booted, "/") guest filesystem
#
# A VM built on a Mac picks up that Mac's DNS settings over vmnet's DHCP: a tailnet's search
# domain ended up in /private/var/run/resolv.conf of a published image this way. An image may be
# public, so nothing host-derived may be baked into it. Exit 0 when clean, 1 with every hit listed
# (file and line, never more than the match) when not, 2 on a usage error.
set -u

root=${1:-}
[ -n "$root" ] && [ -d "$root" ] || { echo "usage: scan-root.sh <root>" >&2; exit 2; }
root=${root%/}

# Where host-derived state lands: network and system configuration, leases, and the home
# directories. Not the whole disk: Apple's own data (spelling lists, public-suffix lists) holds
# arbitrary names and is the same in every image.
dirs=(
  etc
  private/etc
  private/var/run
  private/var/db/dhcpclient
  private/var/db/SystemConfiguration
  Library/Preferences
  private/var/root
  Users
  home
  root
  var/lib/dhcp
  var/lib/NetworkManager
  var/lib/tailscale
  Library/Tailscale
  usr/local/etc
  opt/nomad
)

# What must never be there:
#   a tailnet name            <name>.ts.net
#   a Tailscale auth key      tskey-…
#   a GitHub token            ghp_ gho_ ghu_ ghs_ ghr_ github_pat_
#   a tailnet (CGNAT) address 100.64.0.0/10
pattern='[A-Za-z0-9-]+\.ts\.net|tskey-[A-Za-z0-9]+-[A-Za-z0-9]+|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|(^|[^0-9.])100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3}([^0-9.]|$)'

hits=0
for d in "${dirs[@]}"; do
  [ -e "$root/$d" ] || continue
  # -I skips binaries, -s unreadable files; -o prints only the match, so a token is never echoed
  # with its surroundings. Caches are skipped: they hold Apple's lists, not this build's state.
  out=$(grep -rIsnoE --exclude-dir=Caches --exclude-dir=.cache -e "$pattern" "$root/$d" 2>/dev/null | head -50)
  if [ -n "$out" ]; then
    hits=1
    printf '%s\n' "$out" | sed -E 's/(tskey-|gh[pousr]_|github_pat_)[A-Za-z0-9_-]+/\1…/'
  fi
done

if [ "$hits" -ne 0 ]; then
  echo "image scan: host-derived identifiers found (above); do not publish this image" >&2
  exit 1
fi
echo "image scan: clean ($root)"
