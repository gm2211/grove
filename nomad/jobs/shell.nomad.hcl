## Parameterized "shell" job template — rendered by the grove server (package nomadjobs) with a
## struct carrying at least {Kind: "shell", Pool: "linux"|"macos", CPU, Memory}. See README.md for
## the full dispatch contract and docs/JOBS.md.
##
## shell: runs the dispatched script as-is, no git clone (what `grove exec` and the MCP
## `grove_run` tool use). meta_optional still accepts repo/ref for contract symmetry with
## build/agent, but run.sh below ignores them.

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
        volumes = ["/var/run/docker.sock:/var/run/docker.sock"]
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

        if command -v docker >/dev/null 2>&1 && [ -n "${NOMAD_META_image:-}" ] && [ "${NOMAD_META_image}" != "$DEFAULT_IMAGE" ]; then
          exec docker run --rm \
            -v "${NOMAD_TASK_DIR}":/workspace -w /workspace \
            $(env | awk -F= '/^(NOMAD_META_|GH_TOKEN|GIT_TOKEN)/{print "-e", $1}') \
            "${NOMAD_META_image}" \
            timeout "${NOMAD_META_timeout_seconds:-3600}" bash -eo pipefail script.sh
        else
          exec timeout "${NOMAD_META_timeout_seconds:-3600}" bash -eo pipefail "$script_src"
        fi
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
