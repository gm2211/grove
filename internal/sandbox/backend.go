package sandbox

import (
	"context"
	"io"
	"time"
)

// backend runs one job's steps inside its VM. Runner's job does everything that is the same for
// every guest (packing the bundle, supervising, enforcing the timeout, mapping exit codes,
// uploading artifacts); a backend only says how each step is done there: in a docker container in
// a Linux VM (dockerBackend), or as a plain process tree of the job user in a macOS VM
// (macBackend). Every step is one shell command line run through Orchard's exec, opened from the
// control plane.
type backend interface {
	// paths is where run.sh finds the checkout and grove's files in the VM.
	paths() jobPaths
	// place names where the job runs, for messages ("its container").
	place() string

	// create prepares the job's place in the VM. It returns 0, exitDraining when the VM is
	// shutting down (and was left untouched), or the code Run should fail with.
	create(ctx context.Context, j *job) int
	// push reads the bundle tar (see writeBundle) from stdin and unpacks it into the job's place.
	push(j *job) step
	// start starts run.sh and returns as soon as it runs.
	start(j *job) step
	// newLogs makes the job's resumable log follower.
	newLogs(j *job) jobLogs
	// inspect prints "<state> <exit code>": state "running", or "exited"/"dead" once it is over.
	// It fails (non-zero) when the job is gone.
	inspect(j *job) step
	// stop stops the job, killing it after grace.
	stop(j *job, grace time.Duration) step
	// artifacts writes a tar of the workspace's artifacts directory to stdout, entries named
	// "artifacts/…".
	artifacts(j *job) step
	// noArtifacts reports whether a failed artifacts step only means there was no directory.
	noArtifacts(stderr string) bool
	// remove deletes everything the job left in the VM. It runs whenever create got past the
	// draining check, and must not fail the job.
	remove(j *job)
}

// step is one command line a backend runs in the VM, and the name its failures go by.
type step struct {
	cmd  string
	tool string
}

// jobLogs is one job's output streams as a backend fetches them. command(true) follows them
// until the job ends (or the exchange drops), command(false) fetches what's left once it has;
// both resume from what was already delivered, so a reattach neither repeats nor loses output.
type jobLogs interface {
	streams() (stdout, stderr io.Writer)
	command(follow bool) string
	// reset ends an attach that may have been cut off.
	reset()
	// flush writes out a final line that had no newline, once the job is over.
	flush()
}
