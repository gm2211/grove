// Package artifacts is grove's artifact store: a directory on the control plane's disk (FSStore)
// that build jobs' ./artifacts/** end up in, under "jobs/<id>/". Only the control plane touches it:
// `grove sandbox run` uploads what it pulls out of a VM, `grove serve` lists, serves and expires
// it. See docs/JOBS.md and ARCHITECTURE.md's API table.
package artifacts

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotConfigured is returned by New when no store directory can be determined (no
// config.ArtifactsConfig.Dir and no home directory to put the default under). Callers treat
// artifacts as optional and degrade gracefully.
var ErrNotConfigured = errors.New("artifacts: no artifact store configured (config.Artifacts is empty)")

// ErrNotFound is returned by Get when the key doesn't exist in the store.
var ErrNotFound = errors.New("artifacts: object not found")

// Object is one object under a queried prefix, path made relative to that prefix.
type Object struct {
	Path        string
	Size        int64
	ContentType string
	ModTime     time.Time
}

// Client is the seam between grove and the artifact store.
type Client interface {
	// List returns every object whose key starts with prefix, Path relative to prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Get streams one object's bytes; key is the full store key (not urlencoded, not relative).
	Get(ctx context.Context, key string) (io.ReadCloser, Object, error)
}

// Uploader writes objects into the artifact store. The Client New returns implements it; it is
// a separate interface so read-only users (and their fakes) don't need it. `grove sandbox run`
// uses it to store the build artifacts it copies out of a VM.
type Uploader interface {
	// Put stores size bytes from r at key (the full store key).
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
}

// Deleter removes objects from the artifact store. The Client New returns implements it; `grove
// serve` uses it to expire finished jobs' artifacts (see config.ArtifactRetention).
type Deleter interface {
	// DeletePrefix removes every object whose key starts with prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}
