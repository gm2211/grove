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
    # allow_privileged/volumes: the "build" job's runner container mounts the host docker socket
    # to run a nested container matching a job-requested image (see docs/JOBS.md — Nomad's docker
    # driver does not support interpolating `config.image` from dispatch-time meta, hashicorp/nomad#6247,
    # so grove-runner uses docker-in-docker instead of a dynamic image field).
    volumes {
      enabled = true
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
