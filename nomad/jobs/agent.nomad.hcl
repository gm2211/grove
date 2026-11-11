## Parameterized "agent" job template — rendered by the grove server (package nomadjobs) with a
## struct carrying at least {Kind: "agent", Pool: "linux"|"macos", CPU, Memory}.
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

        # Public resolvers, never the VM's own: a shared VM's resolver is its host Mac, and the
        # egress fence lets a container reach no private address at all.
        dns_servers = ["1.1.1.1", "8.8.8.8"]
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
{{if eq .Pool "macos"}}
        # This job's state directory, and in it the job's own home (see run.sh), must be new and
        # this task's: every job in this VM runs as the same user, so one found already there
        # could be another job's doing.
        mkdir -p "$NOMAD_ALLOC_DIR/data"
        if ! mkdir "$state" || ! mkdir "$state/home"; then
          echo "grove: this allocation's state directory already exists; refusing to run a job in it" >&2
          exit 1
        fi
{{else}}
        mkdir -p "$state"
{{end}}
        log="$state/source.log"
        : > "$log"
{{if eq .Pool "macos"}}
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

        # Before anything of this job runs as the job user: clear out what earlier jobs left
        # running, if the VM is otherwise idle, and record this job. The record's time allows for
        # the clone and a toolchain install; finish() resets it just before "main" starts.
        grove_job_marked=""
        if grove_jobs_lock; then
          grove_jobs_sweep
          if grove_jobs_mark $((grove_job_secs + 7200)); then
            grove_job_marked=1
          fi
          grove_jobs_unlock
        fi
        if [ -z "$grove_job_marked" ]; then
          echo "grove: could not record this job in $grove_jobs; processes other jobs left behind were not cleaned up" >> "$log"
        fi
{{end}}

        tokdir=""
        finish() {
          rm -f "$req"
          [ -z "$tokdir" ] || rm -rf "$tokdir"
{{if eq .Pool "macos"}}
          # "main" runs as the unprivileged job user (see its `user`); hand it the inputs and
          # its home.
          chown -R _grovejob "$state" 2>/dev/null || true
          # "main" starts next: it ends within its timeout, or ten minutes after a cancel.
          [ -z "$grove_job_marked" ] || grove_jobs_mark $((grove_job_secs + 900)) || true
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

        # The clone credential is offered only to the repository's own host: the helper below is
        # set for https://<host>, so git never asks it about a submodule, redirect or alternate on
        # another host. A URL whose host isn't a plain name (userinfo, brackets, anything else)
        # gets no credential at all.
        host="$${repo#https://}"
        host="$${host%%/*}"
        host_re='^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]+)?$'
        if [ -n "$token" ] && ! [[ "$host" =~ $host_re ]]; then
          echo "grove: the repo URL's host is not a plain host name, so the clone credential cannot be scoped to it" >> "$log"
          finish 2
        fi

        # The token reaches git only through this process's environment and an inline credential
        # helper: never argv (visible to every user in `ps`), never a config file, never the
        # checkout's .git/config.
        export GROVE_CLONE_TOKEN="$token"
        export GIT_TERMINAL_PROMPT=0
        helper='!f() { test "$1" = get || exit 0; echo username=x-access-token; printf "password=%s\n" "$GROVE_CLONE_TOKEN"; }; f'
{{if eq .Pool "macos"}}
        # git runs as its own unprivileged user, which the guest's firewall keeps off home networks
        # like the job user (root, which this task runs as, can't be fenced: the VM's Nomad client
        # is root too). sudo drops the environment, so the token reaches git's credential helper
        # through a file only that user can read, never argv; finish() removes it.
        # From /, since this task's own directory is closed to that user.
        as_src() { (cd / && sudo -n -H -u _grovesrc env HOME=/var/empty GIT_TERMINAL_PROMPT=0 "$@"); }
        chown _grovesrc "$state"
        if [ -n "$token" ]; then
          tokdir="$(mktemp -d)"
          printf '%s' "$token" > "$tokdir/token"
          chmod 0400 "$tokdir/token"
          chown -R _grovesrc "$tokdir"
          helper='!f() { test "$1" = get || exit 0; echo username=x-access-token; printf "password=%s\n" "$(cat '"$tokdir"'/token)"; }; f'
        fi
{{else}}
        as_src() { "$@"; }
{{end}}
        auth=(-c credential.helper=)
        if [ -n "$token" ]; then
          auth+=(-c "credential.https://$host.helper=$helper")
        fi

        work="$state/work"
        if ! as_src git "$${auth[@]}" clone --depth 50 -- "$repo" "$work" >> "$log" 2>&1; then
          finish 128
        fi
        # A shallow clone has only the default branch: fetch another branch, or a commit on no
        # branch it has (a pull request's head), before checking it out.
        if [ "$ref" != HEAD ] && ! as_src git -C "$work" rev-parse --verify --quiet "$ref^{commit}" > /dev/null 2>&1; then
          case "$ref" in
            *[!0-9a-f]*) spec="+refs/heads/$ref:refs/heads/$ref" ;;
            *) spec="$ref" ;;
          esac
          as_src git "$${auth[@]}" -C "$work" fetch --quiet --depth 50 origin "$spec" >> "$log" 2>&1 || true
        fi
        if ! as_src git -C "$work" checkout "$ref" -- >> "$log" 2>&1; then
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
      # The node's credential broker, if its startup script found one (internal/fleet's
      # brokerScript); run.sh checks both before using them. Unset node meta stays a literal
      # "${meta...}", which run.sh rejects.
      env {
        GROVE_BROKER    = "${meta.grove_broker}"
        GROVE_BROKER_CA = "${meta.grove_broker_ca}"
      }

      kill_timeout = "10m"

{{if eq .Pool "macos"}}
      driver = "raw_exec"

      # Never root: the caller's script runs as the unprivileged job user the fleet startup
      # script creates, so it cannot read the "source" task's files or the VM's Nomad and
      # Tailscale configuration. It is NOT kept apart from other jobs in this VM: they all run
      # as this one user, so a job running at the same time can read and change this one's
      # files and processes. What a job that has ended leaves behind is dealt with: each job
      # gets its own home (run.sh), and the root tasks around "main" kill leftover processes
      # (grove:jobs in "source", and "cleanup"). See docs/JOBS.md "Jobs sharing a macOS VM".
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
          eval "$(printf '%s' "$NOMAD_META_env_json" | jq -r 'to_entries[] | select(.key | test("^[A-Za-z_][A-Za-z0-9_]*$")) | "export " + .key + "=" + (.value|tostring|@sh)')"
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
