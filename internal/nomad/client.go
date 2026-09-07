package nomad

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

// client is the concrete nomad.Client, wrapping github.com/hashicorp/nomad/api.
type client struct {
	raw *nomadapi.Client
}

// New constructs a Client talking to the Nomad server at url, authenticating with token (an ACL
// SecretID) when non-empty.
func New(url, token string) (Client, error) {
	cfg := nomadapi.DefaultConfig()
	cfg.Address = url

	if token != "" {
		cfg.SecretID = token
	}

	raw, err := nomadapi.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("nomad: %w", err)
	}

	return &client{raw: raw}, nil
}

func (c *client) Ping(ctx context.Context) error {
	// Status().Leader() has no context-aware variant in the Nomad API client; it issues a single
	// synchronous HTTP call bounded by the client's own configured timeout.
	_ = ctx

	if _, err := c.raw.Status().Leader(); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}

	return nil
}

func qOpts(ctx context.Context) *nomadapi.QueryOptions {
	return (&nomadapi.QueryOptions{}).WithContext(ctx)
}

func wOpts(ctx context.Context) *nomadapi.WriteOptions {
	return (&nomadapi.WriteOptions{}).WithContext(ctx)
}

func (c *client) ListNodes(ctx context.Context) ([]Node, error) {
	stubs, _, err := c.raw.Nodes().List(qOpts(ctx))
	if err != nil {
		return nil, err
	}

	out := make([]Node, 0, len(stubs))

	for _, stub := range stubs {
		node, _, err := c.raw.Nodes().Info(stub.ID, qOpts(ctx))
		if err != nil {
			return nil, fmt.Errorf("get node %s: %w", stub.ID, err)
		}

		allocs, _, err := c.raw.Nodes().Allocations(stub.ID, qOpts(ctx))
		if err != nil {
			return nil, fmt.Errorf("list allocations for node %s: %w", stub.ID, err)
		}

		out = append(out, nodeFromAPI(node, runningAllocCount(allocs)))
	}

	return out, nil
}

func (c *client) GetNode(ctx context.Context, id string) (*Node, error) {
	n, _, err := c.raw.Nodes().Info(id, qOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", id, err)
	}
	// runningAllocs isn't needed for placement lookups (the only current caller) — 0 avoids an
	// extra API call; ListNodes still computes it properly for the /fleet view.
	node := nodeFromAPI(n, 0)
	return &node, nil
}

func runningAllocCount(allocs []*nomadapi.Allocation) int {
	running := 0

	for _, a := range allocs {
		if a.ClientStatus == nomadapi.AllocClientStatusRunning {
			running++
		}
	}

	return running
}

func nodeFromAPI(n *nomadapi.Node, runningAllocs int) Node {
	var cpuMHz, memoryMiB int
	if n.NodeResources != nil {
		cpuMHz = int(n.NodeResources.Cpu.CpuShares)
		memoryMiB = int(n.NodeResources.Memory.MemoryMB)
	}

	return Node{
		ID:            n.ID,
		Name:          n.Name,
		Status:        n.Status,
		Eligibility:   n.SchedulingEligibility,
		Drain:         n.Drain,
		NodeClass:     n.NodeClass,
		Datacenter:    n.Datacenter,
		Attributes:    n.Attributes,
		Meta:          n.Meta,
		CPUMHz:        cpuMHz,
		MemoryMiB:     memoryMiB,
		RunningAllocs: runningAllocs,
	}
}

func (c *client) DrainNode(ctx context.Context, nodeID string, enable bool, deadline time.Duration) error {
	if !enable {
		_, err := c.raw.Nodes().UpdateDrain(nodeID, nil, true, wOpts(ctx))
		return err
	}

	spec := &nomadapi.DrainSpec{Deadline: deadline}

	_, err := c.raw.Nodes().UpdateDrain(nodeID, spec, false, wOpts(ctx))

	return err
}

func (c *client) Dispatch(
	ctx context.Context,
	parentJobID string,
	meta map[string]string,
	payload []byte,
) (*DispatchResult, error) {
	resp, _, err := c.raw.Jobs().Dispatch(parentJobID, meta, payload, "", wOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("dispatch %s: %w", parentJobID, err)
	}

	return &DispatchResult{
		DispatchedJobID: resp.DispatchedJobID,
		EvalID:          resp.EvalID,
	}, nil
}

func (c *client) ListAllocations(ctx context.Context, jobID string) ([]Allocation, error) {
	stubs, _, err := c.raw.Jobs().Allocations(jobID, false, qOpts(ctx))
	if err != nil {
		return nil, err
	}

	out := make([]Allocation, 0, len(stubs))
	for _, stub := range stubs {
		out = append(out, allocFromStub(stub))
	}

	return out, nil
}

func (c *client) GetAllocation(ctx context.Context, allocID string) (*Allocation, error) {
	a, _, err := c.raw.Allocations().Info(allocID, qOpts(ctx))
	if err != nil {
		return nil, err
	}

	out := allocFromFull(a)

	return &out, nil
}

// pickTask returns the (deterministic, lowest-sorting) task name and state out of an
// allocation's TaskStates. grove's parameterized jobs (nomad/jobs/*.hcl) run a single task per
// task group, so there's normally exactly one entry; when there's more than one this picks a
// stable one rather than an arbitrary map-iteration order.
func pickTask(states map[string]*nomadapi.TaskState) (string, *nomadapi.TaskState) {
	if len(states) == 0 {
		return "", nil
	}

	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}

	sort.Strings(names)

	return names[0], states[names[0]]
}

// terminalTaskDetails extracts an allocation's exit code, terminating signal (if any) and
// infra-fault failure reason (if any) from its task events. It replaces the separate
// per-call-site "walk Events backwards for the Terminated one" scan that allocFromStub and
// allocFromFull used to duplicate.
func terminalTaskDetails(ts *nomadapi.TaskState) (exitCode, signal *int, failureReason string) {
	if ts == nil {
		return nil, nil, ""
	}
	for i := len(ts.Events) - 1; i >= 0; i-- {
		ev := ts.Events[i]
		if ev.Type != "Terminated" {
			continue
		}
		code := ev.ExitCode
		exitCode = &code
		if ev.Signal != 0 {
			sig := ev.Signal
			signal = &sig
		}
		break
	}
	// Infra faults can show up on any event, not just the terminal one (e.g. a DriverError fires
	// before the task ever starts, so there's no "Terminated" event at all).
	for _, ev := range ts.Events {
		if ev.DriverError != "" {
			return exitCode, signal, ev.DriverError
		}
		if ev.SetupError != "" {
			return exitCode, signal, ev.SetupError
		}
		if ev.DownloadError != "" {
			return exitCode, signal, ev.DownloadError
		}
	}
	return exitCode, signal, ""
}

func finishedAt(ts *nomadapi.TaskState) *time.Time {
	if ts == nil || ts.FinishedAt.IsZero() {
		return nil
	}

	t := ts.FinishedAt

	return &t
}

func allocFromStub(a *nomadapi.AllocationListStub) Allocation {
	taskName, ts := pickTask(a.TaskStates)
	exitCode, signal, failureReason := terminalTaskDetails(ts)

	return Allocation{
		ID:            a.ID,
		JobID:         a.JobID,
		NodeID:        a.NodeID,
		NodeName:      a.NodeName,
		ClientStatus:  a.ClientStatus,
		TaskName:      taskName,
		ExitCode:      exitCode,
		CreatedAt:     time.Unix(0, a.CreateTime),
		FinishedAt:    finishedAt(ts),
		Signal:        signal,
		FailureReason: failureReason,
	}
}

func allocFromFull(a *nomadapi.Allocation) Allocation {
	taskName, ts := pickTask(a.TaskStates)
	exitCode, signal, failureReason := terminalTaskDetails(ts)

	return Allocation{
		ID:            a.ID,
		JobID:         a.JobID,
		NodeID:        a.NodeID,
		NodeName:      a.NodeName,
		ClientStatus:  a.ClientStatus,
		TaskName:      taskName,
		ExitCode:      exitCode,
		CreatedAt:     time.Unix(0, a.CreateTime),
		FinishedAt:    finishedAt(ts),
		Signal:        signal,
		FailureReason: failureReason,
	}
}

func (c *client) Logs(ctx context.Context, allocID, task, stream string, follow bool) (io.ReadCloser, error) {
	alloc, _, err := c.raw.Allocations().Info(allocID, qOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("get allocation %s: %w", allocID, err)
	}

	cancelCh := make(chan struct{})
	frames, errCh := c.raw.AllocFS().Logs(alloc, follow, task, stream, "start", 0, cancelCh, qOpts(ctx))

	pr, pw := io.Pipe()

	go func() {
		defer close(cancelCh)

		for {
			select {
			case <-ctx.Done():
				_ = pw.CloseWithError(ctx.Err())

				return
			case frame, ok := <-frames:
				if !ok {
					_ = pw.Close()

					return
				}

				if _, werr := pw.Write(frame.Data); werr != nil {
					return
				}
			case err, ok := <-errCh:
				if ok && err != nil {
					_ = pw.CloseWithError(err)

					return
				}
			}
		}
	}()

	return pr, nil
}

func (c *client) StopJob(ctx context.Context, jobID string, purge bool) error {
	_, _, err := c.raw.Jobs().Deregister(jobID, purge, wOpts(ctx))
	return err
}

func (c *client) RegisterJobFile(ctx context.Context, hcl string) error {
	job, err := c.raw.Jobs().ParseHCL(hcl, true)
	if err != nil {
		return fmt.Errorf("parse job HCL: %w", err)
	}

	if _, _, err := c.raw.Jobs().Register(job, wOpts(ctx)); err != nil {
		return fmt.Errorf("register job %s: %w", jobNameOf(job), err)
	}

	return nil
}

func jobNameOf(job *nomadapi.Job) string {
	if job == nil || job.ID == nil {
		return "<unknown>"
	}

	return *job.ID
}
