package fleet

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/gm2211/grove/internal/orchard"
)

// WorkerCapabilityGuestPasswordRotation mirrors the fork's v1.WorkerCapabilityGuestPasswordRotation:
// the worker logs in to a VM with the VM's own password and falls back to the image's default one,
// so a startup script may replace the image password. A worker without it would lock itself out of
// the VM's shutdown script (and of the startup script itself after a restart).
const WorkerCapabilityGuestPasswordRotation = "guest-password-rotation"

// imageGuestUser and imageGuestPassword are what Tart's macOS images boot with.
const (
	imageGuestUser     = "admin"
	imageGuestPassword = "admin"
)

// guestPasswordPlaceholder is filled with a fresh random password right before CreateVM (see
// withGuestPassword), the same value going into the VM spec's Password so Orchard can still log in.
// A planned spec never holds one.
const guestPasswordPlaceholder = "@GROVE_GUEST_PASSWORD@"

// guestPasswordScript replaces the image's admin password with the VM's own. admin is a
// passwordless sudoer, so a job that knows the password (su, ssh to localhost, Screen Sharing, an
// osascript administrator prompt) is root, past the pf fence that only matches the job users. The
// password reaches dscl on stdin, never argv: sudo logs argv, and ps shows it to every user. A rerun
// after a restart finds the image password already gone and leaves the account alone. It runs
// first, before Nomad can register, and fails the script if the image password still works.
//
// Auto-login keeps the image's /etc/kcpassword, which no longer matches: a VM that reboots comes
// up at the login window. Nothing grove runs needs a GUI session; jobs run as MacOSJobUser.
const guestPasswordScript = `# grove:guest-password begin
` + grovePrivFunc + `grove_guest_user=$(id -un)
if dscl . -authonly "$grove_guest_user" ` + imageGuestPassword + ` >/dev/null 2>&1; then
  grove_guest_password=` + guestPasswordPlaceholder + `
  printf 'passwd /Users/%s %s\n' "$grove_guest_user" "$grove_guest_password" | grove_priv dscl . >/dev/null
  unset grove_guest_password
  if dscl . -authonly "$grove_guest_user" ` + imageGuestPassword + ` >/dev/null 2>&1; then
    echo "grove: $grove_guest_user still accepts the image password; refusing to take jobs" >&2
    exit 1
  fi
fi
# grove:guest-password end
`

// guestPasswordBlockRE matches guestPasswordScript, filled or not, so specDrifted ignores the
// per-VM password but still notices a VM without the block.
var guestPasswordBlockRE = regexp.MustCompile(`(?s)# grove:guest-password begin\n.*?# grove:guest-password end\n`)

// needsGuestPasswordRotation says whether pool's VMs must get their own admin password: macOS
// pools still on the image password. Linux jobs run in containers that the egress fence keeps off
// the VM's own sshd; a pool with its own password set in fleet.yaml has already replaced the
// image's.
func needsGuestPasswordRotation(pool Pool) bool {
	return pool.MacOS() && (pool.Password == "" || pool.Password == imageGuestPassword)
}

// canRotateGuestPassword says whether worker's Orchard still gets into a VM after the swap.
func canRotateGuestPassword(worker orchard.Worker) bool {
	return slices.Contains(worker.Capabilities, WorkerCapabilityGuestPasswordRotation)
}

// guestPasswordBlock is why a pool needing its own admin password gets no VM on worker, or "" when
// it may have one. A VM left on admin/admin hands every job root, so there is no degraded mode.
func guestPasswordBlock(pool Pool, worker orchard.Worker) string {
	if !needsGuestPasswordRotation(pool) || canRotateGuestPassword(worker) {
		return ""
	}
	return fmt.Sprintf("worker %s's Orchard can't replace the image's admin/admin (no %s capability), "+
		"which would give every job root; upgrade it (docs/OPERATIONS.md \"The macOS guest's admin password\")",
		worker.Name, WorkerCapabilityGuestPasswordRotation)
}

// withGuestPassword fills a planned spec's guest password placeholder with a fresh random password
// and hands Orchard the same one. A spec without the block is returned unchanged.
func withGuestPassword(spec orchard.VMSpec) (orchard.VMSpec, error) {
	if !strings.Contains(spec.StartupScript, guestPasswordPlaceholder) {
		return spec, nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return spec, fmt.Errorf("generate guest password: %w", err)
	}
	password := hex.EncodeToString(raw)
	spec.StartupScript = strings.ReplaceAll(spec.StartupScript, guestPasswordPlaceholder, shQuote(password))
	if spec.Username == "" {
		spec.Username = imageGuestUser
	}
	spec.Password = password
	return spec, nil
}
