// Package sandbox runs grove jobs inside sealed VMs: VMs of a fleet.yaml pool with
// `network: isolated`, which can reach the public internet and nothing at home (not the host Mac,
// the LAN, the tailnet, Nomad, Orchard or the artifact store).
//
// Because such a VM can't reach home, it runs no Nomad client and holds no credential that works
// against home. Instead every exchange is opened from home: a `grove sandbox run` process (one per
// job, placed by Nomad on the control plane's own client, see nomad/jobs/sandbox.nomad.hcl) fetches
// the repository on the control plane, picks a sealed VM with a free slot, pushes the repository
// and script into a fresh container there through Orchard's exec channel, starts it (the container
// checks out the job's ref itself), follows its logs, polls it for liveness, and pulls its
// artifacts and exit status back out.
//
// The clone token and the artifact store's keys stay on the control plane; the job's container
// sees the checkout, its script and the caller's environment (minus GH_TOKEN/GIT_TOKEN, which only
// ever served the clone).
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
	// ExitTimedOut matches the regular job templates: internal/dispatch maps 124 to Job.TimedOut.
	ExitTimedOut = 124
	// ExitLost means the job's sealed VM stopped answering (deleted, recycled under it, or its
	// worker went away); internal/dispatch reports it as StatusLost for sealed pools. A job script
	// that itself exits 125 is reported as 1, so the code stays unambiguous.
	ExitLost = 125
	// ExitSetup means grove couldn't start the job: a bad request, or the container couldn't be
	// created (an image that doesn't exist, say).
	ExitSetup = 1
)

// DefaultTimeout applies when a job sets none, matching the regular templates'
// `${NOMAD_META_timeout_seconds:-3600}`.
const DefaultTimeout = time.Hour

// Request is one job for Runner.Run, assembled by `grove sandbox run` from the Nomad dispatch.
type Request struct {
	Kind    string
	Pool    string
	AllocID string
	// Image is the container image the job runs in (the pool's runner image).
	Image string
	// CPU (MHz) and Memory (MiB) size the job's container, like the regular templates' resources
	// block. Zero leaves it unlimited.
	CPU    int
	Memory int

	Repo string
	Ref  string
	// CloneToken is the control plane's armed private-repo credential, from the dispatch payload.
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

// containerName is unique per Nomad allocation, so a job never collides with another one in the
// same VM.
func (r Request) containerName() string { return "grove-" + r.AllocID }

// cloneCredentialEnv lists the env variables grove treats as clone credentials (see the regular
// templates' source task). In a sealed pool they serve the control plane's clone and never enter
// the VM.
var cloneCredentialEnv = []string{"GH_TOKEN", "GIT_TOKEN"}

// ParsePayload splits a Nomad dispatch payload into the clone token and the caller's script. A
// build or agent payload is "grove-payload/1", the token (or an empty line), then the script; a
// shell job's payload is the script itself (see internal/dispatch encodePayload).
func ParsePayload(kind string, payload []byte) (token, script string) {
	const header = "grove-payload/1\n"
	s := string(payload)
	if kind == KindShell || !strings.HasPrefix(s, header) {
		return "", s
	}
	rest := strings.TrimPrefix(s, header)
	token, script, _ = strings.Cut(rest, "\n")
	return strings.TrimSuffix(token, "\r"), script
}

// shQuote wraps s in single quotes for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
