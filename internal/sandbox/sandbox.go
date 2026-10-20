// Package sandbox runs grove jobs inside sealed VMs: VMs of a fleet.yaml pool with
// `network: isolated` (every pool is), which can reach the public internet and nothing at home (not
// the host Mac, the LAN, the tailnet, Orchard or the artifact store).
//
// Because such a VM can't reach home, it runs no agent of grove's and holds no credential that
// works against home. Instead every exchange is opened from home: `grove serve`'s dispatch service
// runs a Runner per job (internal/dispatch SandboxExecutor), which fetches the repository on the
// control plane, picks a sealed VM with a free slot, pushes the repository
// and script into a fresh container there through Orchard's exec channel, starts it (the container
// checks out the job's ref itself), follows its logs, polls it for liveness, and pulls its
// artifacts and exit status back out.
//
// A macOS guest has no container runtime, so a sealed macOS pool's job runs as a plain process
// tree of the VM's unprivileged job user (fleet.MacOSJobUser) in a fresh run directory instead
// (see macos.go), and every such VM is used for one job only: the runner deletes it through
// Orchard once the job is over, and the fleet reconciler recreates it from the image.
//
// The clone token and the artifact store stay on the control plane; the job sees the
// checkout, its script and the caller's environment (minus GH_TOKEN/GIT_TOKEN, which only ever
// served the clone).
package sandbox

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Kinds, matching dispatch.Kind.
const (
	KindBuild = "build"
	KindAgent = "agent"
	KindShell = "shell"
)

// Exit codes Run returns besides the job's own.
const (
	// ExitTimedOut is timeout(1)'s code: internal/dispatch maps 124 to Job.TimedOut.
	ExitTimedOut = 124
	// ExitLost means the job's sealed VM stopped answering (deleted, recycled under it, or its
	// worker went away); internal/dispatch reports it as StatusLost. A job script
	// that itself exits 125 is reported as 1, so the code stays unambiguous.
	ExitLost = 125
	// ExitSetup means grove couldn't start the job: a bad request, or the container couldn't be
	// created (an image that doesn't exist, say).
	ExitSetup = 1
)

// Guest operating systems a sealed pool's VMs run (Request.OS): Runner drives a Linux VM's jobs in
// docker containers and a macOS VM's as plain processes of its unprivileged job user.
const (
	OSLinux  = "linux"
	OSDarwin = "darwin"
)

// DefaultTimeout applies when a job sets none.
const DefaultTimeout = time.Hour

// Request is one job for Runner.Run, assembled by internal/dispatch's SandboxExecutor.
type Request struct {
	Kind string
	Pool string
	// AllocID is the job's id: it names the job's container (or run directory) in the VM.
	AllocID string
	// OS is the pool's guest OS: OSLinux (or "") or OSDarwin.
	OS string
	// Image is the container image the job runs in (the pool's runner image). A macOS job runs in
	// no container; its Image is only validated.
	Image string
	// CPU (MHz, 2000 per core) and Memory (MiB) size the job's container. Zero leaves it
	// unlimited. A macOS job has its VM to itself and ignores them.
	CPU    int
	Memory int

	Repo string
	Ref  string
	// CloneToken is the control plane's armed private-repo credential (see internal/gitauth). It
	// is used only for the clone on the control plane and never enters the VM.
	CloneToken string
	Env        map[string]string
	Script     string
	Timeout    time.Duration
	// ArtifactPrefix is where a build job's ./artifacts/** land in the artifact store.
	ArtifactPrefix string
}

var (
	allocIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	poolRE    = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	imageRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-/:@+]{0,511}$`)
	envKeyRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// validate checks everything Run interpolates into commands it runs inside the VM.
func (r Request) validate() error {
	switch r.Kind {
	case KindBuild, KindAgent, KindShell:
	default:
		return fmt.Errorf("unknown job kind %q", r.Kind)
	}
	switch r.OS {
	case "", OSLinux, OSDarwin:
	default:
		return fmt.Errorf("unknown guest OS %q", r.OS)
	}
	if !poolRE.MatchString(r.Pool) {
		return fmt.Errorf("invalid pool name %q", r.Pool)
	}
	if !allocIDRE.MatchString(r.AllocID) {
		return fmt.Errorf("invalid allocation id %q", r.AllocID)
	}
	if !imageRE.MatchString(r.Image) {
		return fmt.Errorf("invalid image %q", r.Image)
	}
	if r.CPU < 0 || r.Memory < 0 {
		return fmt.Errorf("negative resources")
	}
	return nil
}

func (r Request) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultTimeout
}

// containerName is unique per job, so a job never collides with another one in the same VM.
func (r Request) containerName() string { return "grove-" + r.AllocID }

// cloneCredentialEnv lists the env variables grove treats as clone credentials. They serve the
// control plane's clone and never enter the VM.
var cloneCredentialEnv = []string{"GH_TOKEN", "GIT_TOKEN"}

// shQuote wraps s in single quotes for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
