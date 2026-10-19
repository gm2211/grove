//go:build unix

package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// fakeMacVM plays a macOS VM reached through orchard.Client.Exec by really running each command
// line macBackend sends, with /bin/sh on this machine, so the launcher, process groups, exit-code
// file, log offsets and stop logic are exercised for real. The VM's disk is a temporary directory:
// every "/private/var/" in a command is rewritten to root+"/private/var/". sudo is a shim that
// drops its flags (checking -u is the job user) and logs each call; pkill is a no-op, so nothing
// outside the test's own processes is ever signalled.
type fakeMacVM struct {
	t    *testing.T
	root string
	bin  string

	mu       sync.Mutex
	commands []string
	pushed   map[string]string // file name -> content from the push step's tar
	// fail makes every exec fail, as if the VM were gone; failAfterStart sets it once the job
	// has been started.
	fail           bool
	failAfterStart bool
	// dropFirstFollow cuts the first follow attach halfway through its first chunk of output.
	dropFirstFollow bool
	follows         int
}

func newFakeMacVM(t *testing.T) *fakeMacVM {
	t.Helper()
	f := &fakeMacVM{t: t, root: t.TempDir(), bin: t.TempDir(), pushed: map[string]string{}}
	for _, dir := range []string{"private/var/run", "private/var/grove-job"} {
		if err := os.MkdirAll(filepath.Join(f.root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"sudo": `#!/bin/sh
printf '%s\n' "$*" >> "$GROVE_TEST_SUDO_LOG"
while [ $# -gt 0 ]; do
  case "$1" in
    -n|-H) shift ;;
    -u) [ "$2" = _grovejob ] || { echo "sudo: unknown user $2" >&2; exit 1; }; shift 2 ;;
    *) break ;;
  esac
done
exec "$@"
`,
		"pkill": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(f.bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A job left running (a lost VM never gets its remove step) must not write into the
	// temporary directories while they are being deleted: kill its launcher, then its group.
	t.Cleanup(func() {
		dirs, _ := filepath.Glob(filepath.Join(f.root, "private/var/grove-sandbox/*"))
		for _, dir := range dirs {
			for _, name := range []string{"launcher", "pgid"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && perr == nil && pid > 0 {
					if name == "pgid" {
						pid = -pid
					}
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	})
	return f
}

func (f *fakeMacVM) path(p string) string { return filepath.Join(f.root, p) }

func (f *fakeMacVM) sudoLog() string {
	data, _ := os.ReadFile(filepath.Join(f.bin, "sudo.log"))
	return string(data)
}

func (f *fakeMacVM) ran(fragment string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.Contains(c, fragment) {
			return true
		}
	}
	return false
}

func (f *fakeMacVM) setFail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = true
}

func (f *fakeMacVM) exec(ctx context.Context, cmd string, opts orchard.ExecOptions) (orchard.ExecSession, error) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	if f.fail {
		f.mu.Unlock()
		return nil, errors.New("vm gone")
	}
	isPush := strings.Contains(cmd, "tar -x -f - -C ")
	isStart := strings.Contains(cmd, "nohup /bin/bash -c ")
	drop := false
	if strings.Contains(cmd, "tail -c +") && strings.Contains(cmd, " -f ") {
		f.follows++
		drop = f.dropFirstFollow && f.follows == 1
	}
	f.mu.Unlock()

	local := strings.ReplaceAll(cmd, "/private/var/", f.root+"/private/var/")
	c := exec.Command("/bin/sh", "-c", local)
	c.Env = append(os.Environ(), "PATH="+f.bin+":"+os.Getenv("PATH"), "GROVE_TEST_SUDO_LOG="+filepath.Join(f.bin, "sudo.log"))
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdin bytes.Buffer
	if opts.Stdin != nil {
		c.Stdin = io.TeeReader(opts.Stdin, &stdin)
	}
	pr, pw := io.Pipe()
	s := &localSession{out: pr, pw: pw, done: make(chan struct{}), started: make(chan struct{})}
	if drop {
		c.Stdout = &droppingWriter{w: pw, drop: func() { s.kill(errors.New("connection reset")) }}
	} else {
		c.Stdout = pw
	}
	c.Stderr = io.Discard
	if opts.Stderr != nil {
		c.Stderr = opts.Stderr
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	s.pid = c.Process.Pid
	close(s.started)

	go func() {
		err := c.Wait()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exitErr):
			s.code = exitErr.ExitCode()
		default:
			s.err = err
		}
		if isPush {
			f.recordPush(stdin.Bytes())
		}
		if isStart && f.failAfterStart {
			f.setFail()
		}
		pw.Close()
		close(s.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			s.kill(ctx.Err())
		case <-s.done:
		}
	}()
	return s, nil
}

func (f *fakeMacVM) recordPush(data []byte) {
	files := untar(f.t, data)
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, body := range files {
		f.pushed[name] = body
	}
}

// localSession is an orchard.ExecSession over a local process group.
type localSession struct {
	out *io.PipeReader
	pw  *io.PipeWriter
	pid int
	// started is closed once pid is set.
	started chan struct{}
	done    chan struct{}
	code    int
	err     error
	once    sync.Once
}

func (s *localSession) kill(err error) {
	s.once.Do(func() {
		<-s.started
		_ = syscall.Kill(-s.pid, syscall.SIGKILL)
		s.pw.CloseWithError(err)
	})
}

// untar reads a tar's regular files and directories (directories map to "").
func untar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err != nil {
			return files
		}
		body, _ := io.ReadAll(tr)
		files[hdr.Name] = string(body)
	}
}

func (s *localSession) Output() io.Reader { return s.out }

func (s *localSession) Wait(ctx context.Context) (int, error) {
	select {
	case <-s.done:
		return s.code, s.err
	case <-ctx.Done():
		s.kill(ctx.Err())
		return 0, ctx.Err()
	}
}

func (s *localSession) Close() error {
	select {
	case <-s.done:
	default:
		s.kill(errors.New("session closed"))
	}
	return nil
}

// droppingWriter passes on half of the first write, then drops the exchange.
type droppingWriter struct {
	w       io.Writer
	drop    func()
	dropped bool
}

func (d *droppingWriter) Write(p []byte) (int, error) {
	if d.dropped {
		return 0, io.ErrClosedPipe
	}
	d.dropped = true
	n := (len(p) + 1) / 2
	_, _ = d.w.Write(p[:n])
	d.drop()
	return 0, io.ErrClosedPipe
}
