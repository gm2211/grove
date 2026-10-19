package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// fakeVM plays one VM's docker, as reached through orchard.Client.Exec: it keeps a single job
// container and answers the commands Runner sends.
type fakeVM struct {
	mu sync.Mutex

	commands []string
	pushed   map[string]string // file name -> content from `docker cp -`

	created  bool
	removed  bool
	draining bool
	// status is what inspect reports; runningInspects counts down the "running" answers before
	// the container exits with exitCode (negative: never exits on its own).
	status          string
	runningInspects int
	exitCode        int
	stopCode        int
	stopped         bool
	// failInspects makes that many inspects fail as if the VM had vanished; failAll fails every
	// exec after the job starts.
	failInspects int
	failAll      bool

	logs []fakeLog
	// dropFollow cuts the first follow attach after this many lines.
	dropFollow int
	follows    int

	artifacts []byte // tar for `docker cp …/artifacts -`; nil: no artifacts directory

	exited chan struct{}
}

type fakeLog struct {
	at     time.Time
	stderr bool
	text   string
}

func newFakeVM() *fakeVM {
	return &fakeVM{pushed: map[string]string{}, status: "created", exited: make(chan struct{}), dropFollow: -1}
}

func (f *fakeVM) exit(code int) {
	if f.status != "exited" {
		f.status = "exited"
		f.exitCode = code
		close(f.exited)
	}
}

// fakeOrchard serves a set of VMs; only vms in docker (Linux, a fakeVM) or mac (macOS, a
// fakeMacVM) answer exec. DeleteVM takes a VM off the list, as Orchard would once it is gone.
type fakeOrchard struct {
	mu      sync.Mutex
	vms     []orchard.VM
	docker  map[string]*fakeVM
	mac     map[string]*fakeMacVM
	deleted []string
	// deleteErr makes that many DeleteVM calls fail.
	deleteErr int
}

func (o *fakeOrchard) ListWorkers(context.Context) ([]orchard.Worker, error) { return nil, nil }
func (o *fakeOrchard) PauseWorker(context.Context, string) error             { return nil }
func (o *fakeOrchard) ResumeWorker(context.Context, string) error            { return nil }
func (o *fakeOrchard) ListVMs(context.Context) ([]orchard.VM, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.vms), nil
}
func (o *fakeOrchard) GetVM(context.Context, string) (*orchard.VM, error) { return nil, nil }
func (o *fakeOrchard) CreateVM(context.Context, orchard.VMSpec) (*orchard.VM, error) {
	return nil, nil
}
func (o *fakeOrchard) DeleteVM(_ context.Context, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.deleteErr > 0 {
		o.deleteErr--
		return errors.New("controller unavailable")
	}
	o.deleted = append(o.deleted, name)
	o.vms = slices.DeleteFunc(o.vms, func(vm orchard.VM) bool { return vm.Name == name })
	return nil
}
func (o *fakeOrchard) Ping(context.Context) error { return nil }

func (o *fakeOrchard) wasDeleted(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Contains(o.deleted, name)
}

func (o *fakeOrchard) Exec(ctx context.Context, vm string, command []string, opts orchard.ExecOptions) (orchard.ExecSession, error) {
	if f, ok := o.mac[vm]; ok {
		return f.exec(ctx, strings.Join(command, " "), opts)
	}
	f, ok := o.docker[vm]
	if !ok {
		return nil, fmt.Errorf("exec into %s: not a VM this test expects jobs in", vm)
	}
	return f.exec(ctx, strings.Join(command, " "), opts)
}

type fakeSession struct {
	out  io.Reader
	code int
	err  error
	wait func(context.Context) // blocks until the session would end
}

func (s *fakeSession) Output() io.Reader { return s.out }
func (s *fakeSession) Wait(ctx context.Context) (int, error) {
	if s.wait != nil {
		s.wait(ctx)
	}
	return s.code, s.err
}
func (s *fakeSession) Close() error { return nil }

func done(out string, code int) *fakeSession {
	return &fakeSession{out: strings.NewReader(out), code: code}
}

func (f *fakeVM) exec(ctx context.Context, cmd string, opts orchard.ExecOptions) (orchard.ExecSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmd)
	if f.failAll && f.status != "created" {
		return nil, errors.New("vm gone")
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	switch {
	case strings.HasPrefix(cmd, "if [ -e /run/grove-draining ]; then exit 75; fi; docker create "):
		if f.draining {
			return done("", 75), nil
		}
		f.created = true
		return done("cid\n", 0), nil

	case cmd == "docker cp - grove-alloc1:/":
		f.mu.Unlock()
		data, err := io.ReadAll(opts.Stdin)
		f.mu.Lock()
		if err != nil {
			return nil, err
		}
		tr := tar.NewReader(bytes.NewReader(data))
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			body, _ := io.ReadAll(tr)
			f.pushed[hdr.Name] = string(body)
		}
		return done("", 0), nil

	case cmd == "docker start grove-alloc1":
		f.status = "running"
		return done("grove-alloc1\n", 0), nil

	case strings.HasPrefix(cmd, "docker inspect "):
		if f.failInspects > 0 {
			f.failInspects--
			return nil, errors.New("vm not answering")
		}
		if f.status == "running" && f.runningInspects == 0 {
			f.exit(f.exitCode)
		}
		if f.runningInspects > 0 {
			f.runningInspects--
		}
		return done(fmt.Sprintf("%s %d\n", f.status, f.exitCode), 0), nil

	case strings.HasPrefix(cmd, "docker logs "):
		return f.logsSession(ctx, cmd, stderr), nil

	case strings.HasPrefix(cmd, "docker stop "):
		f.stopped = true
		f.exit(f.stopCode)
		return done("grove-alloc1\n", 0), nil

	case cmd == "docker cp grove-alloc1:/workspace/artifacts -":
		if f.artifacts == nil {
			fmt.Fprintln(stderr, "Error response from daemon: Could not find the file /workspace/artifacts in container grove-alloc1")
			return done("", 1), nil
		}
		return done(string(f.artifacts), 0), nil

	case cmd == "docker rm --force grove-alloc1":
		f.removed = true
		return done("grove-alloc1\n", 0), nil
	}
	return nil, fmt.Errorf("unexpected command %q", cmd)
}

// logsSession answers `docker logs [--follow] --timestamps [--since S] grove-alloc1`.
func (f *fakeVM) logsSession(ctx context.Context, cmd string, stderr io.Writer) *fakeSession {
	follow := strings.Contains(cmd, "--follow")
	var since time.Time
	if _, rest, ok := strings.Cut(cmd, "--since "); ok {
		var sec, nsec int64
		fmt.Sscanf(strings.Fields(rest)[0], "%d.%d", &sec, &nsec)
		since = time.Unix(sec, nsec)
	}

	var out bytes.Buffer
	sent := 0
	drop := false
	if follow {
		f.follows++
		drop = f.follows == 1 && f.dropFollow >= 0
	}
	for _, l := range f.logs {
		if l.at.Before(since) {
			continue
		}
		if drop && sent == f.dropFollow {
			// Cut mid-line, as a dropped connection would.
			fmt.Fprintf(&out, "%s %s", l.at.Format(time.RFC3339Nano), l.text[:len(l.text)/2])
			return &fakeSession{out: &out, err: errors.New("connection reset")}
		}
		line := fmt.Sprintf("%s %s\n", l.at.Format(time.RFC3339Nano), l.text)
		if l.stderr {
			io.WriteString(stderr, line)
		} else {
			out.WriteString(line)
		}
		sent++
	}
	if !follow {
		return &fakeSession{out: &out}
	}
	exited := f.exited
	return &fakeSession{out: &out, wait: func(waitCtx context.Context) {
		select {
		case <-exited:
		case <-ctx.Done():
		case <-waitCtx.Done():
		}
	}}
}

func (f *fakeVM) ran(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// fakeUploader records uploads.
type fakeUploader struct {
	mu      sync.Mutex
	objects map[string]string
	types   map[string]string
}

func (u *fakeUploader) Put(_ context.Context, key string, r io.Reader, size int64, contentType string) error {
	body, err := io.ReadAll(io.LimitReader(r, size))
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.objects == nil {
		u.objects = map[string]string{}
		u.types = map[string]string{}
	}
	u.objects[key] = string(body)
	u.types[key] = contentType
	return nil
}
