// Package artifacts is grove's view of the artifact bucket (an S3-compatible store, typically
// MinIO on the control plane) that completed `build` jobs upload ./artifacts/** into (see
// docs/JOBS.md and nomad/jobs/build.nomad.hcl's run.sh). See ARCHITECTURE.md's API table.
package artifacts

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotConfigured is returned by New when cfg has no endpoint/bucket configured — grove treats
// artifacts as an optional feature, not every deployment runs a bucket.
var ErrNotConfigured = errors.New("artifacts: no artifact store configured (config.Artifacts is empty)")

// ErrNotFound is returned by Get when the key doesn't exist in the bucket.
var ErrNotFound = errors.New("artifacts: object not found")

// Object is one object under a queried prefix, path made relative to that prefix.
type Object struct {
	Path        string
	Size        int64
	ContentType string
	ModTime     time.Time
}

// Client is the seam between grove and the artifact bucket.
type Client interface {
	// List returns every object whose key starts with prefix, Path relative to prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Get streams one object's bytes; key is the full bucket key (not urlencoded, not relative).
	Get(ctx context.Context, key string) (io.ReadCloser, Object, error)
}
