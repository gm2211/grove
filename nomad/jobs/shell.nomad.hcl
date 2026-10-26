## Parameterized "shell" job template — rendered by the grove server (package nomadjobs) with a
## struct carrying at least {Kind: "shell", Pool: "linux"|"macos", CPU, Memory}.
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
    meta_optional = ["repo", "ref", "env_json", "timeout_seconds", "grove_meta_json", "artifact_prefix", "image", "retry_lost"]
  }

  constraint {
    attribute = "${meta.pool}"
    value     = "{{.Pool}}"
  }
{{if .NodePool}}
  # The pool's VMs register in a node pool of its name, which their introduction tokens bind
  # them to (see internal/fleet's enrol.go): unlike meta.pool, which a node sets for itself, a VM
  # can't claim another pool's node pool, so it is never handed that pool's jobs.
  node_pool = "{{.NodePool}}"
{{end}}
  reschedule {
    attempts = 0
  }

  group "main" {
    restart {
      attempts = 0
    }

    task "main" {
      # The node's credential broker, if its startup script found one (internal/fleet's
      # brokerScript); run.sh checks both before using them. Unset node meta stays a literal
      # "${meta...}", which run.sh rejects.
      env {
        GROVE_BROKER    = "${meta.grove_broker}"
        GROVE_BROKER_CA = "${meta.grove_broker_ca}"
      }

{{if eq .Pool "macos"}}
      driver = "raw_exec"

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

        # Public resolvers, never the VM's own: a shared VM's resolver is its host Mac, and the
        # egress fence lets a container reach no private address at all.
        dns_servers = ["1.1.1.1", "8.8.8.8"]
        # Never the VM's docker socket: it would give the job root on the guest, past the fence
        # that keeps jobs off home networks. So a NOMAD_META_image override is ignored.
      }
{{end}}

      dispatch_payload {
        file = "script.sh"
      }

      template {
        data        = <<-EOF
        #!/bin/bash
        set -euo pipefail

        DEFAULT_IMAGE="{{.RunnerImage}}"
        script_src="$NOMAD_TASK_DIR/script.sh"
        T="$${NOMAD_META_timeout_seconds:-3600}"

        if [ -n "$${NOMAD_META_env_json:-}" ]; then
          eval "$(printf '%s' "$NOMAD_META_env_json" | jq -r 'to_entries[] | "export " + .key + "=" + (.value|tostring|@sh)')"
        fi

        # grove:broker begin
        # The worker's credential broker (internal/broker), which this VM's startup script found on
        # its gateway and whose CA it checked: the node advertises both as meta (see env below).
        # Every HTTP(S) request the job makes goes through it, and the CA it adds to the job's trust
        # is name-constrained to the hosts whose placeholders it swaps for real credentials.
        grove_broker="$${GROVE_BROKER:-}"
        grove_broker_ca="$${GROVE_BROKER_CA:-}"
        unset GROVE_BROKER GROVE_BROKER_CA
        case "$grove_broker" in
          http://*:3128) ;;
          *) grove_broker="" ;;
        esac
        case "$grove_broker_ca" in
          "" | *[!A-Za-z0-9+/=]*) grove_broker="" ;;
        esac
        if [ -n "$grove_broker" ]; then
          grove_bundle="$(mktemp "$${TMPDIR:-/tmp}/grove-ca.XXXXXX")"
          for grove_roots in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem; do
            if [ -s "$grove_roots" ]; then
              cat "$grove_roots" > "$grove_bundle"
              break
            fi
          done
          {
            echo "-----BEGIN CERTIFICATE-----"
            printf '%s\n' "$grove_broker_ca" | fold -w 64
            echo "-----END CERTIFICATE-----"
          } >> "$grove_bundle"
          chmod 0644 "$grove_bundle"
          export HTTPS_PROXY="$grove_broker" HTTP_PROXY="$grove_broker" https_proxy="$grove_broker" http_proxy="$grove_broker"
          export NO_PROXY="" no_proxy=""
          export SSL_CERT_FILE="$grove_bundle" CURL_CA_BUNDLE="$grove_bundle" GIT_SSL_CAINFO="$grove_bundle"
          export REQUESTS_CA_BUNDLE="$grove_bundle" NODE_EXTRA_CA_CERTS="$grove_bundle"
        elif env | grep -q 'gph1_'; then
          echo "grove: this job's environment holds credential placeholders (gph1_...), but this VM has no credential broker, so calls that need them will fail" >&2
        fi
        # grove:broker end

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

        # NOMAD_META_image is still accepted by the dispatch contract but ignored: every job runs
        # in this fixed runner image, and no job can reach the VM's docker daemon.
        run_with_timeout "$T" bash -eo pipefail "$script_src"
        exit $?
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
