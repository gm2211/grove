package install

import (
	"context"
	"fmt"
	"io"
)

// resolveWorkerIdentity settles the name and labels this install renders (see workeridentity.go)
// and refuses a rename it cannot complete. On the control plane host a rename is finished by the
// worker-rename step; anywhere else the admin must make the fleet change, so without --force the
// install stops and prints it rather than leave the pools orphaned.
func resolveWorkerIdentity(opts Options, out io.Writer) (Options, error) {
	prev := detectWorkerIdentity(opts)
	id := chooseWorkerIdentity(opts, prev)

	changed := prev.Name != "" && (prev.Name != id.Name || (prev.Labels != "" && prev.Labels != id.Labels))
	if changed {
		note := renameFleetNote(prev, id)
		if prev.Name != id.Name && opts.Token == "" {
			return opts, fmt.Errorf("renaming the worker %q to %q needs a bootstrap credential minted for the new name: pass --token\n%s", prev.Name, id.Name, note)
		}
		if _, onControlPlane := controlPlaneConfig(opts); onControlPlane {
			p := pendingIdentity{Name: prev.Name, Labels: prev.Labels}
			dup := false
			for _, q := range id.Pending {
				dup = dup || q == p
			}
			if !dup {
				id.Pending = append(append([]pendingIdentity{}, id.Pending...), p)
			}
		} else if !opts.Force {
			return opts, fmt.Errorf("this Mac does not host the control plane, so it cannot update fleet.yaml or delete the old Orchard worker record; refusing to orphan the pools.\n%s"+
				"Make that change on the control plane host, then re-run with --force.", note)
		} else {
			fmt.Fprintf(out, "        WARNING: --force: renaming without updating the control plane. Make this change there:\n%s", note)
		}
	}

	opts.Hostname, opts.Labels = id.Name, id.Labels
	opts.identity = id
	if opts.Controller == "" {
		opts.Controller = prev.Controller
	}
	return opts, nil
}

// workerIdentityStep records the identity so later runs keep it, whatever the host's name becomes.
func workerIdentityStep(opts Options) Step {
	id := opts.identity
	want := renderWorkerIdentityRecord(id)
	return Step{
		Name: "worker-identity",
		Description: fmt.Sprintf("Record this worker's identity (name %q, labels %q; from %s) in %s so later installs keep it.",
			id.Name, id.Labels, id.Source, workerIdentityPath(opts)),
		Check: fileHasContent(workerIdentityPath(opts), func() (string, error) { return want, nil }),
		Apply: func(context.Context) error { return writeFile(workerIdentityPath(opts), want) },
	}
}

// workerFinishSteps are the steps that can only complete once the new worker LaunchDaemon is
// loaded, which for an install that needs sudo is after the root script: retiring what the new shape
// supersedes, and finishing a rename.
func workerFinishSteps(r Runner, opts Options, ds []daemon, bk *backupSession, out io.Writer) []Step {
	steps := []Step{retireLaunchAgentsStep(r, opts, ds, bk, out)}
	if len(opts.identity.Pending) > 0 {
		steps = append(steps, renameWorkerStep(r, opts, opts.identity, ds, bk, out))
	}
	return steps
}

// FinishWorkerInstall runs the steps of a worker install that wait for its LaunchDaemons: it is
// called after the root script ran. A failure is returned; what it could not do is retried by the
// next `grove install --role worker`.
func FinishWorkerInstall(ctx context.Context, r Runner, opts Options, out io.Writer) error {
	opts, err := resolveWorkerIdentity(opts, out)
	if err != nil {
		return err
	}
	steps := workerFinishSteps(r, opts, workerDaemons(opts), newBackupSession(opts, out), out)
	_, err = RunPlan(ctx, steps, PlanOptions{Out: out})
	return err
}
