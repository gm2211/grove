package fleet

import (
	"fmt"
	"strings"
	"time"
)

// defaultShutdownTimeout is used when a Pool doesn't set ShutdownTimeout.
const defaultShutdownTimeout = 2*time.Hour + 30*time.Minute

// nomadDrainDeadline bounds how long Nomad itself waits for allocations to finish once told to
// drain, independent of grove's own poll loop below (which is bounded by ShutdownTimeout).
const nomadDrainDeadline = "2h"

// BuildStartupScript renders the guest StartupScript for a VM: it records which pool/worker/VM
// it belongs to (so the Nomad client inside can advertise it as node meta), optionally joins the
// tailnet, and restarts the Nomad client so the new meta takes effect. pool.StartupScript, if
// set, is appended verbatim after the generated part (see ARCHITECTURE.md → "Recycling").
func BuildStartupScript(pool Pool, worker, vmName, tailscaleAuthKey string) string {
	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eu\n\n")
	fmt.Fprintf(&b, "grove_pool=%s\n", shQuote(pool.Name))
	fmt.Fprintf(&b, "grove_host=%s\n", shQuote(worker))
	fmt.Fprintf(&b, "grove_vm=%s\n\n", shQuote(vmName))

	b.WriteString(nomadMetaScript)
	b.WriteString("\n")

	if tailscaleAuthKey != "" {
		fmt.Fprintf(&b, "tailscale up --authkey=%s --hostname=\"$grove_vm\" --ssh --accept-routes\n\n",
			shQuote(tailscaleAuthKey))
	}

	b.WriteString(nomadRestartScript)

	appendUserScript(&b, "pool.startupScript", pool.StartupScript)

	return b.String()
}

// BuildShutdownScript renders the default guest ShutdownScript: drain the local Nomad client,
// then wait (bounded by the returned timeout, defaulting to defaultShutdownTimeout) for zero
// non-terminal allocations before Orchard deletes the VM. pool.ShutdownScript, if set, is
// appended verbatim after the generated part.
func BuildShutdownScript(pool Pool) (script string, timeout time.Duration) {
	timeout = pool.ShutdownTimeout.Std()
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}

	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eu\n\n")
	fmt.Fprintf(&b, "nomad node drain -self -enable -deadline %s -m %s\n\n",
		nomadDrainDeadline, shQuote("grove recycle"))
	fmt.Fprintf(&b, "grove_deadline=$(( $(date +%%s) + %d ))\n", int(timeout.Seconds()))
	b.WriteString(pollAllocsScript)

	appendUserScript(&b, "pool.shutdownScript", pool.ShutdownScript)

	return b.String(), timeout
}

func appendUserScript(b *strings.Builder, label, script string) {
	if strings.TrimSpace(script) == "" {
		return
	}

	fmt.Fprintf(b, "\n# --- %s ---\n", label)
	b.WriteString(script)

	if !strings.HasSuffix(script, "\n") {
		b.WriteString("\n")
	}
}

// shQuote wraps s in single quotes for safe interpolation into a POSIX shell script.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// nomadMetaScript writes the Nomad client meta file naming this VM's pool/host/vm, at the path
// appropriate for the guest OS.
const nomadMetaScript = `if [ "$(uname -s)" = "Darwin" ]; then
  grove_meta_file="/usr/local/etc/nomad.d/grove-meta.hcl"
else
  grove_meta_file="/etc/nomad.d/grove-meta.hcl"
fi

mkdir -p "$(dirname "$grove_meta_file")"
cat > "$grove_meta_file" <<GROVE_META
client {
  meta {
    pool = "$grove_pool"
    host = "$grove_host"
    vm = "$grove_vm"
  }
}
GROVE_META
`

// nomadRestartScript restarts the Nomad client so a freshly written meta file takes effect.
const nomadRestartScript = `if [ "$(uname -s)" = "Darwin" ]; then
  sudo launchctl kickstart -k system/com.grove.nomad
else
  systemctl restart nomad
fi
`

// pollAllocsScript polls "nomad node status -self -json" until no allocation on this node is
// still pending/running, or until grove_deadline (set by the caller) passes. It prefers jq for
// robust JSON parsing but falls back to a grep-based count so it still degrades gracefully on
// images that don't ship jq.
const pollAllocsScript = `while :; do
  grove_status_json=$(nomad node status -self -json 2>/dev/null || echo '{}')
  if command -v jq >/dev/null 2>&1; then
    grove_remaining=$(printf '%s' "$grove_status_json" | \
      jq '[(.Allocations // [])[] | select(.ClientStatus=="pending" or .ClientStatus=="running")] | length' \
      2>/dev/null || echo "")
  else
    grove_remaining=$(printf '%s' "$grove_status_json" | \
      grep -o '"ClientStatus":"\(pending\|running\)"' | wc -l | tr -d ' ')
  fi
  [ -z "$grove_remaining" ] && grove_remaining=0

  if [ "$grove_remaining" -eq 0 ] 2>/dev/null; then
    break
  fi

  if [ "$(date +%s)" -ge "$grove_deadline" ]; then
    echo "grove: shutdown timeout reached with $grove_remaining allocation(s) still non-terminal" >&2
    break
  fi

  sleep 5
done
`
