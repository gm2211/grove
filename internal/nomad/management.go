package nomad

import (
	"context"
	"errors"
)

// ManagementTokenHint is what to tell an operator whose Nomad refused a control-plane call.
const ManagementTokenHint = "the configured nomad.token is not a management token; set config.yaml nomad.token to a management token"

// CheckManagement probes a call only a management token may make (listing ACL tokens, which the
// fleet reconciler needs to enrol VMs and revoke their tokens). It returns nil when the call
// works or c cannot make it at all, an error wrapping ErrPermissionDenied on a 403, and any
// other error as is.
func CheckManagement(ctx context.Context, c Client) error {
	t, ok := c.(interface {
		ListNodeTokens(context.Context) ([]NodeToken, error)
	})
	if !ok {
		return nil
	}
	_, err := t.ListNodeTokens(ctx)
	return err
}

// IsPermissionDenied reports whether err is a Nomad 403.
func IsPermissionDenied(err error) bool { return errors.Is(err, ErrPermissionDenied) }
