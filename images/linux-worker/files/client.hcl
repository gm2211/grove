# Static half of the Nomad client config baked into the grove-linux-worker image.
# Anything that varies per VM (server addresses, pool/host meta) lives in grove-meta.hcl,
# which the fleet startup script overwrites on every boot. See docs/IMAGES.md.
#
# `nomad agent -config /etc/nomad.d` loads every *.hcl file in this directory and merges them,
# so this file and grove-meta.hcl combine into one client config.

data_dir  = "/opt/nomad/data"
bind_addr = "127.0.0.1"

client {
  enabled    = true
  node_class = "linux"

  options = {
    "driver.raw_exec.enable" = "1"
  }
}

plugin "docker" {
  config {
    # No host bind mounts and no privileged containers: a job that could mount the docker socket
    # or run privileged would be root on this VM, past the fence that keeps jobs off home
    # networks (see docs/JOBS.md).
    volumes {
      enabled = false
    }
    allow_privileged = false
  }
}

plugin "raw_exec" {
  config {
    enabled = true
  }
}

log_level = "INFO"
