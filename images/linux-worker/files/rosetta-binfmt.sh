#!/bin/bash
# Mounts the host Rosetta 2 translator (shared into this VM by Tart as a virtiofs share tagged
# "rosetta", see linux-worker.pkr.hcl's `rosetta = "rosetta"` and `tart run --rosetta rosetta`)
# and registers it with binfmt_misc so the kernel routes x86_64 ELF execs through it. This lets
# amd64 Docker images run unmodified on this arm64 host. Idempotent: safe to run on every boot.
set -euo pipefail

MOUNT_POINT=/mnt/rosetta

mkdir -p "$MOUNT_POINT"
if ! mountpoint -q "$MOUNT_POINT"; then
  mount -t virtiofs rosetta "$MOUNT_POINT" || {
    echo "rosetta-binfmt: no 'rosetta' virtiofs share found (VM not started with --rosetta rosetta?); skipping" >&2
    exit 0
  }
fi

if [ ! -x "$MOUNT_POINT/rosetta" ]; then
  echo "rosetta-binfmt: $MOUNT_POINT/rosetta not found or not executable; skipping registration" >&2
  exit 0
fi

if [ ! -d /proc/sys/fs/binfmt_misc ]; then
  modprobe binfmt_misc || true
fi
if ! mountpoint -q /proc/sys/fs/binfmt_misc; then
  mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc || true
fi

if [ -e /proc/sys/fs/binfmt_misc/rosetta ]; then
  echo -1 > /proc/sys/fs/binfmt_misc/rosetta || true
fi

# Well-known Rosetta binfmt_misc registration (x86_64 ELF magic/mask), as documented by Apple for
# Virtualization.framework Linux guests and used by Lima/colima's rosetta.sh.
echo ':rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:'"$MOUNT_POINT"'/rosetta:OCF' > /proc/sys/fs/binfmt_misc/register

echo "rosetta-binfmt: registered $MOUNT_POINT/rosetta for x86_64 binaries"
