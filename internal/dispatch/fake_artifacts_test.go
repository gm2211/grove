package dispatch

import (
	"context"
	"io"

	"github.com/gm2211/grove/internal/artifacts"
)

// fakeArtifacts is a minimal in-memory artifacts.Client used by dispatch package tests.
type fakeArtifacts struct {
	objectsByPrefix map[string][]artifacts.Object
	listErr         error

	listCalls []string
}

func (f *fakeArtifacts) List(ctx context.Context, prefix string) ([]artifacts.Object, error) {
	f.listCalls = append(f.listCalls, prefix)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.objectsByPrefix[prefix], nil
}

func (f *fakeArtifacts) Get(ctx context.Context, key string) (io.ReadCloser, artifacts.Object, error) {
	return nil, artifacts.Object{}, artifacts.ErrNotFound
}
