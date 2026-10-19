//go:build !unix

package sandbox

import (
	"context"
	"errors"

	"github.com/gm2211/grove/internal/orchard"
)

// fakeMacVM needs process groups; see fake_macos_test.go.
type fakeMacVM struct{}

func (f *fakeMacVM) exec(context.Context, string, orchard.ExecOptions) (orchard.ExecSession, error) {
	return nil, errors.New("fakeMacVM needs a unix system")
}
