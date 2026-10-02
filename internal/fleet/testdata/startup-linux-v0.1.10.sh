#!/bin/sh
set -eu

grove_pool='linux'
grove_host='mac1'
grove_vm='linux-mac1-0'

if [ "$(uname -s)" = "Darwin" ]; then
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

if [ "$(uname -s)" = "Darwin" ]; then
  grove_priv launchctl kickstart -k system/com.grove.nomad
else
  grove_priv systemctl restart nomad
fi
