## Parameterized job template for a SEALED pool (fleet.yaml `network: isolated`), used for every
## kind (build/agent/shell) in place of <kind>.nomad.hcl. Rendered by the grove server (package
## nomadjobs) with {Kind, Pool, CPU, Memory, RunnerImage, GroveBin, Slots, OS}.
##
## A sealed VM can reach the public internet and nothing at home, so it runs no Nomad client and
## this job is not placed in it. Nomad places it on the control plane's own client instead (the
## grove_sandbox_host node meta), where `grove sandbox run` drives the job from outside: it clones
## the repository there, pushes the checkout and script into a fresh container in one of the
## pool's sealed VMs, follows its logs, polls it for liveness, and pulls its artifacts and exit
## status back out. A macOS pool's VMs have no container runtime: there `grove sandbox run --os
## darwin` runs the job as the VM's unprivileged job user instead, and deletes the VM afterwards
## (one job per VM; the fleet reconciler recreates it). See internal/sandbox and
## docs/OPERATIONS.md "Isolating a pool's network".
##
## The dispatch contract is the same as the other templates': same meta, same payload, exit 124
## on a timeout. Exit 125 means the sealed VM was lost (internal/dispatch reports StatusLost).
## Sealed pools can't opt into the docker socket (fleet.yaml validation), so the "image" meta is
## accepted but ignored, as on any pool without it.

job "grove-{{.Kind}}-{{.Pool}}" {
  datacenters = ["dc1"]
  type        = "batch"

  parameterized {
    payload       = "required"
    meta_required = ["requester"]
    meta_optional = ["repo", "ref", "env_json", "timeout_seconds", "grove_meta_json", "artifact_prefix", "image"]
  }

  constraint {
    attribute = "${meta.grove_sandbox_host}"
    value     = "true"
  }

  # grove owns retries at the JobRequest level (see internal/dispatch); Nomad must not also retry.
  reschedule {
    attempts = 0
  }

  group "main" {
    restart {
      attempts = 0
    }

    task "main" {
      driver = "raw_exec"

      # Time for `grove sandbox run` to stop the job's container (an agent gets most of it to
      # shut down cleanly) and remove it.
      kill_timeout = "{{if eq .Kind "agent"}}10m{{else}}1m{{end}}"

      config {
        command = {{printf "%q" .GroveBin}}
        args = [
          "sandbox", "run",
          "--kind", "{{.Kind}}",
          "--pool", "{{.Pool}}",
          "--image", "{{.RunnerImage}}",
          "--slots", "{{.Slots}}",
          "--cpu", "{{if .CPU}}{{.CPU}}{{else}}2000{{end}}",
          "--memory", "{{if .Memory}}{{.Memory}}{{else}}4096{{end}}",
          "--payload", "${NOMAD_TASK_DIR}/request",{{if eq .OS "darwin"}}
          "--os", "darwin",{{end}}
        ]
      }

      # Only this task exists, so only it ever sees the payload (and the clone token in it); it
      # deletes the file as soon as it has read it.
      dispatch_payload {
        file = "request"
      }

      # The runner itself: the job's CPU and memory are enforced on its container in the VM.
      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
