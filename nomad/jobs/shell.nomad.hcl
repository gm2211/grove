## Parameterized "shell" job template — rendered by the grove server (package nomadjobs) with a
## struct carrying at least {Kind: "shell", Pool: "linux"|"macos", CPU, Memory}.
## See README.md for the full dispatch contract and docs/JOBS.md.
##
## shell: runs the dispatched script, no git clone (what `grove exec` and the MCP
## `grove_run` tool use). meta_optional still accepts repo/ref for contract symmetry with
## build/agent, but run.sh below ignores them.

job "grove-{{.Kind}}-{{.Pool}}" {
  datacenters = ["dc1"]
  type        = "batch"

  parameterized {
    payload       = "required"
    meta_required = ["requester"]
    meta_optional = ["repo", "ref", "env_json", "timeout_seconds", "grove_meta_json", "artifact_prefix", "image", "task_image", "retry_lost", "toolchain_json", "toolchain_hash"]
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

{{if eq .Pool "macos"}}
    # prepare: a root prestart task, on macOS only, that does for a shell job what "source" does
    # for a build or an agent before "main" starts: kill what earlier jobs left running as the
    # job user if the VM is otherwise idle, record this job, and make the job's own home. It
    # gets no payload and runs nothing of the caller's.
    task "prepare" {
      lifecycle {
        hook    = "prestart"
        sidecar = false
      }

      driver = "raw_exec"

      config {
        command = "/bin/bash"
        args    = ["${NOMAD_TASK_DIR}/prepare.sh"]
      }

      template {
        data        = <<-EOF
        #!/bin/bash
        set -uo pipefail
        umask 077

        # This job's state directory, and in it the job's own home (see run.sh), must be new and
        # this task's: every job in this VM runs as the same user, so one found already there
        # could be another job's doing.
        state="$NOMAD_ALLOC_DIR/data/grove"
        mkdir -p "$NOMAD_ALLOC_DIR/data"
        if ! mkdir "$state" || ! mkdir "$state/home"; then
          echo "grove: this allocation's state directory already exists; refusing to run a job in it" >&2
          exit 1
        fi

        # grove:jobs begin
        # Which jobs are running in this VM, kept by root for root: one record per allocation in a
        # directory only root can use, holding the time after which that job cannot still be
        # running. Every macOS job's root tasks keep it (the one before "main" adds the record,
        # "cleanup" removes it), so they can tell an idle VM, where every process of the job user
        # is something an earlier job left behind, from one where another job is still running
        # as that same user and must not be touched. See docs/JOBS.md "Jobs sharing a macOS VM".
        grove_jobs=/private/var/run/grove-jobs
        grove_job_id="$${NOMAD_ALLOC_ID:-}"
        case "$grove_job_id" in
          "" | *[!A-Za-z0-9-]*) grove_job_id="" ;;
        esac
        grove_job_secs="$${NOMAD_META_timeout_seconds:-3600}"
        case "$grove_job_secs" in
          "" | *[!0-9]*) grove_job_secs=3600 ;;
          ??????????*) grove_job_secs=999999999 ;;
        esac
        # Takes the directory's lock (a mkdir, which is atomic). A lock held for 30 seconds is
        # taken over: every holder is done in well under one, so its holder died.
        grove_jobs_lock() {
          local tries=0 total=0
          [ -n "$grove_job_id" ] || return 1
          mkdir -p "$grove_jobs" 2>/dev/null || return 1
          if [ -L "$grove_jobs" ] || [ ! -O "$grove_jobs" ]; then
            return 1
          fi
          chmod 0700 "$grove_jobs" 2>/dev/null || return 1
          until mkdir "$grove_jobs/.lock" 2>/dev/null; do
            tries=$((tries + 1))
            total=$((total + 1))
            [ "$total" -lt 450 ] || return 1
            if [ "$tries" -ge 150 ]; then
              rmdir "$grove_jobs/.lock" 2>/dev/null || true
              tries=0
            fi
            sleep 0.2
          done
        }
        grove_jobs_unlock() {
          rmdir "$grove_jobs/.lock" 2>/dev/null || true
        }
        # Records that this job may still be running for the next $1 seconds.
        grove_jobs_mark() {
          printf '%s\n' "$(( $(date +%s) + $1 ))" > "$grove_jobs/.new.$grove_job_id" &&
            mv -f "$grove_jobs/.new.$grove_job_id" "$grove_jobs/$grove_job_id"
        }
        # Prints how many other jobs may still be running here, dropping the records of those
        # that cannot be (their time is up: the VM or its Nomad client died under them).
        grove_jobs_others() {
          local now f at n=0
          now="$(date +%s)"
          for f in "$grove_jobs"/*; do
            [ -f "$f" ] || continue
            [ "$f" != "$grove_jobs/$grove_job_id" ] || continue
            at="$(cat "$f" 2>/dev/null || true)"
            case "$at" in
              "" | *[!0-9]*) at=0 ;;
            esac
            if [ "$at" -lt "$now" ]; then
              rm -f "$f"
              continue
            fi
            n=$((n + 1))
          done
          printf '%s\n' "$n"
        }
        # With no other job running here, kills every process of the job user: whatever an
        # earlier job left behind. With one running it does nothing, since the two can't be told
        # apart. Call it holding the lock, so no job starts in between.
        grove_jobs_sweep() {
          local round=0
          [ "$(grove_jobs_others)" = 0 ] || return 0
          while [ "$round" -lt 5 ] && pgrep -U _grovejob >/dev/null 2>&1; do
            pkill -KILL -U _grovejob >/dev/null 2>&1 || true
            round=$((round + 1))
            sleep 0.2
          done
        }
        # grove:jobs end

        # "main" starts next: it ends within its timeout (a few seconds after a cancel).
        grove_job_marked=""
        if grove_jobs_lock; then
          grove_jobs_sweep
          if grove_jobs_mark $((grove_job_secs + 900)); then
            grove_job_marked=1
          fi
          grove_jobs_unlock
        fi
        if [ -z "$grove_job_marked" ]; then
          echo "grove: could not record this job in $grove_jobs; processes other jobs left behind were not cleaned up" >&2
        fi

        # "main" runs as the unprivileged job user (see its `user`); hand it its home.
        if ! chown -R _grovejob "$state"; then
          echo "grove: could not hand the job its home directory" >&2
          exit 1
        fi
        exit 0
        EOF
        destination = "local/prepare.sh"
        perms       = "0700"
      }

      resources {
        cpu    = {{if .CPU}}{{.CPU}}{{else}}2000{{end}}
        memory = {{if .Memory}}{{.Memory}}{{else}}4096{{end}}
      }
    }
{{end}}
    task "main" {
      # The node's credential broker, if its startup script found one (internal/fleet's
      # brokerScript); run.sh checks both before using them. Unset node meta stays a literal
      # "${meta...}", which run.sh rejects.
      env {
        GROVE_BROKER    = "${meta.grove_broker}"
        GROVE_BROKER_CA = "${meta.grove_broker_ca}"
      }

{{if eq .Pool "macos"}}
      # macOS guests have no container runtime; jobs run as native processes.
      driver = "raw_exec"

      # Never root: like a build's or an agent's, the caller's script runs as the unprivileged
      # job user the fleet startup script creates (the VM's Nomad client is root, and without
      # `user` raw_exec runs the task as the client's user), so it cannot read or change the VM's
      # Nomad and Tailscale configuration or another task's root-owned files, and the guest's
      # firewall, which fences that user, applies to it. Nomad writes script.sh (the payload)
      # and run.sh world-readable, so the job user can read both.
      # It is NOT kept apart from other jobs in this VM: they all run as this one user, so a
      # job running at the same time can read and change this one's files and processes. What
      # a job that has ended leaves behind is dealt with: each job gets its own home (run.sh),
      # and the root tasks around "main" kill leftover processes ("prepare", "cleanup"). See
      # docs/JOBS.md "Jobs sharing a macOS VM".
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
{{if eq .Pool "macos"}}
        state="$NOMAD_ALLOC_DIR/data/grove"
        # This task runs as the unprivileged job user (see `user` above). Its home is this job's
        # alone: an empty directory in the allocation that the root task before this one made
        # and handed over (mode 0700), never the account's own home, which every job in this VM
        # would share, so no dotfile, PATH shim or tool configuration of an earlier job is
        # there. "cleanup" deletes it. The temp directory is this task's own too.
        export HOME="$state/home" USER=_grovejob LOGNAME=_grovejob
        if [ -L "$HOME" ] || [ ! -d "$HOME" ] || [ ! -O "$HOME" ]; then
          echo "grove: this job has no home directory of its own" >&2
          exit 1
        fi
        export TMPDIR="$(dirname "$NOMAD_TASK_DIR")/tmp"
{{end}}
        if [ -n "$${NOMAD_META_env_json:-}" ]; then
          eval "$(printf '%s' "$NOMAD_META_env_json" | jq -r 'to_entries[] | "export " + .key + "=" + (.value|tostring|@sh)')"
        fi

        # A job with named secrets (internal/secrets) has a framed payload, since secrets never
        # travel in dispatch meta (internal/dispatch payload.go): "grove-payload/2", an empty
        # line, one line holding the secrets (base64 of a JSON object of name to value), then the
        # script. Export them, and run the script from a copy without the frame. A frame that
        # can't be read fails the job.
        if [ "$(head -n 1 "$script_src")" = "grove-payload/2" ]; then
          grove_b64d() { printf '%s' "$1" | base64 --decode 2>/dev/null || printf '%s' "$1" | base64 -d; }
          grove_script="$(mktemp "$${TMPDIR:-/tmp}/grove-script.XXXXXX")"
          if ! grove_line="$(sed -n 3p "$script_src")" ||
            ! grove_secrets="$([ -z "$grove_line" ] || grove_b64d "$grove_line" | jq -r 'to_entries[] | select(.key | test("^[A-Za-z_][A-Za-z0-9_]*$")) | "export " + .key + "=" + (.value|tostring|@sh)')" ||
            ! tail -n +4 "$script_src" > "$grove_script"; then
            echo "grove: could not read this job's secrets" >&2
            exit 1
          fi
          rm -f "$script_src" 2>/dev/null || true
          script_src="$grove_script"
          eval "$grove_secrets"
          unset grove_line grove_secrets
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
        elif case "$(env)" in *gph1_*) true ;; *) false ;; esac; then
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
{{if eq .Pool "macos"}}
    # cleanup: a root task that runs once "main" has ended, however it ended. Every job in this
    # VM runs as the same user in a VM that is reused, so what a job leaves running could read
    # the next job's environment (its secrets, its credential placeholders). Nomad kills the
    # task's own process group; this deals with what left that group. See docs/JOBS.md "Jobs
    # sharing a macOS VM" for what it can and cannot do.
    task "cleanup" {
      lifecycle {
        hook = "poststop"
      }

      driver = "raw_exec"

      config {
        command = "/bin/bash"
        args    = ["${NOMAD_TASK_DIR}/cleanup.sh"]
      }

      template {
        data        = <<-EOF
        #!/bin/bash
        set -uo pipefail
        umask 077

        # grove:jobs begin
        # Which jobs are running in this VM, kept by root for root: one record per allocation in a
        # directory only root can use, holding the time after which that job cannot still be
        # running. Every macOS job's root tasks keep it (the one before "main" adds the record,
        # "cleanup" removes it), so they can tell an idle VM, where every process of the job user
        # is something an earlier job left behind, from one where another job is still running
        # as that same user and must not be touched. See docs/JOBS.md "Jobs sharing a macOS VM".
        grove_jobs=/private/var/run/grove-jobs
        grove_job_id="$${NOMAD_ALLOC_ID:-}"
        case "$grove_job_id" in
          "" | *[!A-Za-z0-9-]*) grove_job_id="" ;;
        esac
        grove_job_secs="$${NOMAD_META_timeout_seconds:-3600}"
        case "$grove_job_secs" in
          "" | *[!0-9]*) grove_job_secs=3600 ;;
          ??????????*) grove_job_secs=999999999 ;;
        esac
        # Takes the directory's lock (a mkdir, which is atomic). A lock held for 30 seconds is
        # taken over: every holder is done in well under one, so its holder died.
        grove_jobs_lock() {
          local tries=0 total=0
          [ -n "$grove_job_id" ] || return 1
          mkdir -p "$grove_jobs" 2>/dev/null || return 1
          if [ -L "$grove_jobs" ] || [ ! -O "$grove_jobs" ]; then
            return 1
          fi
          chmod 0700 "$grove_jobs" 2>/dev/null || return 1
          until mkdir "$grove_jobs/.lock" 2>/dev/null; do
            tries=$((tries + 1))
            total=$((total + 1))
            [ "$total" -lt 450 ] || return 1
            if [ "$tries" -ge 150 ]; then
              rmdir "$grove_jobs/.lock" 2>/dev/null || true
              tries=0
            fi
            sleep 0.2
          done
        }
        grove_jobs_unlock() {
          rmdir "$grove_jobs/.lock" 2>/dev/null || true
        }
        # Records that this job may still be running for the next $1 seconds.
        grove_jobs_mark() {
          printf '%s\n' "$(( $(date +%s) + $1 ))" > "$grove_jobs/.new.$grove_job_id" &&
            mv -f "$grove_jobs/.new.$grove_job_id" "$grove_jobs/$grove_job_id"
        }
        # Prints how many other jobs may still be running here, dropping the records of those
        # that cannot be (their time is up: the VM or its Nomad client died under them).
        grove_jobs_others() {
          local now f at n=0
          now="$(date +%s)"
          for f in "$grove_jobs"/*; do
            [ -f "$f" ] || continue
            [ "$f" != "$grove_jobs/$grove_job_id" ] || continue
            at="$(cat "$f" 2>/dev/null || true)"
            case "$at" in
              "" | *[!0-9]*) at=0 ;;
            esac
            if [ "$at" -lt "$now" ]; then
              rm -f "$f"
              continue
            fi
            n=$((n + 1))
          done
          printf '%s\n' "$n"
        }
        # With no other job running here, kills every process of the job user: whatever an
        # earlier job left behind. With one running it does nothing, since the two can't be told
        # apart. Call it holding the lock, so no job starts in between.
        grove_jobs_sweep() {
          local round=0
          [ "$(grove_jobs_others)" = 0 ] || return 0
          while [ "$round" -lt 5 ] && pgrep -U _grovejob >/dev/null 2>&1; do
            pkill -KILL -U _grovejob >/dev/null 2>&1 || true
            round=$((round + 1))
            sleep 0.2
          done
        }
        # grove:jobs end

        # 1. This job's own leftovers, even while other jobs are running: the job user's processes
        # that still carry this allocation's id in their environment, which every process the job
        # started inherited. A process that dropped its environment is not found this way; step 2
        # gets it once the VM is idle.
        job_uid="$(id -u _grovejob 2>/dev/null || true)"
        if [ -n "$grove_job_id" ] && [ -n "$job_uid" ]; then
          round=0
          while [ "$round" -lt 3 ]; do
            pids="$(ps -x -ww -E -U _grovejob -o pid=,uid=,command= 2>/dev/null |
              awk -v uid="$job_uid" -v want="NOMAD_ALLOC_ID=$grove_job_id" '$2 == uid { for (i = 3; i <= NF; i++) if ($i == want) { print $1; break } }')"
            [ -n "$pids" ] || break
            echo "grove: killing processes this job left behind:" $pids
            kill -KILL $pids 2>/dev/null || true
            round=$((round + 1))
            sleep 0.2
          done
        fi

        # 2. This job is over: drop its record and, if no other job is running in this VM, kill
        # everything still running as the job user.
        if grove_jobs_lock; then
          rm -f "$grove_jobs/$grove_job_id"
          grove_jobs_sweep
          grove_jobs_unlock
        else
          echo "grove: could not use $grove_jobs; leftover processes of the job user were not swept" >&2
        fi

        # 3. The job's home, with whatever it wrote there (tool logins, caches). Deleted as the
        # job user, from /, so nothing the job put in its place can point root anywhere.
        if [ -n "$${NOMAD_ALLOC_DIR:-}" ]; then
          (cd / && sudo -n -u _grovejob /bin/rm -rf -- "$NOMAD_ALLOC_DIR/data/grove/home") || true
        fi
        exit 0
        EOF
        destination = "local/cleanup.sh"
        perms       = "0700"
      }

      resources {
        cpu    = {{if .CPU}}{{.CPU}}{{else}}2000{{end}}
        memory = {{if .Memory}}{{.Memory}}{{else}}4096{{end}}
      }
    }
{{end}}
  }
}
