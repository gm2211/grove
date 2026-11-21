package nomad

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// TokenDispatcher dispatches a parameterized job under an idempotency token, and finds and stops
// whatever copy a token produced. Grove uses its own job id as the token, so a Dispatch that
// errored can be followed up: an error does not prove Nomad created nothing (a timeout after
// Nomad committed the dispatch, or a commit that lands after the error). The concrete Client
// returned by New implements it; see AsTokenDispatcher. Spec S24 (spec/quint/lifecycle.qnt).
type TokenDispatcher interface {
	// DispatchWithToken is Client.Dispatch with Nomad's idempotency token set: while a copy
	// dispatched with token is not dead, dispatching again with it returns that copy.
	DispatchWithToken(ctx context.Context, parentJobID string, meta map[string]string, payload []byte, token string) (*DispatchResult, error)
	// StopDispatched stops and purges every copy of parentJobID dispatched with token, and
	// returns how many it stopped. No copy is not an error.
	StopDispatched(ctx context.Context, parentJobID, token string) (int, error)
}

// AsTokenDispatcher returns c's TokenDispatcher implementation, if it has one.
func AsTokenDispatcher(c Client) (TokenDispatcher, bool) {
	d, ok := c.(TokenDispatcher)
	return d, ok
}

func (c *client) DispatchWithToken(ctx context.Context, parentJobID string, meta map[string]string, payload []byte, token string) (*DispatchResult, error) {
	resp, _, err := c.raw.Jobs().Dispatch(parentJobID, meta, payload, token, wOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("dispatch %s: %w", parentJobID, err)
	}
	return &DispatchResult{DispatchedJobID: resp.DispatchedJobID, EvalID: resp.EvalID}, nil
}

func (c *client) StopDispatched(ctx context.Context, parentJobID, token string) (int, error) {
	if token == "" {
		return 0, errors.New("nomad: no idempotency token to look for")
	}
	q := qOpts(ctx)
	q.Prefix = parentJobID + "/dispatch-"
	stubs, _, err := c.raw.Jobs().List(q)
	if err != nil {
		return 0, fmt.Errorf("nomad: list copies of %s: %w", parentJobID, err)
	}
	stopped := 0
	var errs []error
	for _, st := range stubs {
		if st == nil || st.ParentID != parentJobID || !strings.HasPrefix(st.ID, parentJobID+"/dispatch-") {
			continue
		}
		job, _, err := c.raw.Jobs().Info(st.ID, qOpts(ctx))
		if err != nil {
			if errors.Is(wrapNotFound(err), ErrNotFound) {
				continue
			}
			errs = append(errs, fmt.Errorf("nomad: read %s: %w", st.ID, err))
			continue
		}
		if job == nil || job.DispatchIdempotencyToken == nil || *job.DispatchIdempotencyToken != token {
			continue
		}
		if err := c.StopJob(ctx, st.ID, true); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, fmt.Errorf("nomad: stop %s: %w", st.ID, err))
			continue
		}
		stopped++
	}
	return stopped, errors.Join(errs...)
}
