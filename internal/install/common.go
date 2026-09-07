package install

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// BuildPlan assembles the ordered step list for opts.Role. It fails fast (before returning any
// steps) if only the Mac App Store build of Tailscale is present, since nothing else in the plan
// can succeed without a tailnet identity that comes up unattended.
func BuildPlan(r Runner, opts Options, out io.Writer) ([]Step, error) {
	if opts.goos() == "darwin" {
		if _, err := FindTailscale(opts.lookPath(), opts.exists()); errors.Is(err, ErrAppStoreTailscale) {
			return nil, err
		}
	}

	steps := buildCommonSteps(r, opts, out)

	switch opts.Role {
	case RoleWorker:
		steps = append(steps, buildWorkerSteps(r, opts, out)...)
	case RoleControlPlane:
		steps = append(steps, buildControlPlaneSteps(r, opts, out)...)
	case RoleClient:
		steps = append(steps, buildClientSteps(r, opts, out)...)
	default:
		return nil, fmt.Errorf("unknown role %q: want %q, %q or %q", opts.Role, RoleWorker, RoleControlPlane, RoleClient)
	}
	return steps, nil
}

// buildCommonSteps are the checks every role needs regardless of what it runs.
func buildCommonSteps(r Runner, opts Options, out io.Writer) []Step {
	lookPath := opts.lookPath()
	exists := opts.exists()
	return []Step{
		{
			Name:        "tailscale",
			Description: "Verify Tailscale (standalone variant) is installed and the tailnet is up. If not, run `tailscale up` interactively — grove can't authenticate a tailnet for you.",
			Check: func(ctx context.Context) (bool, error) {
				bin, err := FindTailscale(lookPath, exists)
				if err != nil {
					return false, err
				}
				st, err := GetTailscaleStatus(ctx, r, bin)
				if err != nil {
					return false, err
				}
				if st.BackendState != "Running" {
					return false, nil
				}
				fmt.Fprintf(out, "        tailnet IP: %s   MagicDNS: %s\n", st.TailnetIP(), st.Self.DNSName)
				return true, nil
			},
			Apply: func(ctx context.Context) error {
				return errors.New("tailscale is installed but not up; run `tailscale up` (needs interactive auth) then re-run grove install")
			},
		},
	}
}
