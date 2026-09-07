## Parameterized "build" job template — rendered by the grove server (package nomadjobs) with a
## struct carrying at least {Kind: "build", Pool: "linux"|"macos", CPU, Memory}. See README.md for
## the full dispatch contract and docs/JOBS.md for how internal/dispatch maps a JobRequest onto
## `nomad job dispatch`.
##
## build: clones repo@ref, runs the dispatched script, uploads ./artifacts/** to the artifact
## store, and exits with the script's exit code.

job "grove-{{.Kind}}-{{.Pool}}" {
  datacenters = ["dc1"]
  type        = "batch"

  parameterized {
    payload       = "required"
    meta_required = ["requester"]
    meta_optional = ["repo", "ref", "env_json", "timeout_seconds", "grove_meta_json", "artifact_prefix", "image"]
  }

  # Node-meta constraint. ${meta.pool} is set on the client node itself (client.hcl's
  # grove-meta.hcl, written by the fleet startup script) — a node attribute, not a runtime env
  # var, so it's usable in constraints (runtime env vars like ${NOMAD_META_x} are not: they don't
  # exist until after placement — see developer.hashicorp.com/nomad/docs/reference/runtime-variable-interpolation).
  constraint {
    attribute = "${meta.pool}"
    value     = "{{.Pool}}"
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
{{if eq .Pool "macos"}}
      # macOS guests have no container runtime; jobs run as native processes.
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
        # Nomad's docker driver cannot interpolate `image` from dispatch-time meta
        # (hashicorp/nomad#6247), so a per-job image override (meta_optional "image") is NOT a
        # different outer container — instead run.sh nests a `docker run` against this socket
        # when NOMAD_META_image differs from the default. See docs/JOBS.md.
        volumes = ["/var/run/docker.sock:/var/run/docker.sock"]
      }
{{end}}

      # The dispatched payload (the caller's script) lands at ${NOMAD_TASK_DIR}/script.sh.
      dispatch_payload {
        file = "script.sh"
      }

      # run.sh: applies env_json, clones repo@ref, runs script.sh under `timeout`, uploads
      # artifacts, and propagates script.sh's exit code as the task's exit code. Plain bash only,
      # no consul-template directives, since env is available natively via NOMAD_META_* and
      # NOMAD_TASK_DIR, which Nomad injects into every task regardless of driver.
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

        set +e
        if command -v docker >/dev/null 2>&1 && [ -n "${NOMAD_META_image:-}" ] && [ "${NOMAD_META_image}" != "$DEFAULT_IMAGE" ]; then
          docker run --rm \
            -v "$PWD":/workspace -w /workspace \
            $(env | awk -F= '/^(NOMAD_META_|GH_TOKEN|GIT_TOKEN)/{print "-e", $1}') \
            "${NOMAD_META_image}" \
            timeout "${NOMAD_META_timeout_seconds:-3600}" bash -eo pipefail "$(basename "$script_src")"
        else
          timeout "${NOMAD_META_timeout_seconds:-3600}" bash -eo pipefail "$script_src"
        fi
        code=$?
        set -e

        if [ -d ./artifacts ] && [ -n "${ARTIFACT_ENDPOINT:-}" ]; then
          prefix="${NOMAD_META_artifact_prefix:-${NOMAD_ALLOC_ID}}"
          mc alias set grove "$ARTIFACT_ENDPOINT" "$ARTIFACT_ACCESS_KEY" "$ARTIFACT_SECRET_KEY" >/dev/null
          mc cp --recursive ./artifacts/ "grove/${ARTIFACT_BUCKET}/${prefix}/" || true
        fi

        exit $code
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
