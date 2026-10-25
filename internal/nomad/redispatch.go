package nomad

import (
	"context"
	"fmt"
)

// DispatchInputs reads back what a dispatched job was dispatched with, so it can be dispatched
// again from another parameterized job (grove's Linux-to-macOS fallback) without grove ever
// keeping the payload or the unredacted env itself. The concrete Client returned by New implements
// it; see AsDispatchInputs.
type DispatchInputs interface {
	// DispatchInput returns the dispatch meta and payload of the dispatched job dispatchedJobID.
	DispatchInput(ctx context.Context, dispatchedJobID string) (meta map[string]string, payload []byte, err error)
}

// AsDispatchInputs returns c's DispatchInputs implementation, if it has one.
func AsDispatchInputs(c Client) (DispatchInputs, bool) {
	d, ok := c.(DispatchInputs)
	return d, ok
}

func (c *client) DispatchInput(ctx context.Context, dispatchedJobID string) (map[string]string, []byte, error) {
	job, _, err := c.raw.Jobs().Info(dispatchedJobID, qOpts(ctx))
	if err != nil {
		return nil, nil, fmt.Errorf("nomad: read dispatched job %s: %w", dispatchedJobID, wrapNotFound(err))
	}
	if job == nil || job.ParentID == nil || *job.ParentID == "" {
		return nil, nil, fmt.Errorf("nomad: %s is not a dispatched job", dispatchedJobID)
	}
	return job.Meta, job.Payload, nil
}
