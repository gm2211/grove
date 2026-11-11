#!/bin/bash
# Scrub a stopped local Tart VM's disk from the host: what the guest wrote about the build Mac
# after the in-guest scrub, up to shutdown. macOS only.
#
#   scrub-tart-vm.sh <tart vm name>
#
# The image build's last provisioner erases the unified logs inside the guest, but the guest keeps
# logging until it powers off: a pushed image's last Persist tracev3 was written 1.5 minutes after
# the provisioning shell ended and carried the tailnet search domain, and others carried the build
# Mac's mDNS name. So `make images-macos*` runs this on the stopped VM, then scan-tart-vm.sh, which
# stays the gate. Only the Data volume is touched; the sealed system volume is never mounted.
set -euo pipefail

vm=${1:-}
[ -n "$vm" ] || { echo "usage: scrub-tart-vm.sh <tart vm name>" >&2; exit 2; }
disk="${TART_HOME:-$HOME/.tart}/vms/$vm/disk.img"
[ -f "$disk" ] || { echo "scrub-tart-vm: no disk at $disk (is $vm a local Tart VM?)" >&2; exit 2; }

# Writing to a running guest's disk would corrupt it. Refuse unless Tart says it is stopped, and
# refuse when Tart can't be asked at all.
tart=${TART:-$(command -v tart || true)}
[ -n "$tart" ] || { echo "scrub-tart-vm: tart not found (set TART=/path/to/tart)" >&2; exit 2; }
state=$("$tart" list --source local --format json |
  /usr/bin/python3 -c 'import json,sys; n=sys.argv[1]; print(next((v.get("State","") for v in json.load(sys.stdin) if v.get("Name")==n), ""))' "$vm")
[ "$state" = "stopped" ] || { echo "scrub-tart-vm: $vm is ${state:-unknown to tart}, not stopped; refusing to write its disk" >&2; exit 2; }

attached=$(/usr/bin/hdiutil attach -nomount -imagekey diskimage-class=CRawDiskImage "$disk")
device=$(printf '%s\n' "$attached" | /usr/bin/awk 'NR==1 {print $1}')
mnt=$(/usr/bin/mktemp -d)
mounted=""
cleanup() {
  [ -z "$mounted" ] || /sbin/umount "$mnt" 2>/dev/null || /usr/sbin/diskutil unmount force "$mnt" >/dev/null 2>&1 || true
  /usr/bin/hdiutil detach "$device" >/dev/null 2>&1 || /usr/bin/hdiutil detach -force "$device" >/dev/null 2>&1 || true
  /bin/rmdir "$mnt" 2>/dev/null || true
}
trap cleanup EXIT

# Same lookup as scan-tart-vm.sh: the volumes are listed under the synthesized APFS containers.
data=""
for d in $(printf '%s\n' "$attached" | /usr/bin/awk '{print $1}' | /usr/bin/grep -E '^/dev/disk[0-9]+$' | /usr/bin/sort -u); do
  data=$(/usr/sbin/diskutil list "$d" | /usr/bin/awk '/APFS Volume/ && /Data/ {print $NF}' | /usr/bin/head -1)
  [ -z "$data" ] || break
done
[ -n "$data" ] || { echo "scrub-tart-vm: no Data volume found in $disk" >&2; exit 2; }
# noowners: the guest's files belong to the guest's root, and this runs as the host user.
/sbin/mount_apfs -o nobrowse,noowners "/dev/$data" "$mnt"
mounted=1
[ -d "$mnt/private/var" ] || { echo "scrub-tart-vm: /dev/$data has no private/var; not a macOS Data volume" >&2; exit 2; }

removed=0
gone() { # remove each existing path, counting them; never follows a symlink out of the volume
  local p
  for p in "$@"; do
    if [ -e "$p" ] || [ -L "$p" ]; then
      /bin/rm -rf -- "$p"
      removed=$((removed + 1))
    fi
  done
}

# The unified logs: empty each store but keep its directory, so logd finds it on first boot.
for store in Persist Special Signpost HighVolume; do
  dir="$mnt/private/var/db/diagnostics/$store"
  [ -d "$dir" ] || continue
  gone "$dir"/* "$dir"/.[!.]*
done
# What DHCP from the build Mac's vmnet handed the guest.
gone "$mnt/private/var/run/resolv.conf" "$mnt/private/etc/resolv.conf.grove-bak"
[ -d "$mnt/private/var/db/dhcpclient/leases" ] && gone "$mnt/private/var/db/dhcpclient/leases"/*
# Shell histories of every account, root included.
for home in "$mnt"/Users/* "$mnt/private/var/root"; do
  [ -d "$home" ] || continue
  gone "$home/.zsh_history" "$home/.bash_history" "$home/.zsh_sessions"
done

/bin/sync
echo "scrub-tart-vm: $vm: removed $removed path(s) from its Data volume"
