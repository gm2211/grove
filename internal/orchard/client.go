package orchard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	orchardclient "github.com/cirruslabs/orchard/pkg/client"
	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
	"github.com/coder/websocket"
)

// workerOfflineTimeout is how long since a Worker's last heartbeat before grove treats it as
// offline. The Orchard client library doesn't expose the controller's own configured timeout
// (see internal/controller/scheduler.workerOfflineTimeout in the fork), so this mirrors the
// scheduler's health-check semantics using a fixed, conservative window.
const workerOfflineTimeout = 30 * time.Second

// client is the concrete orchard.Client, wrapping github.com/cirruslabs/orchard/pkg/client
// (our fork, github.com/gm2211/orchard — see go.mod's replace directive and ARCHITECTURE.md's
// "Fork of Orchard" section).
type client struct {
	raw *orchardclient.Client
}

// New constructs a Client talking to the Orchard controller at url.
//
// token authenticates as an Orchard service account. It is either a bare token (used with an
// empty service-account name) or "name:token" if the controller requires both; grove's config
// contract (internal/config.Endpoint) only carries a single token string, so this is the seam
// that lets either convention work without changing that contract.
func New(url, token string) (Client, error) {
	opts := []orchardclient.Option{orchardclient.WithAddress(url)}

	if name, tok := splitToken(token); tok != "" {
		opts = append(opts, orchardclient.WithCredentials(name, tok))
	}

	raw, err := orchardclient.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("orchard: %w", err)
	}

	return &client{raw: raw}, nil
}

func splitToken(token string) (name, tok string) {
	if i := strings.IndexByte(token, ':'); i >= 0 {
		return token[:i], token[i+1:]
	}

	return "", token
}

func (c *client) Ping(ctx context.Context) error {
	return c.raw.Check(ctx)
}

func (c *client) ListWorkers(ctx context.Context) ([]Worker, error) {
	workers, err := c.raw.Workers().List(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Worker, 0, len(workers))
	for _, w := range workers {
		out = append(out, workerFromV1(w))
	}

	return out, nil
}

func (c *client) PauseWorker(ctx context.Context, name string) error {
	return c.setWorkerPaused(ctx, name, true)
}

func (c *client) ResumeWorker(ctx context.Context, name string) error {
	return c.setWorkerPaused(ctx, name, false)
}

func (c *client) setWorkerPaused(ctx context.Context, name string, paused bool) error {
	w, err := c.raw.Workers().Get(ctx, name)
	if err != nil {
		return fmt.Errorf("get worker %s: %w", name, err)
	}

	w.SchedulingPaused = paused

	if _, err := c.raw.Workers().Update(ctx, *w); err != nil {
		return fmt.Errorf("update worker %s: %w", name, err)
	}

	return nil
}

func workerFromV1(w v1.Worker) Worker {
	return Worker{
		Name:             w.Name,
		MachineID:        w.MachineID,
		Arch:             string(w.Arch),
		Runtime:          string(w.Runtime),
		Labels:           map[string]string(w.Labels.Copy()),
		Resources:        map[string]uint64(w.Resources.Copy()),
		LastSeen:         w.LastSeen,
		Offline:          w.Offline(workerOfflineTimeout),
		SchedulingPaused: w.SchedulingPaused,
	}
}

func (c *client) ListVMs(ctx context.Context) ([]VM, error) {
	vms, err := c.raw.VMs().List(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]VM, 0, len(vms))
	for _, vm := range vms {
		out = append(out, vmFromV1(vm))
	}

	return out, nil
}

func (c *client) GetVM(ctx context.Context, name string) (*VM, error) {
	vm, err := c.raw.VMs().Get(ctx, name)
	if err != nil {
		return nil, err
	}

	out := vmFromV1(*vm)

	return &out, nil
}

func (c *client) CreateVM(ctx context.Context, spec VMSpec) (*VM, error) {
	vm := vmToV1(spec)

	if err := c.raw.VMs().Create(ctx, vm); err != nil {
		return nil, err
	}

	// Create() doesn't echo back the created resource's server-assigned state (UID,
	// CreatedAt, ...), so fetch it explicitly.
	return c.GetVM(ctx, spec.Name)
}

func (c *client) DeleteVM(ctx context.Context, name string) error {
	return c.raw.VMs().Delete(ctx, name)
}

func vmFromV1(vm v1.VM) VM {
	return VM{
		Name:          vm.Name,
		UID:           vm.UID,
		Image:         vm.Image,
		Status:        string(vm.Status),
		StatusMessage: vm.StatusMessage,
		Worker:        vm.Worker,
		CPU:           vm.AssignedCPU,
		Memory:        vm.AssignedMemory,
		Labels:        map[string]string(vm.Labels.Copy()),
		Resources:     map[string]uint64(vm.Resources.Copy()),
		RestartPolicy: string(vm.RestartPolicy),
		RestartCount:  vm.RestartCount,
		CreatedAt:     vm.CreatedAt,
		StartedAt:     vm.StartedAt,
		TTL:           time.Duration(vm.TTLSeconds) * time.Second,
	}
}

func vmToV1(spec VMSpec) *v1.VM {
	labels := make(v1.Labels, len(spec.Labels)+1)
	for k, v := range spec.Labels {
		labels[k] = v
	}

	// Pin the VM to its intended worker using Orchard's own scheduler-matched label:
	// every Worker automatically carries v1.LabelWorkerName, and the scheduler only ever
	// places a VM on a Worker whose Labels are a superset of the VM's
	// (see internal/controller/scheduler.schedulingLoopIteration in the fork, the
	// "!worker.Labels.Contains(unscheduledVM.Labels)" check). fleet.Reconciler sets
	// spec.Labels["host"] to the target worker's name; mirror it into the real pinning label
	// here so grove callers only ever need to know about "host".
	if host, ok := spec.Labels["host"]; ok && host != "" {
		labels[v1.LabelWorkerName] = host
	}

	vm := &v1.VM{
		Meta:          v1.Meta{Name: spec.Name},
		Image:         spec.Image,
		CPU:           spec.CPU,
		Memory:        spec.Memory,
		DiskSize:      spec.DiskSize,
		Headless:      spec.Headless,
		Username:      spec.Username,
		Password:      spec.Password,
		RestartPolicy: v1.RestartPolicy(spec.RestartPolicy),
		Labels:        labels,
		Resources:     v1.Resources(spec.Resources),
	}

	if spec.StartupScript != "" {
		vm.StartupScript = &v1.VMScript{ScriptContent: spec.StartupScript}
	}

	if spec.ShutdownScript != "" {
		vm.ShutdownScript = &v1.VMScript{ScriptContent: spec.ShutdownScript}
	}
	if spec.ShutdownTimeout > 0 {
		vm.ShutdownScriptTimeoutSeconds = uint64(spec.ShutdownTimeout.Seconds())
	}

	if spec.TTL > 0 {
		vm.TTLSeconds = uint64(spec.TTL.Seconds())
	}

	return vm
}

// Exec.

func (c *client) Exec(ctx context.Context, vm string, command []string, opts ExecOptions) (ExecSession, error) {
	waitSeconds := uint16(opts.Wait / time.Second)
	if opts.Wait > 0 && waitSeconds == 0 {
		waitSeconds = 1
	}

	conn, err := c.raw.VMs().ExecSession(ctx, vm, orchardclient.ExecSessionOptions{
		Command:     strings.Join(command, " "),
		Interactive: opts.Stdin != nil,
		TTY:         opts.TTY,
		WaitSeconds: waitSeconds,
		Session:     opts.Session,
	})
	if err != nil {
		return nil, fmt.Errorf("exec %s: %w", vm, err)
	}

	return newExecSession(ctx, conn, opts.Stdin), nil
}

// execFrameType and execFrame mirror the wire format of the fork's internal
// internal/execstream.Frame type. That package is internal to the orchard module and can't be
// imported from grove, so its JSON shape (documented by the controller's exec endpoint,
// internal/controller/api_vms_exec.go) is reproduced here instead.
type execFrameType string

const (
	execFrameStdin  execFrameType = "stdin"
	execFrameResize execFrameType = "resize"
	execFrameStdout execFrameType = "stdout"
	execFrameStderr execFrameType = "stderr"
	execFrameExit   execFrameType = "exit"
	execFrameError  execFrameType = "error"
)

type execFrame struct {
	Type  execFrameType `json:"type"`
	Data  []byte        `json:"data,omitempty"`
	Exit  *execExit     `json:"exit,omitempty"`
	Error string        `json:"error,omitempty"`
}

type execExit struct {
	Code int32 `json:"code"`
}

// execSession implements orchard.ExecSession over the raw exec WebSocket connection.
type execSession struct {
	conn *websocket.Conn
	pr   *io.PipeReader
	pw   *io.PipeWriter
	done chan struct{}

	exitCode int
	exitErr  error
}

func newExecSession(ctx context.Context, conn *websocket.Conn, stdin io.Reader) *execSession {
	pr, pw := io.Pipe()

	s := &execSession{
		conn: conn,
		pr:   pr,
		pw:   pw,
		done: make(chan struct{}),
	}

	go s.readLoop(ctx)

	if stdin != nil {
		go s.writeStdin(ctx, stdin)
	}

	return s
}

func (s *execSession) Output() io.Reader {
	return s.pr
}

func (s *execSession) Wait(ctx context.Context) (int, error) {
	select {
	case <-s.done:
		return s.exitCode, s.exitErr
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (s *execSession) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}

func (s *execSession) readLoop(ctx context.Context) {
	defer close(s.done)

	for {
		msgType, data, err := s.conn.Read(ctx)
		if err != nil {
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Code == websocket.StatusNormalClosure {
				err = nil
			}

			s.exitErr = err
			_ = s.pw.CloseWithError(err)

			return
		}

		if msgType != websocket.MessageText {
			continue
		}

		var frame execFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}

		switch frame.Type {
		case execFrameStdout, execFrameStderr:
			if _, werr := s.pw.Write(frame.Data); werr != nil {
				return
			}
		case execFrameExit:
			if frame.Exit != nil {
				s.exitCode = int(frame.Exit.Code)
			}

			_ = s.pw.Close()

			return
		case execFrameError:
			s.exitErr = errors.New(frame.Error)
			_ = s.pw.CloseWithError(s.exitErr)

			return
		case execFrameResize, execFrameStdin:
			// Server-originated frames never carry these; ignore.
		}
	}
}

func (s *execSession) writeStdin(ctx context.Context, r io.Reader) {
	buf := make([]byte, 4096)

	for {
		n, err := r.Read(buf)
		if n > 0 {
			frame := execFrame{Type: execFrameStdin, Data: append([]byte(nil), buf[:n]...)}

			payload, merr := json.Marshal(frame)
			if merr == nil {
				if werr := s.conn.Write(ctx, websocket.MessageText, payload); werr != nil {
					return
				}
			}
		}

		if err != nil {
			return
		}
	}
}
