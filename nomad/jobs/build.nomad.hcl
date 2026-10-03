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

    # source: a prestart task that fetches the job's inputs before "main" starts, so the
    # private-repo credential never enters the environment, files or process tree of the
    # caller's script. Only this task declares dispatch_payload, so only it receives the
    # payload (see internal/dispatch's encodePayload: a "grove-payload/1" header line, the clone
    # token or an empty line, then the caller's script). It writes the script and the checkout
    # into the shared alloc dir (alloc/data/grove), deletes the payload, and always exits 0:
    # its log and exit code are replayed by run.sh, so a failed clone still shows up in the
    # job's own logs and exit status rather than in a task grove never reads.
    task "source" {
      lifecycle {
        hook    = "prestart"
        sidecar = false
      }

{{if eq .Pool "macos"}}
      driver = "raw_exec"

      config {
        command = "/bin/bash"
        args    = ["${NOMAD_TASK_DIR}/source.sh"]
      }
{{else}}
      driver = "docker"

      config {
        image   = "{{.RunnerImage}}"
        command = "/bin/bash"
        args    = ["${NOMAD_TASK_DIR}/source.sh"]
      }
{{end}}

      dispatch_payload {
        file = "request"
      }

      template {
        data        = <<-EOF
        #!/bin/bash
        set -uo pipefail
        umask 077

        # Nomad writes the payload world-readable; close this task's directory before reading it.
        chmod 0700 "$NOMAD_TASK_DIR" 2>/dev/null || true

        req="$NOMAD_TASK_DIR/request"
        state="$NOMAD_ALLOC_DIR/data/grove"
        mkdir -p "$state"
        log="$state/source.log"
        : > "$log"

        finish() {
          rm -f "$req"
{{if eq .Pool "macos"}}
          # "main" runs as the unprivileged job user (see its `user`); hand it the inputs.
          chown -R _grovejob "$state" 2>/dev/null || true
{{end}}
          printf '%s\n' "$1" > "$state/source.exit"
          chmod 0644 "$state/source.exit" "$log" 2>/dev/null || true
          exit 0
        }

        token=""
        if [ "$(head -n 1 "$req")" = "grove-payload/1" ]; then
          token="$(sed -n 2p "$req")"
          tail -n +3 "$req" > "$state/script.sh"
        else
          cp "$req" "$state/script.sh"
        fi
        rm -f "$req"

        repo="$${NOMAD_META_repo:-}"
        ref="$${NOMAD_META_ref:-HEAD}"
        if [ -z "$repo" ]; then
          finish 0
        fi

        # A token the caller put in its own env still works for the clone, as before; the armed
        # control-plane token (from the payload) takes precedence.
        if [ -z "$token" ] && [ -n "$${NOMAD_META_env_json:-}" ]; then
          token="$(printf '%s' "$NOMAD_META_env_json" | jq -r '.GH_TOKEN // .GIT_TOKEN // empty')"
        fi

        case "$repo" in
          https://*) ;;
          *) echo "grove: repo must be an https:// URL" >> "$log"; finish 2 ;;
        esac
        case "$ref" in
          -*) echo "grove: ref must not start with '-'" >> "$log"; finish 2 ;;
        esac

        # The token reaches git only through this process's environment and an inline credential
        # helper: never argv (visible to every user in `ps`), never a config file, never the
        # checkout's .git/config.
        export GROVE_CLONE_TOKEN="$token"
        export GIT_TERMINAL_PROMPT=0
        helper='!f() { test "$1" = get || exit 0; echo username=x-access-token; printf "password=%s\n" "$GROVE_CLONE_TOKEN"; }; f'
        auth=(-c credential.helper=)
        if [ -n "$token" ]; then
          auth+=(-c "credential.helper=$helper")
        fi

        work="$state/work"
        if ! git "$${auth[@]}" clone --depth 50 -- "$repo" "$work" >> "$log" 2>&1; then
          finish 128
        fi
        if ! git -C "$work" checkout "$ref" -- >> "$log" 2>&1; then
          finish 1
        fi
        unset GROVE_CLONE_TOKEN
        cp "$state/script.sh" "$work/script.sh"
        finish 0
        EOF
        destination = "local/source.sh"
        perms       = "0700"
      }

      resources {
        cpu    = {{if .CPU}}{{.CPU}}{{else}}2000{{end}}
        memory = {{if .Memory}}{{.Memory}}{{else}}4096{{end}}
      }
    }

    task "main" {
{{if eq .Pool "macos"}}
      # macOS guests have no container runtime; jobs run as native processes.
      driver = "raw_exec"

      # Never root: the caller's script runs as the unprivileged job user the fleet startup
      # script creates, so it cannot read the "source" task's files, other jobs' state, or the
      # VM's Nomad and Tailscale configuration.
      user = "_grovejob"

      config {
        command = "/bin/bash"
        args    = ["${NOMAD_TASK_DIR}/run.sh"]
      }
{{else}}
      driver = "docker"

      config {
        image   = "{{.RunnerImage}}"
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

      # run.sh: checks the "source" task's result, applies env_json, runs script.sh in the
      # checkout under `timeout`, uploads artifacts, and propagates script.sh's exit code as the
      # task's exit code. Plain bash only,
      # no consul-template directives, since env is available natively via NOMAD_META_* and
      # NOMAD_TASK_DIR, which Nomad injects into every task regardless of driver.
      template {
        data        = <<-EOF
        #!/bin/bash
        set -euo pipefail

        DEFAULT_IMAGE="{{.RunnerImage}}"
        state="$NOMAD_ALLOC_DIR/data/grove"
        T="$${NOMAD_META_timeout_seconds:-3600}"

        # The "source" prestart task fetched this job's inputs (see above). Replay its log and
        # stop on its failure, so a failed clone reads in the job's logs and exit code exactly as
        # it did when run.sh cloned inline.
        if [ ! -f "$state/source.exit" ]; then
          echo "grove: the source step did not finish" >&2
          exit 1
        fi
        cat "$state/source.log" >&2 || true
        source_code="$(cat "$state/source.exit")"
        if [ "$source_code" != 0 ]; then
          exit "$source_code"
        fi
        script_src="$state/script.sh"
{{if eq .Pool "macos"}}
        # This task runs as the unprivileged job user (see `user` above), whose home and temp
        # directory the fleet startup script creates.
        export HOME=/private/var/grove-job USER=_grovejob LOGNAME=_grovejob
        export TMPDIR="$(dirname "$NOMAD_TASK_DIR")/tmp"
{{end}}
        if [ -n "$${NOMAD_META_env_json:-}" ]; then
          eval "$(printf '%s' "$NOMAD_META_env_json" | jq -r 'to_entries[] | select(.key | test("^[A-Za-z_][A-Za-z0-9_]*$")) | "export " + .key + "=" + (.value|tostring|@sh)')"
        fi

        if [ -n "$${NOMAD_META_repo:-}" ]; then
          cd "$state/work"
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
