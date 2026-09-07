## Parameterized "build" job template — rendered by the grove server (package nomadjobs) with a
## struct carrying at least {Kind: "build", Pool: "linux"|"macos", CPU, Memory, AllowDockerSocket}.
## See README.md for the full dispatch contract and docs/JOBS.md for how internal/dispatch maps a
## JobRequest onto `nomad job dispatch`.
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
{{if .AllowDockerSocket}}
        # OPT-IN (fleet.yaml pool.allowDockerSocket: true). This mounts the VM's docker socket
        # into every job on this pool: any job gets root-equivalent control of the VM host and
        # can reach every other job's containers through the same daemon, trading job-to-job
        # isolation for per-dispatch image selection. Default is false — see the branch below in
        # run.sh for what happens when this is off.
        volumes = ["/var/run/docker.sock:/var/run/docker.sock"]
{{end}}
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
        script_src="$NOMAD_TASK_DIR/script.sh"
        T="$${NOMAD_META_timeout_seconds:-3600}"

        if [ -n "$${NOMAD_META_env_json:-}" ]; then
          eval "$(printf '%s' "$NOMAD_META_env_json" | jq -r 'to_entries[] | "export " + .key + "=" + (.value|tostring|@sh)')"
        fi

        if [ -n "$${NOMAD_META_repo:-}" ]; then
          if [ -n "$${GH_TOKEN:-}$${GIT_TOKEN:-}" ]; then
            export GH_TOKEN="$${GH_TOKEN:-$GIT_TOKEN}"
            gh auth setup-git || true
          fi
          git clone --depth 50 "$NOMAD_META_repo" work
          cd work
          git checkout "$${NOMAD_META_ref:-HEAD}"
          cp "$script_src" ./script.sh
          script_src="$PWD/script.sh"
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

        set +e
{{if .AllowDockerSocket}}
        if command -v docker >/dev/null 2>&1 && [ -n "$${NOMAD_META_image:-}" ] && [ "$NOMAD_META_image" != "$DEFAULT_IMAGE" ]; then
          docker run --rm \
            -v "$PWD":/workspace -w /workspace \
            $(env | awk -F= '/^(NOMAD_META_|GH_TOKEN|GIT_TOKEN)/{print "-e", $1}') \
            "$NOMAD_META_image" \
            timeout "$T" bash -eo pipefail "$(basename "$script_src")"
        else
          run_with_timeout "$T" bash -eo pipefail "$script_src"
        fi
{{else}}
        # This pool has no docker-socket access (fleet.yaml pool.allowDockerSocket is unset or
        # false — the grove default). NOMAD_META_image is still accepted for dispatch-contract
        # symmetry with opted-in pools, but it's ignored here: every job runs in this fixed
        # grove-runner image, no nested `docker run` is ever attempted.
        run_with_timeout "$T" bash -eo pipefail "$script_src"
{{end}}
        code=$?
        set -e

        if [ -d ./artifacts ] && [ -n "$${ARTIFACT_ENDPOINT:-}" ]; then
          prefix="$${NOMAD_META_artifact_prefix:-$NOMAD_ALLOC_ID}"
          mc alias set grove "$ARTIFACT_ENDPOINT" "$ARTIFACT_ACCESS_KEY" "$ARTIFACT_SECRET_KEY" >/dev/null
          mc cp --recursive ./artifacts/ "grove/$ARTIFACT_BUCKET/$prefix/" || true
        fi

        exit $code
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
