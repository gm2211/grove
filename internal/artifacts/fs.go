package artifacts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gm2211/grove/internal/config"
)

// tmpDir is the directory under the store's root that Put stages files in before renaming them
// into place. It is not a valid first key segment, so no key can ever name it.
const tmpDir = ".tmp"

// staleTmpAge is how old a staged file must be before NewFS treats it as left behind by a crashed
// writer and removes it.
const staleTmpAge = 24 * time.Hour

// FSStore is the artifact store: a directory on the control plane's disk. Only the control plane
// reads and writes it (`grove serve` lists, serves and expires artifacts; `grove sandbox run`
// uploads the artifacts it pulls out of a sealed VM), so there is no network service and no
// credential. Keys are "/"-separated paths below the root ("jobs/<id>/dist/app.tar.gz"); files
// are 0600, directories 0700, and every write lands by atomic rename, so a reader sees either the
// whole file or none of it.
//
// Every key is validated (no empty, "." or ".." segment, no absolute path, no backslash or NUL)
// and every file operation goes through an os.Root, so neither a crafted key nor a symlink placed
// inside the root can reach a file outside it. Get and List also ignore anything that is not a
// regular file, symlinks included.
type FSStore struct {
	dir string
}

var (
	_ Client   = (*FSStore)(nil)
	_ Uploader = (*FSStore)(nil)
	_ Deleter  = (*FSStore)(nil)
)

// DefaultDir is where the artifact store lives when config.ArtifactsConfig.Dir is empty: next to
// the job history, $XDG_STATE_HOME/grove/artifacts (default ~/.local/state/grove/artifacts).
func DefaultDir() (string, error) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "grove", "artifacts"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "grove", "artifacts"), nil
}

// New builds the artifact store from cfg: an FSStore rooted at cfg.Dir, or at DefaultDir when
// that's empty. It returns ErrNotConfigured (callers degrade gracefully: dispatch skips
// populating Job.Artifacts, the server's download handler returns 404) only when no directory can
// be determined at all. Legacy S3/MinIO keys in cfg are ignored.
func New(cfg config.ArtifactsConfig) (Client, error) {
	dir := cfg.Dir
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotConfigured, err)
		}
		dir = d
	}
	return NewFS(dir)
}

// NewFS opens (creating it 0700 if needed) the artifact store rooted at dir, which must be an
// absolute path, and removes staged files a crashed writer left behind.
func NewFS(dir string) (*FSStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("artifacts: store directory %q is not an absolute path", dir)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("artifacts: %w", err)
	}
	s := &FSStore{dir: dir}
	s.sweepTmp(time.Now())
	return s, nil
}

// Dir is the store's root directory.
func (s *FSStore) Dir() string { return s.dir }

func (s *FSStore) open() (*os.Root, error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("artifacts: open store %s: %w", s.dir, err)
	}
	return root, nil
}

// validKey reports whether key is a relative "/"-separated path naming something below the root:
// every segment non-empty and not "." or "..", no backslash or NUL, and not inside tmpDir.
func validKey(key string) bool {
	if key == "" || strings.ContainsAny(key, "\\\x00") {
		return false
	}
	segs := strings.Split(key, "/")
	if segs[0] == tmpDir {
		return false
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	// Belt and braces: the cleaned, joined path must stay under the root.
	return filepath.IsLocal(filepath.FromSlash(key))
}

// prefixDir splits a listing/deletion prefix into the directory it lies in ("" for the root) and
// validates that directory part. "jobs/abc/" → "jobs/abc"; "jobs/ab" → "jobs".
func prefixDir(prefix string) (string, error) {
	dir := ""
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		dir = prefix[:i]
	}
	if dir != "" && !validKey(dir) {
		return "", fmt.Errorf("artifacts: invalid prefix %q", prefix)
	}
	if dir == "" && strings.ContainsAny(prefix, "\\\x00") {
		return "", fmt.Errorf("artifacts: invalid prefix %q", prefix)
	}
	return dir, nil
}

// List returns every regular file whose key starts with prefix, Path relative to prefix, in
// lexical key order. A prefix with nothing under it yields an empty list, not an error.
func (s *FSStore) List(ctx context.Context, prefix string) ([]Object, error) {
	dir, err := prefixDir(prefix)
	if err != nil {
		return nil, err
	}
	root, err := s.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var out []Object
	err = s.walk(ctx, root, dir, func(key string, info fs.FileInfo) {
		if !strings.HasPrefix(key, prefix) {
			return
		}
		out = append(out, Object{
			Path:        strings.TrimPrefix(key, prefix),
			Size:        info.Size(),
			ContentType: contentTypeFor(key),
			ModTime:     info.ModTime(),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("artifacts: list %s: %w", prefix, err)
	}
	return out, nil
}

// walk calls fn for every regular file below dir ("" for the root), skipping tmpDir. It never
// follows a symlink.
func (s *FSStore) walk(ctx context.Context, root *os.Root, dir string, fn func(key string, info fs.FileInfo)) error {
	start := dir
	if start == "" {
		start = "."
	}
	err := fs.WalkDir(root.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == start && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			if p == tmpDir {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, sockets, devices: never part of the store
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while walking
			}
			return err
		}
		fn(p, info)
		return nil
	})
	return err
}

// Get opens one artifact for reading. key is the full key; it must name a regular file.
func (s *FSStore) Get(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	if !validKey(key) {
		return nil, Object{}, fmt.Errorf("artifacts: invalid key %q", key)
	}
	root, err := s.open()
	if err != nil {
		return nil, Object{}, err
	}
	defer root.Close()
	info, err := root.Lstat(filepath.FromSlash(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
			return nil, Object{}, ErrNotFound
		}
		return nil, Object{}, fmt.Errorf("artifacts: stat %s: %w", key, err)
	}
	if !info.Mode().IsRegular() {
		return nil, Object{}, ErrNotFound
	}
	f, err := root.Open(filepath.FromSlash(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, Object{}, ErrNotFound
		}
		return nil, Object{}, fmt.Errorf("artifacts: open %s: %w", key, err)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		if err == nil {
			return nil, Object{}, ErrNotFound
		}
		return nil, Object{}, fmt.Errorf("artifacts: stat %s: %w", key, err)
	}
	return f, Object{Path: key, Size: st.Size(), ContentType: contentTypeFor(key), ModTime: st.ModTime()}, nil
}

// Put stores the bytes of r at key: exactly size bytes when size >= 0 (fewer is an error), or
// everything up to EOF when size < 0. The file is written under tmpDir and renamed into place,
// replacing any existing file at key. contentType is not stored; Get and List derive it from the
// key's extension.
func (s *FSStore) Put(ctx context.Context, key string, r io.Reader, size int64, _ string) error {
	if !validKey(key) {
		return fmt.Errorf("artifacts: invalid key %q", key)
	}
	root, err := s.open()
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.FromSlash(key)
	if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return fmt.Errorf("artifacts: put %s: %w", key, err)
	}
	if err := root.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("artifacts: put %s: %w", key, err)
	}
	tmpName, f, err := createTmp(root)
	if err != nil {
		return fmt.Errorf("artifacts: put %s: %w", key, err)
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			_ = root.Remove(tmpName)
		}
	}()

	src := r
	if size >= 0 {
		src = io.LimitReader(r, size)
	}
	n, err := io.Copy(f, ctxReader{ctx: ctx, r: src})
	if err != nil {
		return fmt.Errorf("artifacts: put %s: %w", key, err)
	}
	if size >= 0 && n != size {
		return fmt.Errorf("artifacts: put %s: got %d bytes, want %d", key, n, size)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("artifacts: put %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("artifacts: put %s: %w", key, err)
	}
	if err := root.Rename(tmpName, name); err != nil {
		return fmt.Errorf("artifacts: put %s: %w", key, err)
	}
	ok = true
	return nil
}

// DeletePrefix removes every artifact whose key starts with prefix. A prefix ending in "/" removes
// that whole directory; any other prefix removes the matching files and then the directories it
// left empty. Deleting the whole store (an empty prefix) is refused.
func (s *FSStore) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" || prefix == "/" {
		return fmt.Errorf("artifacts: refusing to delete the whole store")
	}
	dir, err := prefixDir(prefix)
	if err != nil {
		return err
	}
	root, err := s.open()
	if err != nil {
		return err
	}
	defer root.Close()
	if strings.HasSuffix(prefix, "/") {
		if err := root.RemoveAll(filepath.FromSlash(dir)); err != nil {
			return fmt.Errorf("artifacts: delete %s: %w", prefix, err)
		}
		return nil
	}
	var keys []string
	if err := s.walk(ctx, root, dir, func(key string, _ fs.FileInfo) {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}); err != nil {
		return fmt.Errorf("artifacts: delete %s: %w", prefix, err)
	}
	for _, key := range keys {
		if err := root.Remove(filepath.FromSlash(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("artifacts: delete %s: %w", key, err)
		}
		// Prune the directories this left empty, up to (not including) dir. Remove fails on a
		// non-empty directory, which is where pruning stops.
		for p := path.Dir(key); p != "." && p != dir && strings.HasPrefix(p, dir); p = path.Dir(p) {
			if root.Remove(filepath.FromSlash(p)) != nil {
				break
			}
		}
	}
	return nil
}

// sweepTmp removes staged files older than staleTmpAge: a writer that crashed between create and
// rename. Best effort; younger ones may belong to an upload still in progress.
func (s *FSStore) sweepTmp(now time.Time) {
	root, err := s.open()
	if err != nil {
		return
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), tmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < staleTmpAge {
			continue
		}
		_ = root.RemoveAll(filepath.Join(tmpDir, e.Name()))
	}
}

func createTmp(root *os.Root) (string, *os.File, error) {
	for range 8 {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, err
		}
		name := filepath.Join(tmpDir, "put-"+hex.EncodeToString(b[:]))
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return name, f, err
	}
	return "", nil, errors.New("could not create a staging file")
}

func contentTypeFor(key string) string {
	if ct := mime.TypeByExtension(path.Ext(key)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// isNotDir reports whether err says a path component is not a directory ("jobs/x/file" when
// jobs/x is a file): for Get, that just means the key doesn't exist.
func isNotDir(err error) bool { return errors.Is(err, syscall.ENOTDIR) }

// ctxReader stops a copy once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
