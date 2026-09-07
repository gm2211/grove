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
}

plugin "raw_exec" {
  config {
    enabled = true
  }
}

log_level = "INFO"
