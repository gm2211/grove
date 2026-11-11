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

attached=$(/usr/bin/hdiutil attach -readonly -nomount -imagekey diskimage-class=CRawDiskImage "$disk")
device=$(printf '%s\n' "$attached" | /usr/bin/awk 'NR==1 {print $1}')
mnt=$(/usr/bin/mktemp -d)
cleanup() {
  /sbin/umount "$mnt" 2>/dev/null || true
  /usr/bin/hdiutil detach "$device" >/dev/null 2>&1 || true
  /bin/rmdir "$mnt" 2>/dev/null || true
}
trap cleanup EXIT

# The guest's Data volume holds /private/var, /Users and /Library; it is the APFS volume with
# that role in the disk's container.
data=$(/usr/sbin/diskutil list "$device" | /usr/bin/awk '/APFS Volume/ && / Data / {print $NF}' | /usr/bin/head -1)
[ -n "$data" ] || { echo "scan-tart-vm: no Data volume found on $device" >&2; exit 2; }
/sbin/mount_apfs -o rdonly,nobrowse "/dev/$data" "$mnt"

# On the Data volume the guest's /private/var is at /private/var, /Users at /Users.
"$here/scan-root.sh" "$mnt"
