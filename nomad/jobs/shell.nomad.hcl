## Parameterized "shell" job template — rendered by the grove server (package nomadjobs) with a
## struct carrying at least {Kind: "shell", Pool: "linux"|"macos", CPU, Memory, AllowDockerSocket}.
## See README.md for the full dispatch contract and docs/JOBS.md.
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
        script_src="$NOMAD_TASK_DIR/script.sh"
        T="$${NOMAD_META_timeout_seconds:-3600}"

        if [ -n "$${NOMAD_META_env_json:-}" ]; then
          eval "$(printf '%s' "$NOMAD_META_env_json" | jq -r 'to_entries[] | "export " + .key + "=" + (.value|tostring|@sh)')"
        fi

        # Portable timeout: macOS raw_exec hosts have no GNU coreutils `timeout(1)`. Prefer the
        # real `timeout` when present (Linux/docker runner image); otherwise run the command in
        # the background under a watchdog that SIGTERMs (then SIGKILLs, after a 10s grace) it
        # once $T elapses. Either way, forward SIGTERM/SIGINT (Nomad's kill_timeout) to the child
        # so a cancel actually reaches it, and return 124 on a timeout to preserve the existing
        # TimedOut contract (internal/dispatch maps 124 -> Job.TimedOut). See docs/JOBS.md.
        run_with_timeout() {
          local dur="$1"; shift
          local pid code watchdog marker
          if command -v timeout >/dev/null 2>&1; then
            timeout "$dur" "$@" &
            pid=$!
            trap 'kill -TERM "$pid" 2>/dev/null' TERM INT
            code=0
            wait "$pid" || code=$?
            trap - TERM INT
            return $code
          fi
          # set -m (job control) makes `&` put "$@" in its own process group, so the watchdog
          # below can kill -TERM/-KILL "-$pid" (the whole group, i.e. "$@" AND any grandchildren
          # it forks, e.g. a `sleep` at the end of a dispatched script.sh) instead of only the
          # immediate child — without it, SIGTERM only reaches the immediate child and any
          # grandchild is orphaned and keeps running for its full, unbounded duration.
          set -m
          "$@" &
          pid=$!
          set +m
          marker="$(mktemp)"
          rm -f "$marker"
          # The grace-period loop below (poll + `ps`, not a blind `sleep 10`) is required, not
          # just nicer: on macOS's bash 3.2, a bare `sleep "$dur"` here followed directly by
          # `kill -TERM "-$pid"` can leave the main flow's `wait "$pid"` below stuck indefinitely
          # (a SIGCHLD-notification race between this watchdog subprocess and run.sh's own
          # blocking wait for its child) even though the kill itself lands and the target dies
          # right away. Forking `ps` here reliably un-sticks it. See docs/JOBS.md.
          (
            sleep "$dur"
            : > "$marker" 2>/dev/null
            kill -TERM "-$pid" 2>/dev/null
            grace=0
            while [ "$grace" -lt 10 ]; do
              stat="$(ps -o stat= -p "$pid" 2>/dev/null)"
              case "$stat" in
                ""|*Z*) break ;;
              esac
              sleep 1
              grace=$((grace + 1))
            done
            kill -KILL "-$pid" 2>/dev/null
          ) &
          watchdog=$!
          trap 'kill -TERM "-$pid" 2>/dev/null; kill -TERM "$watchdog" 2>/dev/null' TERM INT
          code=0
          wait "$pid" || code=$?
          trap - TERM INT
          kill "$watchdog" 2>/dev/null
          wait "$watchdog" 2>/dev/null || true
          if [ -f "$marker" ]; then
            rm -f "$marker"
            return 124
          fi
          rm -f "$marker"
          return $code
        }

{{if .AllowDockerSocket}}
        if command -v docker >/dev/null 2>&1 && [ -n "$${NOMAD_META_image:-}" ] && [ "$NOMAD_META_image" != "$DEFAULT_IMAGE" ]; then
          exec docker run --rm \
            -v "$NOMAD_TASK_DIR":/workspace -w /workspace \
            $(env | awk -F= '/^(NOMAD_META_|GH_TOKEN|GIT_TOKEN)/{print "-e", $1}') \
            "$NOMAD_META_image" \
            timeout "$T" bash -eo pipefail script.sh
        else
          run_with_timeout "$T" bash -eo pipefail "$script_src"
          exit $?
        fi
{{else}}
        # This pool has no docker-socket access (fleet.yaml pool.allowDockerSocket is unset or
        # false — the grove default). NOMAD_META_image is still accepted for dispatch-contract
        # symmetry with opted-in pools, but it's ignored here: every job runs in this fixed
        # grove-runner image, no nested `docker run` is ever attempted.
        run_with_timeout "$T" bash -eo pipefail "$script_src"
        exit $?
{{end}}
        EOF
        destination = "local/run.sh"
        perms       = "0755"
      }

      # CPU/Memory come from fleet.yaml pools[].jobCPU/jobMemory (dispatch.PoolConfig,
      # defaulting to 500 MHz / 1024 MiB — see docs/JOBS.md "Per-job resource sizing"),
      # falling back to this template's own 2000/4096 only when the caller didn't supply
      # pool sizing at all. A dispatch-time JobRequest.Resources hint is validated against
      # these pool defaults but not applied here — Nomad has no per-dispatch resources
      # override for a parameterized job.
      resources {
        cpu    = {{if .CPU}}{{.CPU}}{{else}}2000{{end}}
        memory = {{if .Memory}}{{.Memory}}{{else}}4096{{end}}
      }
    }
  }
}
