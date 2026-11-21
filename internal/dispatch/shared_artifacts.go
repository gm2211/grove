package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/nomad"
)

// sharedArtifactsDir is where a shared pool's build or agent job leaves ./artifacts/**: the
// source task clones into $NOMAD_ALLOC_DIR/data/grove/work and main runs the script there.
// Nomad's fs API takes paths relative to the allocation directory, whose "alloc" is
// $NOMAD_ALLOC_DIR.
const sharedArtifactsDir = "alloc/data/grove/work/artifacts"

// Limits on what one shared-pool job may leave. The bytes are attacker-influenced and land on the
// control plane's disk, so they are bounded however the job lays them out.
const (
	sharedArtifactBytes   = 512 << 20
	sharedArtifactFiles   = 2000
	sharedArtifactDepth   = 16
	sharedArtifactTimeout = 2 * time.Minute
)

// collectSharedArtifacts copies a finished shared-pool job's ./artifacts/** from its allocation
// directory, through the Nomad servers, into the control plane's artifact store under the job's
// prefix. A sealed pool's `grove sandbox run` does this itself; a shared VM can't write to the
// store, so the control plane pulls instead. Only regular files are copied (never a symlink or
// anything else the listing doesn't call a plain file), under validated names, within the limits
// above. It is best effort: the job already ended on its own, so a failure is logged, never
// turned into a different job result.
func (s *service) collectSharedArtifacts(ctx context.Context, rec *record, job *Job) {
	if job.AllocID == "" || (job.Status != StatusSuccess && job.Status != StatusFailed) {
		return
	}
	if rec.Request.Kind != KindBuild && rec.Request.Kind != KindAgent {
		return
	}
	if s.pools[rec.Request.Pool].Isolated || s.pools[runPool(rec)].Isolated {
		return
	}
	fs, ok := s.nomad.(nomad.AllocFiles)
	if !ok {
		return
	}
	up, ok := s.artifacts.(artifacts.Uploader)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sharedArtifactTimeout)
	defer cancel()
	c := &sharedCollector{fs: fs, up: up, alloc: job.AllocID, prefix: artifactPrefix(rec.ID)}
	err := c.walk(ctx, sharedArtifactsDir, "", 0)
	switch {
	case err == nil:
		if c.files > 0 {
			slog.Info("dispatch: collected a shared-pool job's artifacts", "id", rec.ID, "files", c.files, "bytes", c.bytes)
		}
	case c.files == 0 && missingDir(err):
		// The job made no ./artifacts.
	default:
		slog.Warn("dispatch: collecting a shared-pool job's artifacts failed", "id", rec.ID, "alloc", job.AllocID, "files", c.files, "err", err)
	}
}

type sharedCollector struct {
	fs     nomad.AllocFiles
	up     artifacts.Uploader
	alloc  string
	prefix string
	files  int
	bytes  int64
}

func (c *sharedCollector) walk(ctx context.Context, dir, rel string, depth int) error {
	if depth > sharedArtifactDepth {
		return fmt.Errorf("artifacts nest deeper than %d directories", sharedArtifactDepth)
	}
	entries, err := c.fs.ListAllocDir(ctx, c.alloc, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !artifactName(e.Name) {
			continue
		}
		childDir, childRel := dir+"/"+e.Name, path.Join(rel, e.Name)
		if e.IsDir {
			if !strings.HasPrefix(e.Mode, "d") {
				continue
			}
			if err := c.walk(ctx, childDir, childRel, depth+1); err != nil {
				return err
			}
			continue
		}
		if !strings.HasPrefix(e.Mode, "-") || e.Size < 0 {
			continue // a symlink, device, pipe or socket: never followed
		}
		if c.files+1 > sharedArtifactFiles {
			return fmt.Errorf("more than %d artifact files", sharedArtifactFiles)
		}
		if c.bytes+e.Size > sharedArtifactBytes {
			return fmt.Errorf("artifacts exceed %d bytes", sharedArtifactBytes)
		}
		if err := c.copy(ctx, childDir, childRel, e.Size); err != nil {
			return err
		}
		c.files++
		c.bytes += e.Size
	}
	return nil
}

func (c *sharedCollector) copy(ctx context.Context, src, rel string, size int64) error {
	ct := mime.TypeByExtension(path.Ext(rel))
	if ct == "" {
		ct = "application/octet-stream"
	}
	if size == 0 {
		return c.up.Put(ctx, c.prefix+rel, strings.NewReader(""), 0, ct)
	}
	rc, err := c.fs.ReadAllocFile(ctx, c.alloc, src, size)
	if err != nil {
		return err
	}
	defer rc.Close()
	// The listing's size is what is stored: a file that changed since must not be cut short or
	// grow past the limit unnoticed.
	r := &exactReader{r: io.LimitReader(rc, size), want: size}
	if err := c.up.Put(ctx, c.prefix+rel, r, size, ct); err != nil {
		return err
	}
	return r.check()
}

// exactReader counts what it passes on, so a short read is an error rather than a truncated object.
type exactReader struct {
	r    io.Reader
	want int64
	n    int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.n += int64(n)
	return n, err
}

func (e *exactReader) check() error {
	if e.n != e.want {
		return fmt.Errorf("artifact changed while it was copied (%d of %d bytes)", e.n, e.want)
	}
	return nil
}

// artifactName is a single path component the store and the download API accept.
func artifactName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

func missingDir(err error) bool {
	return errors.Is(err, nomad.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "no such file")
}
