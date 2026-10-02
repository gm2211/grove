#!/bin/bash
# Build-time acceptance: a healthy process and a completed allocation, not launchd presence.
set -euo pipefail
export PATH=/usr/local/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/bin:/bin:/usr/sbin:/sbin
export NOMAD_ADDR=http://127.0.0.1:14646
unset NOMAD_TOKEN NOMAD_NAMESPACE NOMAD_REGION
work=$(mktemp -d /tmp/grove-nomad-acceptance.XXXXXX)
agent_pid=''
cleanup() {
  if [ -f "$work/agent.pid" ]; then
    agent_pid=$(cat "$work/agent.pid")
    case "$agent_pid" in ''|*[!0-9]*) ;; *) sudo -n kill "$agent_pid" 2>/dev/null || true ;; esac
  fi
  [ -z "${launcher_pid:-}" ] || wait "$launcher_pid" 2>/dev/null || true
  sudo -n rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cat > "$work/config.hcl" <<HCL
ports {
  http = 14646
  rpc = 14647
  serf = 14648
}
client {
  cpu_total_compute = $(( $(sysctl -n hw.ncpu) * 2000 ))
}
plugin "raw_exec" {
  config {
    enabled = true
  }
}
HCL
# Log stays owned by the unprivileged builder; only the agent needs root.
# shellcheck disable=SC2024
sudo -n /bin/sh -c 'echo $$ > "$1"; shift; exec "$@"' sh "$work/agent.pid" \
  "$(command -v nomad)" agent -dev -bind=127.0.0.1 -config="$work/config.hcl" \
  -data-dir="$work/data" > "$work/agent.log" 2>&1 &
launcher_pid=$!
ready=false
for _ in {1..60}; do
  if curl -fsS "$NOMAD_ADDR/v1/nodes" 2>/dev/null | jq -e 'any(.[]; .Status == "ready")' >/dev/null; then
    ready=true
    break
  fi
  sleep 1
done
if [ "$ready" != true ]; then
  echo 'FATAL: Nomad failed worker readiness during image build.' >&2
  tail -n 50 "$work/agent.log" >&2
  exit 1
fi
cat > "$work/job.nomad.hcl" <<'HCL'
job "grove-image-acceptance" {
  datacenters = ["dc1"]
  type = "batch"
  group "check" {
    restart {
      attempts = 0
      mode = "fail"
    }
    task "check" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args = ["-c", "echo GROVE_IMAGE_NOMAD_OK"]
      }
      resources {
        cpu = 100
        memory = 32
      }
    }
  }
}
HCL
nomad job run -detach "$work/job.nomad.hcl" >/dev/null
passed=false
for _ in {1..60}; do
  alloc=$(curl -fsS "$NOMAD_ADDR/v1/job/grove-image-acceptance/allocations" | jq -r '.[0].ID // empty')
  if [ -n "$alloc" ]; then
    if curl -fsS "$NOMAD_ADDR/v1/allocation/$alloc" | jq -e '.ClientStatus == "complete" and any(.TaskStates.check.Events[]; .Type == "Terminated" and .ExitCode == 0)' >/dev/null; then
      nomad alloc logs "$alloc" check | grep -qx GROVE_IMAGE_NOMAD_OK
      passed=true
      break
    fi
  fi
  sleep 1
done
if [ "$passed" != true ]; then
  echo 'FATAL: Nomad failed raw_exec allocation during image build.' >&2
  tail -n 50 "$work/agent.log" >&2
  exit 1
fi
nomad job stop -purge grove-image-acceptance >/dev/null
echo GROVE_IMAGE_NOMAD_ACCEPTANCE_PASSED
