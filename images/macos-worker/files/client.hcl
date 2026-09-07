# Static half of the Nomad client config baked into the grove-macos-worker image.
# Anything that varies per VM (server addresses, pool/host meta) lives in grove-meta.hcl,
# which the fleet startup script overwrites on every boot. See docs/IMAGES.md.
#
# `nomad agent -config /usr/local/etc/nomad.d` loads every *.hcl file in this directory and
# merges them, so this file and grove-meta.hcl combine into one client config.

data_dir = "/opt/nomad/data"
bind_addr = "127.0.0.1"

client {
  enabled = true
  node_class = "macos"

  # macOS guests run jobs as native processes (raw_exec), never as containers — there is no
  # macOS container runtime. build/agent/shell task configs in nomad/jobs/*.nomad.hcl reflect
  # this via the pool-conditional driver choice.
  options = {
    "driver.raw_exec.enable" = "1"
  }

  # cpu_total_compute is deliberately NOT set here. Nomad's stock fingerprinter badly
  # under-reports CPU on Apple Silicon (observed cpu.totalcompute=24 on a real M5 Max — see
  # docs/OPERATIONS.md "jobs pending with DimensionExhausted cpu on macOS"), so it needs a manual
  # override — but this image is built once and run on whatever Mac model a worker happens to be,
  # so a single baked-in value here would be wrong on every other model. The override is instead
  # computed per boot from the guest's actual core count (internal/fleet/scripts.go's
  # StartupScript: ncpu * 2000 MHz) and written into the *dynamic* grove-meta.hcl (see
  # grove-meta.hcl.placeholder and docs/IMAGES.md's two-file config contract), which this file's
  # `nomad agent -config` merges with at startup.
}

plugin "raw_exec" {
  config {
    enabled = true
  }
}

log_level = "INFO"
