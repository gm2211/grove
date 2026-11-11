#!/bin/bash
# Scan a stopped local Tart VM's disk with scan-root.sh, read-only. macOS only.
#
#   scan-tart-vm.sh <tart vm name>
#
# `make images-macos` runs it after the build, so an image that baked in the build Mac's DNS
# search domain (or anything else scan-root.sh knows) never gets as far as `tart push`.
set -euo pipefail

vm=${1:-}
[ -n "$vm" ] || { echo "usage: scan-tart-vm.sh <tart vm name>" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
disk="${TART_HOME:-$HOME/.tart}/vms/$vm/disk.img"
[ -f "$disk" ] || { echo "scan-tart-vm: no disk at $disk (is $vm a local Tart VM?)" >&2; exit 2; }

# hdiutil prints the physical disk first, then the synthesized APFS container disks; the volumes
# are listed only under the containers. Detaching the physical disk ejects them too.
attached=$(/usr/bin/hdiutil attach -readonly -nomount -imagekey diskimage-class=CRawDiskImage "$disk")
device=$(printf '%s\n' "$attached" | /usr/bin/awk 'NR==1 {print $1}')
mnt=$(/usr/bin/mktemp -d)
cleanup() {
  /sbin/umount "$mnt" 2>/dev/null || true
  /usr/bin/hdiutil detach "$device" >/dev/null 2>&1 || true
  /bin/rmdir "$mnt" 2>/dev/null || true
}
trap cleanup EXIT

# The guest's Data volume holds /private/var, /Users and /Library.
data=""
for d in $(printf '%s\n' "$attached" | /usr/bin/awk '{print $1}' | /usr/bin/grep -E '^/dev/disk[0-9]+$' | /usr/bin/sort -u); do
  data=$(/usr/sbin/diskutil list "$d" | /usr/bin/awk '/APFS Volume/ && /Data/ {print $NF}' | /usr/bin/head -1)
  [ -z "$data" ] || break
done
[ -n "$data" ] || { echo "scan-tart-vm: no Data volume found in $disk" >&2; exit 2; }
/sbin/mount_apfs -o rdonly,nobrowse "/dev/$data" "$mnt"

# What identifies this Mac and its operator, on top of what scan-root.sh always looks for.
ids=${GROVE_SCAN_IDENTIFIERS:-}
for name in "$(/usr/sbin/scutil --get LocalHostName 2>/dev/null)" "$(/usr/sbin/scutil --get ComputerName 2>/dev/null)" \
  "$(/usr/sbin/scutil --get HostName 2>/dev/null)" "$(/usr/bin/id -un)"; do
  [ -z "$name" ] || ids="$ids
$name"
done

GROVE_SCAN_IDENTIFIERS=$ids "$here/scan-root.sh" "$mnt"
