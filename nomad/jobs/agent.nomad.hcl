## Parameterized "agent" job template — rendered by the grove server (package nomadjobs) with a
## struct carrying at least {Kind: "agent", Pool: "linux"|"macos", CPU, Memory, AllowDockerSocket}.
## See README.md for the full dispatch contract and docs/JOBS.md.
##
## agent: long-running coding-agent session (Claude Code / Codex) against a checkout of repo@ref.
## Same clone + env behavior as "build", but no artifact upload, and a generous kill_timeout so a
## cancel (`grove exec` cancel / DELETE /jobs/{id}) gives the agent process time to shut down
## cleanly instead of being SIGKILLed mid-write.

job "grove-{{.Kind}}-{{.Pool}}" {
  datacenters = ["dc1"]
  type        = "batch"

  parameterized {
    payload       = "required"
    meta_required = ["requester"]
    meta_optional = ["repo", "ref", "env_json", "timeout_seconds", "grove_meta_json", "artifact_prefix", "image"]
  }

  constraint {
    attribute = "${meta.pool}"
    value     = "{{.Pool}}"
  }

  reschedule {
    attempts = 0
  }

  group "main" {
    restart {
      attempts = 0
    }

    task "main" {
      kill_timeout = "10m"

{{if eq .Pool "macos"}}
      driver = "raw_exec"

      config {
        command = "/bin/bash"
        args    = ["${NOMAD_TASK_DIR}/run.sh"]
      }
{{else}}
      driver = "docker"

      config {
        image   = "ghcr.io/gm2211/grove-runner:latest"
        command = "/bin/bash"
        args    = ["${NOMAD_TASK_DIR}/run.sh"]
        # See build.nomad.hcl: docker `image` cannot be interpolated from dispatch meta
        # (hashicorp/nomad#6247); a NOMAD_META_image override is honored via nested `docker run`
        # in run.sh instead.
{{if .AllowDockerSocket}}
        # OPT-IN (fleet.yaml pool.allowDockerSocket: true) — see build.nomad.hcl for the
        # isolation trade-off this mount makes. Default is false.
        volumes = ["/var/run/docker.sock:/var/run/docker.sock"]
{{end}}
      }
{{end}}

      dispatch_payload {
        file = "script.sh"
      }

      template {
        data        = <<-EOF
        #!/bin/bash
        set -euo pipefail

        DEFAULT_IMAGE="ghcr.io/gm2211/grove-runner:latest"
        script_src="${NOMAD_TASK_DIR}/script.sh"

        if [ -n "${NOMAD_META_env_json:-}" ]; then
          eval "$(printf '%s' "$NOMAD_META_env_json" | jq -r 'to_entries[] | "export " + .key + "=" + (.value|tostring|@sh)')"
        fi

        if [ -n "${NOMAD_META_repo:-}" ]; then
          if [ -n "${GH_TOKEN:-}${GIT_TOKEN:-}" ]; then
            export GH_TOKEN="${GH_TOKEN:-$GIT_TOKEN}"
            gh auth setup-git || true
          fi
          git clone --depth 50 "$NOMAD_META_repo" work
          cd work
          git checkout "${NOMAD_META_ref:-HEAD}"
          cp "$script_src" ./script.sh
          script_src="$PWD/script.sh"
        fi

{{if .AllowDockerSocket}}
        if command -v docker >/dev/null 2>&1 && [ -n "${NOMAD_META_image:-}" ] && [ "${NOMAD_META_image}" != "$DEFAULT_IMAGE" ]; then
          exec docker run --rm \
            -v "$PWD":/workspace -w /workspace \
            $(env | awk -F= '/^(NOMAD_META_|GH_TOKEN|GIT_TOKEN)/{print "-e", $1}') \
            "${NOMAD_META_image}" \
            timeout "${NOMAD_META_timeout_seconds:-3600}" bash -eo pipefail "$(basename "$script_src")"
        else
          exec timeout "${NOMAD_META_timeout_seconds:-3600}" bash -eo pipefail "$script_src"
        fi
{{else}}
        # This pool has no docker-socket access (fleet.yaml pool.allowDockerSocket is unset or
        # false — the grove default). NOMAD_META_image is still accepted for dispatch-contract
        # symmetry with opted-in pools, but it's ignored here: every job runs in this fixed
        # grove-runner image, no nested `docker run` is ever attempted.
        exec timeout "${NOMAD_META_timeout_seconds:-3600}" bash -eo pipefail "$script_src"
{{end}}
        EOF
        destination = "local/run.sh"
        perms       = "0755"
      }

      resources {
        cpu    = {{if .CPU}}{{.CPU}}{{else}}2000{{end}}
        memory = {{if .Memory}}{{.Memory}}{{else}}4096{{end}}
      }
    }
  }
}
