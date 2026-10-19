package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/fleet"
	"github.com/gm2211/grove/internal/orchard"
)

// ExitCanceled is what Run returns when its context ends (Nomad stopping the job): 128+SIGTERM.
const ExitCanceled = 143

// drainingMarker is the file a sealed Linux VM's shutdown script creates (internal/fleet) to stop
// new jobs landing in it (fleet.SealedDrainingMarkerMacOS in a macOS VM); drainingExit is how the
// create command reports it, and exitDraining is how job.run hands that back to Run, which then
// picks another VM. Run never returns exitDraining.
const (
	drainingMarker = fleet.SealedDrainingMarker
	drainingExit   = 75
	exitDraining   = -75
)

// Defaults for Runner's tunables.
const (
	defaultPollInterval  = 15 * time.Second
	defaultMissedPolls   = 4
	defaultSlotWait      = 5 * time.Second
	defaultNoVMTimeout   = 30 * time.Minute
	defaultStopGrace     = 10 * time.Second
	defaultArtifactLimit = 2 << 30 // 2 GiB per job

	defaultTimeoutSlack = time.Minute
	// createTimeout bounds `docker create`, which pulls the image on first use.
	createTimeout = 30 * time.Minute
	// commandTimeout bounds the short commands (docker inspect, start, rm and their macOS
	// counterparts).
	commandTimeout = time.Minute
	// vmWait is how long an exec waits for the VM's agent to come up.
	vmWait = 30 * time.Second
)

// Runner drives jobs in a pool's sealed VMs from the control plane. See the package comment.
type Runner struct {
	Orchard orchard.Client
	// Uploader receives a build job's artifacts. Nil skips collecting them.
	Uploader artifacts.Uploader
	Slots    Slots
	// Stdout and Stderr receive the job's own output; grove's messages ("grove: …") go to Stderr.
	Stdout io.Writer
	Stderr io.Writer
	// WorkDir is where checkouts are made before being pushed in; "" uses the system default.
	WorkDir string
	// Git is the git binary for checkouts; "" finds one.
	Git string

	// PollInterval is how often Runner checks that the job's container is still there and
	// running; after MissedPolls failed checks in a row the VM is considered lost.
	PollInterval time.Duration
	MissedPolls  int
	// SlotWait is how often Runner looks again for a free slot; NoVMTimeout is how long it waits
	// when the pool has no sealed VM running at all.
	SlotWait    time.Duration
	NoVMTimeout time.Duration
	// StopGrace is how long stopping the job (`docker stop`, or TERM to a macOS job's process
	// group) lets it shut down before killing it.
	StopGrace time.Duration
	// TimeoutSlack is how long past the job's own timeout Runner waits before stopping the job
	// itself. A container's timeout(1) normally fires first; a macOS guest has none.
	TimeoutSlack time.Duration
	// ArtifactLimit caps the bytes of artifacts collected from one job.
	ArtifactLimit int64

	stderrMu sync.Mutex
}

func (r *Runner) defaults() {
	if r.Stdout == nil {
		r.Stdout = os.Stdout
	}
	if r.Stderr == nil {
		r.Stderr = os.Stderr
	}
	if r.Git == "" {
		r.Git = gitBinary()
	}
	if r.PollInterval <= 0 {
		r.PollInterval = defaultPollInterval
	}
	if r.MissedPolls <= 0 {
		r.MissedPolls = defaultMissedPolls
	}
	if r.SlotWait <= 0 {
		r.SlotWait = defaultSlotWait
	}
	if r.NoVMTimeout <= 0 {
		r.NoVMTimeout = defaultNoVMTimeout
	}
	if r.StopGrace <= 0 {
		r.StopGrace = defaultStopGrace
	}
	if r.TimeoutSlack <= 0 {
		r.TimeoutSlack = defaultTimeoutSlack
	}
	if r.ArtifactLimit <= 0 {
		r.ArtifactLimit = defaultArtifactLimit
	}
}

// stderr serializes writes from the log follower and from say.
func (r *Runner) stderr() io.Writer { return lockedWriter{mu: &r.stderrMu, w: r.Stderr} }

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (r *Runner) say(format string, args ...any) {
	fmt.Fprintf(r.stderr(), "grove: "+format+"\n", args...)
}

// Run runs req in a sealed VM of req.Pool and returns the exit code the job's Nomad task should
// exit with: the job's own, or ExitTimedOut, ExitLost, ExitSetup or ExitCanceled.
func (r *Runner) Run(ctx context.Context, req Request) int {
	r.defaults()
	if err := req.validate(); err != nil {
		r.say("refusing the job: %v", err)
		return ExitSetup
	}

	checkout := ""
	if req.Repo != "" && req.Kind != KindShell {
		dir, err := os.MkdirTemp(r.WorkDir, "grove-sandbox-")
		if err != nil {
			r.say("could not make a directory for the checkout: %v", err)
			return ExitSetup
		}
		defer os.RemoveAll(dir)
		checkout = filepath.Join(dir, "work")
		if code := Clone(ctx, r.Git, req.Repo, req.Ref, cloneToken(req), checkout, r.stderr()); code != 0 {
			if ctx.Err() != nil {
				return ExitCanceled
			}
			return code
		}
	}

	// VMs found shutting down, by name and UID: the reconciler recreates a VM under the same
	// name, and that new VM is fine to use.
	draining := map[string]bool{}
	for {
		slot, uid, code := r.acquire(ctx, req.Pool, draining)
		if slot == nil {
			return code
		}
		code = r.newJob(req, slot.VM).run(ctx, checkout)
		slot.Release()
		if code != exitDraining {
			return code
		}
		r.say("%s is shutting down; picking another VM", slot.VM)
		draining[slot.VM+"/"+uid] = true
	}
}

// acquire waits for a free slot in one of pool's sealed, running VMs. A VM that isn't sealed
// (Softnet with the host blocked) never gets a sealed pool's job, even while fleet.yaml says the
// pool is isolated and the reconciler hasn't yet replaced it.
func (r *Runner) acquire(ctx context.Context, pool string, exclude map[string]bool) (slot *Slot, uid string, code int) {
	start := time.Now()
	said := ""
	for {
		var candidates []string
		uids := map[string]string{}
		vms, err := r.Orchard.ListVMs(ctx)
		msg := ""
		if err != nil {
			msg = fmt.Sprintf("could not list VMs (%v); retrying", err)
		} else {
			for _, vm := range vms {
				if p, _, _, ok := fleet.ParseVMName(vm.Name); ok && p == pool && vm.Status == "running" && fleet.IsolatedVM(vm) && !exclude[vm.Name+"/"+vm.UID] {
					candidates = append(candidates, vm.Name)
					uids[vm.Name] = vm.UID
				}
			}
			slot, serr := r.Slots.TryAcquire(candidates)
			if serr != nil {
				r.say("could not take a VM slot: %v", serr)
				return nil, "", ExitSetup
			}
			if slot != nil {
				return slot, uids[slot.VM], 0
			}
			if len(candidates) == 0 {
				msg = fmt.Sprintf("no sealed VM of pool %s is running yet; waiting", pool)
			} else {
				msg = fmt.Sprintf("all %d sealed VMs of pool %s are busy; waiting for a free slot", len(candidates), pool)
			}
		}
		if msg != said {
			r.say("%s", msg)
			said = msg
		}
		if len(candidates) == 0 && time.Since(start) > r.NoVMTimeout {
			r.say("no sealed VM of pool %s came up within %s", pool, r.NoVMTimeout)
			return nil, "", ExitLost
		}
		select {
		case <-ctx.Done():
			r.say("canceled while waiting for a sealed VM")
			return nil, "", ExitCanceled
		case <-time.After(r.SlotWait):
		}
	}
}

// job is one run of a Request in a chosen VM.
type job struct {
	r    *Runner
	req  Request
	vm   string
	name string
	// b runs the job's steps in the VM: docker in a Linux VM, plain processes in a macOS one.
	b backend

	logs jobLogs
}

func (r *Runner) newJob(req Request, vm string) *job {
	j := &job{r: r, req: req, vm: vm, name: req.containerName()}
	if req.OS == OSDarwin {
		j.b = macBackend{}
	} else {
		j.b = dockerBackend{}
	}
	return j
}

// errLost marks a failure to talk to the VM at all, as opposed to a command that ran and failed.
var errLost = errors.New("lost contact with the VM")

// exec runs one shell command line in the VM. It returns the command's exit code, or an error
// wrapping errLost when the exchange itself failed.
func (j *job) exec(ctx context.Context, timeout time.Duration, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	session, err := j.r.Orchard.Exec(ctx, j.vm, []string{cmd}, orchard.ExecOptions{Stdin: stdin, Stderr: stderr, Wait: vmWait})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errLost, err)
	}
	defer session.Close()
	if _, err := io.Copy(stdout, session.Output()); err != nil {
		// Closing the session unblocks its reader, which would otherwise wait on output no one
		// reads any more.
		session.Close()
		return 0, fmt.Errorf("%w: %v", errLost, err)
	}
	code, err := session.Wait(ctx)
	if err != nil {
		return code, fmt.Errorf("%w: %v", errLost, err)
	}
	return code, nil
}

func (j *job) run(ctx context.Context, checkout string) int {
	// A step that failed because Nomad is stopping the job reports that, not the failure.
	failed := func(code int) int {
		if ctx.Err() != nil {
			return ExitCanceled
		}
		return code
	}
	code := j.b.create(ctx, j)
	// A VM that refused the job because it is shutting down was left untouched.
	if code != exitDraining {
		defer j.b.remove(j)
	}
	if code != 0 {
		return failed(code)
	}
	j.r.say("running in sealed VM %s (pool %s)", j.vm, j.req.Pool)
	if code := j.push(ctx, checkout); code != 0 {
		return failed(code)
	}
	if code := j.start(ctx); code != 0 {
		return failed(code)
	}
	code = j.supervise(ctx)
	if code != ExitLost && ctx.Err() == nil && j.req.Kind == KindBuild {
		j.collectArtifacts()
	}
	return code
}

// push copies the checkout and the job's files into the job's place in the VM.
func (j *job) push(ctx context.Context, checkout string) int {
	env, dropped := envScript(j.req.Env)
	if len(dropped) > 0 {
		j.r.say("%s served only the checkout and are not passed into the sealed VM", strings.Join(dropped, " and "))
	}
	files := map[string]string{
		"run.sh":    runScriptAt(j.b.paths(), j.req.timeout(), j.req.Ref, checkout != ""),
		"script.sh": j.req.Script,
		"env.sh":    env,
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	bundled := make(chan error, 1)
	go func() {
		err := writeBundle(pw, checkout, files)
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			// The VM is waiting for the rest of the input; end the exchange instead.
			cancel()
		}
		pw.CloseWithError(err)
		bundled <- err
	}()

	step := j.b.push(j)
	var errBuf bytes.Buffer
	code, err := j.exec(ctx, createTimeout, step.cmd, pr, nil, &errBuf)
	pr.Close() // a writer the VM stopped reading from gets io.ErrClosedPipe
	if bundleErr := <-bundled; bundleErr != nil && !errors.Is(bundleErr, io.ErrClosedPipe) {
		j.r.say("could not pack the job's files: %v", bundleErr)
		return ExitSetup
	}
	if err != nil {
		j.r.say("could not copy the job's files into %s: %v", j.vm, err)
		return ExitLost
	}
	if code != 0 {
		j.r.say("could not copy the job's files into %s (%s exited %d): %s", j.b.place(), step.tool, code, strings.TrimSpace(errBuf.String()))
		return ExitSetup
	}
	return 0
}

func (j *job) start(ctx context.Context) int {
	step := j.b.start(j)
	var errBuf bytes.Buffer
	code, err := j.exec(ctx, commandTimeout, step.cmd, nil, nil, &errBuf)
	if err != nil {
		j.r.say("could not start the job in %s: %v", j.vm, err)
		return ExitLost
	}
	if code != 0 {
		j.r.say("could not start the job (%s exited %d): %s", step.tool, code, strings.TrimSpace(errBuf.String()))
		return ExitSetup
	}
	return 0
}

// supervise follows the running job's logs, polls it until it exits, and enforces its timeout and
// cancellation. It returns the job's exit code.
func (j *job) supervise(ctx context.Context) int {
	j.logs = j.b.newLogs(j)

	poke := make(chan struct{}, 1)
	followCtx, stopFollow := context.WithCancel(context.Background())
	followDone := make(chan struct{})
	go j.follow(followCtx, poke, followDone)
	finish := func() {
		stopFollow()
		<-followDone
		j.catchUp()
	}

	ticker := time.NewTicker(j.r.PollInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(j.req.timeout() + j.r.TimeoutSlack)
	defer deadline.Stop()

	missed := 0
	timedOut := false
	for {
		select {
		case <-ctx.Done():
			j.r.say("stopping the job")
			code := j.stop(j.r.StopGrace)
			finish()
			if code < 0 {
				return ExitCanceled
			}
			return code
		case <-deadline.C:
			j.r.say("the job ran past its %s timeout; stopping it", j.req.timeout())
			timedOut = true
			j.stop(defaultStopGrace)
		case <-ticker.C:
		case <-poke:
		}

		status, code, err := j.inspect()
		if err != nil {
			missed++
			if missed >= j.r.MissedPolls {
				stopFollow()
				<-followDone
				j.r.say("lost the job: %s stopped answering (%v)", j.vm, err)
				return ExitLost
			}
			continue
		}
		missed = 0
		if status != "exited" && status != "dead" {
			continue
		}
		finish()
		switch {
		case timedOut:
			return ExitTimedOut
		case code == ExitLost:
			j.r.say("the job exited %d, reported as 1 (%d means grove lost the VM)", ExitLost, ExitLost)
			return 1
		default:
			return code
		}
	}
}

// follow streams the job's logs, reattaching from where it left off whenever the stream drops,
// until ctx ends. Each time a stream ends it pokes supervise to check on the job right away.
func (j *job) follow(ctx context.Context, poke chan<- struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		stdout, stderr := j.logs.streams()
		_, _ = j.exec(ctx, 0, j.logs.command(true), nil, stdout, stderr)
		j.logs.reset()
		select {
		case poke <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// catchUp fetches whatever output follow didn't deliver, once the job has exited.
func (j *job) catchUp() {
	stdout, stderr := j.logs.streams()
	if _, err := j.exec(context.Background(), 30*time.Second, j.logs.command(false), nil, stdout, stderr); err != nil {
		j.r.say("could not fetch the end of the job's output: %v", err)
	}
	j.logs.flush()
}

// inspect reports the job's state ("running", "exited", …) and exit code. The backend's inspect
// command prints both as "<state> <code>".
func (j *job) inspect() (string, int, error) {
	step := j.b.inspect(j)
	var out bytes.Buffer
	code, err := j.exec(context.Background(), commandTimeout, step.cmd, nil, &out, nil)
	if err != nil {
		return "", 0, err
	}
	if code != 0 {
		return "", 0, fmt.Errorf("%s exited %d", step.tool, code)
	}
	status, exit, ok := strings.Cut(strings.TrimSpace(out.String()), " ")
	n, perr := strconv.Atoi(exit)
	if !ok || perr != nil {
		return "", 0, fmt.Errorf("unexpected %s output %q", step.tool, out.String())
	}
	return status, n, nil
}

// stop asks the job to stop, killing it after grace, and returns its exit code, or -1 when that
// can't be read.
func (j *job) stop(grace time.Duration) int {
	if _, err := j.exec(context.Background(), grace+commandTimeout, j.b.stop(j, grace).cmd, nil, nil, nil); err != nil {
		j.r.say("could not stop the job in %s: %v", j.vm, err)
		return -1
	}
	status, code, err := j.inspect()
	if err != nil || (status != "exited" && status != "dead") {
		return -1
	}
	return code
}

// collectArtifacts copies the build's ./artifacts/** out of the VM and stores each regular file
// under the job's artifact prefix in the control plane's artifact store. The VM sends a tar; only
// the regular files in it, validated paths and all, are written (by the store, never extracted).
func (j *job) collectArtifacts() {
	if j.r.Uploader == nil || j.req.ArtifactPrefix == "" {
		return
	}
	pr, pw := io.Pipe()
	uploaded := make(chan error, 1)
	var count int
	go func() {
		n, err := j.uploadTar(pr)
		count = n
		pr.CloseWithError(err)
		uploaded <- err
	}()

	step := j.b.artifacts(j)
	var errBuf bytes.Buffer
	code, err := j.exec(context.Background(), createTimeout, step.cmd, nil, pw, &errBuf)
	pw.CloseWithError(err)
	upErr := <-uploaded

	switch {
	case code != 0 && j.b.noArtifacts(errBuf.String()):
		return // the job made no artifacts
	case upErr != nil:
		j.r.say("could not upload the job's artifacts: %v", upErr)
	case err != nil:
		j.r.say("could not copy the job's artifacts out of %s: %v", j.vm, err)
	case code != 0:
		j.r.say("could not copy the job's artifacts out (%s exited %d): %s", step.tool, code, strings.TrimSpace(errBuf.String()))
	default:
		if count > 0 {
			j.r.say("uploaded %d artifact(s)", count)
		}
	}
}

// uploadTar uploads the regular files in an artifacts tar stream (`docker cp …/artifacts -`, or
// `tar -c … artifacts` in a macOS VM). Paths are taken relative to the artifacts directory;
// anything that would climb out of it is skipped.
func (j *job) uploadTar(r io.Reader) (int, error) {
	prefix := strings.TrimSuffix(j.req.ArtifactPrefix, "/") + "/"
	tr := tar.NewReader(r)
	var total int64
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			_, _ = io.Copy(io.Discard, r)
			return count, nil
		}
		if err != nil {
			return count, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel, ok := artifactPath(hdr.Name)
		if !ok {
			continue
		}
		total += hdr.Size
		if total > j.r.ArtifactLimit {
			return count, fmt.Errorf("artifacts exceed %d bytes", j.r.ArtifactLimit)
		}
		ct := mime.TypeByExtension(path.Ext(rel))
		if ct == "" {
			ct = "application/octet-stream"
		}
		if err := j.r.Uploader.Put(context.Background(), prefix+rel, tr, hdr.Size, ct); err != nil {
			return count, err
		}
		count++
	}
}

// artifactPath turns a tar entry name ("artifacts/dir/file") into the path below the artifacts
// directory, refusing anything absolute or climbing out.
func artifactPath(name string) (string, bool) {
	name = strings.TrimPrefix(name, "./")
	_, rel, ok := strings.Cut(name, "/")
	if !ok || rel == "" || strings.HasPrefix(rel, "/") {
		return "", false
	}
	if strings.ContainsAny(rel, "\\\x00") {
		return "", false // the store (and the download API) refuse these; skip, don't abort
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", false
		}
	}
	rel = path.Clean(rel)
	if rel == "." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// shellJoin quotes args into one shell command line, which is what Orchard's exec runs.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@+,") == "" {
			quoted[i] = a
			continue
		}
		quoted[i] = shQuote(a)
	}
	return strings.Join(quoted, " ")
}
