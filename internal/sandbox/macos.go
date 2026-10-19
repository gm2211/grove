package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/fleet"
)

// A macOS guest has no container runtime, so macBackend runs the job as a plain process tree of
// the VM's unprivileged job user, the account fleet's startup script creates for every macOS VM:
//
//   - its files live in a fresh run directory, macRunsDir/<alloc>, owned by the job user and
//     closed to everyone else (0700): workspace/ (the checkout), grove/ (run.sh, script.sh,
//     env.sh) and tmp/ (its TMPDIR);
//   - grove's own bookkeeping lives in a root-only control directory, macControlDir/<alloc>: the
//     job's stdout and stderr, the launcher's PID, the job's process group and its exit code, so
//     the job can't touch the files grove reads back, and root never writes into a directory the
//     job owns;
//   - a detached root launcher starts run.sh as the job user in a process group of its own and
//     writes the exit code atomically when it ends, so nothing depends on the exec session that
//     started it staying open.
//
// Every command runs as the VM's admin user (Orchard's SSH login) through `sudo -n`, as the
// fleet's macOS startup script does. Apple lets a Mac run only two macOS VMs at once, so a sealed
// macOS VM is never reused: remove marks it draining and deletes it through Orchard, and the fleet
// reconciler recreates it from the image.
type macBackend struct{}

const (
	macJobUser = fleet.MacOSJobUser
	// macRunsDir holds the jobs' run directories, inside the job user's home.
	macRunsDir = fleet.MacOSJobHome + "/runs"
	// macControlDir holds grove's per-job control directories, root's alone.
	macControlDir = "/private/var/grove-sandbox"
	// macPath is the job's PATH: the same one the image's Nomad client gives a shared macOS
	// pool's jobs (images/macos-worker/files/com.grove.nomad.plist).
	macPath = "/usr/local/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/bin:/bin:/usr/sbin:/sbin"
	// macNoArtifacts is how the artifacts command says the job made none.
	macNoArtifacts = "grove: no artifacts"
	// macUsedExit is how create reports a VM that already ran a job.
	macUsedExit = 76
	// macUnknownUser is sudo's complaint about a job user the startup script hasn't created yet.
	macUnknownUser = "unknown user"
)

// macFollowPoll is how often (in seconds, as sleep(1) takes it) the follow command checks whether
// the job has ended. Tests shorten it.
var macFollowPoll = "1"

// macDeleteAttempts and macDeleteBackoff bound remove's retries of Orchard's DeleteVM.
var (
	macDeleteAttempts = 3
	macDeleteBackoff  = 5 * time.Second
)

// macPaths makes run.sh find its run directory from its own path, grove/run.sh.
var macPaths = jobPaths{
	prelude: `grove_run=$(cd "$(dirname "$0")/.." && pwd -P) || exit 1
export HOME=` + fleet.MacOSJobHome + ` USER=` + macJobUser + ` LOGNAME=` + macJobUser + `
export TMPDIR="$grove_run/tmp"
export PATH=` + macPath + `
`,
	workspace: `"$grove_run/workspace"`,
	grove:     `"$grove_run/grove"`,
}

func (macBackend) paths() jobPaths { return macPaths }

func (macBackend) place() string { return "its run directory" }

// runDir and controlDir are the job's directories; AllocID is validated, so they need no quoting.
func runDir(j *job) string     { return macRunsDir + "/" + j.req.AllocID }
func controlDir(j *job) string { return macControlDir + "/" + j.req.AllocID }

// asRoot and asJobUser wrap a bash script into the command line that runs it in the VM.
func asRoot(script string) string { return "sudo -n /bin/bash -c " + shQuote(script) }
func asJobUser(script string) string {
	return "sudo -n -u " + macJobUser + " /bin/bash -c " + shQuote(script)
}

// create checks the VM isn't shutting down and has never had a job, then makes the job's fresh
// control and run directories. A VM that already holds another job's control directory was left
// behind by a runner that died before it could delete it (Nomad killed it, say): it is marked
// draining and deleted here instead.
func (macBackend) create(ctx context.Context, j *job) int {
	c, r := controlDir(j), runDir(j)
	cmd := fmt.Sprintf("if [ -e %s ]; then exit %d; fi; %s && %s",
		fleet.SealedDrainingMarkerMacOS, drainingExit,
		asRoot(fmt.Sprintf(`set -e
if [ -d %[1]s ] && [ -n "$(ls -A %[1]s)" ]; then touch %[3]s; exit %[4]d; fi
mkdir -p -m 0700 %[1]s; mkdir -m 0700 %[2]s; : > %[2]s/stdout; : > %[2]s/stderr`, macControlDir, c, fleet.SealedDrainingMarkerMacOS, macUsedExit)),
		asJobUser(fmt.Sprintf("set -e; umask 077; mkdir -p %s; rm -rf %s; mkdir %[2]s %[2]s/tmp", macRunsDir, r)))
	var errBuf bytes.Buffer
	code, err := j.exec(ctx, commandTimeout, cmd, nil, nil, io.MultiWriter(j.r.stderr(), &errBuf))
	if err != nil {
		j.r.say("could not prepare the job's run directory in %s: %v", j.vm, err)
		return ExitLost
	}
	if code == drainingExit {
		return exitDraining
	}
	if code == macUsedExit {
		j.r.say("%s already ran an earlier job, whose runner never deleted it", j.vm)
		deleteVM(j)
		return exitDraining
	}
	if code != 0 {
		if strings.Contains(errBuf.String(), macUnknownUser) {
			j.r.say("%s has no %s account yet: its startup script hasn't run", j.vm, macJobUser)
			return ExitLost
		}
		j.r.say("could not prepare the job's run directory (exit %d)", code)
		return ExitSetup
	}
	return 0
}

// push unpacks the bundle as the job user, so everything in the run directory is the job's own.
func (macBackend) push(j *job) step {
	return step{cmd: asJobUser("tar -x -f - -C " + runDir(j)), tool: "tar"}
}

// start launches run.sh detached: a root launcher, immune to the exec session closing, runs it as
// the job user, in a new process group and an empty environment (run.sh sets what the job gets),
// with its output appended to the control directory's stdout and stderr, records that group,
// waits, and renames the exit code into place.
func (macBackend) start(j *job) step {
	c, r := controlDir(j), runDir(j)
	launcher := fmt.Sprintf(`set -m
sudo -n -u %[3]s -H /usr/bin/env -i /bin/bash %[2]s/grove/run.sh </dev/null >>%[1]s/stdout 2>>%[1]s/stderr &
grove_pg=$!
echo "$grove_pg" > %[1]s/pgid.new && mv %[1]s/pgid.new %[1]s/pgid
wait "$grove_pg"
grove_code=$?
echo "$grove_code" > %[1]s/exit.new && mv %[1]s/exit.new %[1]s/exit
`, c, r, macJobUser)
	return step{
		cmd:  asRoot(fmt.Sprintf("nohup /bin/bash -c %s </dev/null >/dev/null 2>&1 & echo $! > %s/launcher", shQuote(launcher), c)),
		tool: "the launcher",
	}
}

func (macBackend) newLogs(j *job) jobLogs {
	return &macLogs{dir: controlDir(j), stdout: &offsetStream{out: j.r.Stdout}, stderr: &offsetStream{out: j.r.stderr()}}
}

// macLauncherAlive is a test for the launcher still running.
func macLauncherAlive(c string) string {
	return fmt.Sprintf(`kill -0 "$(cat %s/launcher 2>/dev/null)" 2>/dev/null`, c)
}

// inspect reports "exited <code>" once the exit code is written, "running 0" while the launcher
// is alive, and fails when neither holds (the VM rebooted under the job, or its files are gone).
func (macBackend) inspect(j *job) step {
	c := controlDir(j)
	exited := fmt.Sprintf(`if [ -f %[1]s/exit ]; then echo "exited $(cat %[1]s/exit)"; exit 0; fi`, c)
	return step{
		cmd: asRoot(fmt.Sprintf(`%[1]s
if %[2]s; then echo "running 0"; exit 0; fi
%[1]s
echo "grove: the job's launcher is gone" >&2
exit 3`, exited, macLauncherAlive(c))),
		tool: "inspect",
	}
}

// stop sends TERM to the job's process group, waits up to grace for it to go, then kills it and
// every process left of the job user (a VM runs one job, so they are all this job's), and waits a
// little for the launcher to record the exit code.
func (macBackend) stop(j *job, grace time.Duration) step {
	c := controlDir(j)
	secs := max(int(grace/time.Second), 1)
	return step{
		cmd: asRoot(fmt.Sprintf(`grove_i=0
while [ ! -s %[1]s/pgid ] && [ "$grove_i" -lt 10 ] && %[2]s; do sleep 0.5; grove_i=$((grove_i+1)); done
grove_pg=$(cat %[1]s/pgid 2>/dev/null || true)
if [ -n "$grove_pg" ]; then
  kill -s TERM -- "-$grove_pg" 2>/dev/null || true
  grove_i=0
  while [ "$grove_i" -lt %[3]d ] && kill -s 0 -- "-$grove_pg" 2>/dev/null; do sleep 1; grove_i=$((grove_i+1)); done
  kill -s KILL -- "-$grove_pg" 2>/dev/null || true
fi
pkill -9 -U %[4]s 2>/dev/null || true
grove_i=0
while [ ! -f %[1]s/exit ] && [ "$grove_i" -lt 20 ] && %[2]s; do sleep 0.5; grove_i=$((grove_i+1)); done
exit 0`, c, macLauncherAlive(c), secs, macJobUser)),
		tool: "stop",
	}
}

// artifacts tars the workspace's artifacts directory as the job user, so it reads nothing the job
// couldn't (a symlink the job planted leads nowhere it couldn't already go).
func (macBackend) artifacts(j *job) step {
	ws := runDir(j) + "/workspace"
	return step{
		cmd: asJobUser(fmt.Sprintf(`if [ ! -d %[1]s/artifacts ]; then echo %[2]s >&2; exit 3; fi
exec tar -c -f - -C %[1]s artifacts`, ws, shQuote(macNoArtifacts))),
		tool: "tar",
	}
}

func (macBackend) noArtifacts(stderr string) bool { return strings.Contains(stderr, macNoArtifacts) }

// remove marks the VM draining (no other runner may place a job in it from now on), kills what is
// left of the job, deletes its directories, and then deletes the VM itself through Orchard: a
// sealed macOS VM runs one job, and the fleet reconciler recreates it fresh from the image.
func (macBackend) remove(j *job) {
	cmd := asRoot(fmt.Sprintf("touch %s; pkill -9 -U %s 2>/dev/null; rm -rf %s %s; exit 0",
		fleet.SealedDrainingMarkerMacOS, macJobUser, runDir(j), controlDir(j)))
	if _, err := j.exec(context.Background(), commandTimeout, cmd, nil, nil, nil); err != nil {
		j.r.say("could not clean up after the job in %s: %v", j.vm, err)
	}
	deleteVM(j)
}

// deleteVM deletes the job's VM through Orchard, retrying a little.
func deleteVM(j *job) {
	var err error
	for attempt := range macDeleteAttempts {
		if attempt > 0 {
			time.Sleep(macDeleteBackoff)
		}
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		err = j.r.Orchard.DeleteVM(ctx, j.vm)
		cancel()
		if err == nil {
			j.r.say("deleted %s: a sealed macOS VM runs one job; the fleet reconciler recreates it", j.vm)
			return
		}
	}
	j.r.say("could not delete %s after its job (%v); it takes no new job, and is replaced when it is next recycled", j.vm, err)
}

// macLogs follows the job's stdout and stderr files with `tail -c +N -f`, N being one past the
// bytes of that stream already received: a reattach after a dropped exchange resumes at the exact
// byte, so nothing is repeated or lost.
type macLogs struct {
	dir            string
	stdout, stderr *offsetStream
}

func (l *macLogs) streams() (io.Writer, io.Writer) { return l.stdout, l.stderr }

func (l *macLogs) command(follow bool) string {
	out, errs := l.stdout.next(), l.stderr.next()
	if !follow {
		return asRoot(fmt.Sprintf("tail -c +%d %s/stdout && tail -c +%d %s/stderr >&2", out, l.dir, errs, l.dir))
	}
	// The tails follow until the job's exit code appears (or its launcher is gone), plus a
	// moment for them to deliver the last bytes; catchUp fetches anything they still miss.
	return asRoot(fmt.Sprintf(`tail -c +%[1]d -f %[3]s/stdout & grove_out=$!
tail -c +%[2]d -f %[3]s/stderr >&2 & grove_err=$!
while [ ! -f %[3]s/exit ] && %[4]s; do sleep %[5]s; done
sleep %[5]s
kill "$grove_out" "$grove_err" 2>/dev/null
wait
exit 0`, out, errs, l.dir, macLauncherAlive(l.dir), macFollowPoll))
}

func (l *macLogs) reset() {}

func (l *macLogs) flush() {
	l.stdout.flush()
	l.stderr.flush()
}

// offsetStream passes one output stream through a line at a time, counting the bytes it has
// received so a reattach can resume right after them. A line cut off by a dropped exchange is held
// until its end arrives in the next attach.
type offsetStream struct {
	mu      sync.Mutex
	out     io.Writer
	off     int64
	partial []byte
}

func (s *offsetStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.off += int64(len(p))
	s.partial = append(s.partial, p...)
	if i := bytes.LastIndexByte(s.partial, '\n'); i >= 0 {
		_, _ = s.out.Write(s.partial[:i+1])
		s.partial = append([]byte(nil), s.partial[i+1:]...)
	}
	return len(p), nil
}

// next is tail's 1-based starting byte for the next attach.
func (s *offsetStream) next() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.off + 1
}

func (s *offsetStream) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.partial) > 0 {
		_, _ = s.out.Write(s.partial)
		s.partial = nil
	}
}
