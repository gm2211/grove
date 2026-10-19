package artifacts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/config"
)

func newTestStore(t *testing.T) *FSStore {
	t.Helper()
	s, err := NewFS(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return s
}

func put(t *testing.T, s *FSStore, key, body string) {
	t.Helper()
	if err := s.Put(context.Background(), key, strings.NewReader(body), int64(len(body)), ""); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

func TestFSStore_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	put(t, s, "jobs/j1/dist/app.json", `{"ok":true}`)
	put(t, s, "jobs/j1/readme", "hello")

	rc, obj, err := s.Get(ctx, "jobs/j1/dist/app.json")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != `{"ok":true}` || obj.Size != int64(len(body)) || obj.ContentType != "application/json" {
		t.Errorf("Get = %q %+v", body, obj)
	}
	if obj.ModTime.IsZero() {
		t.Error("Get should report a ModTime")
	}

	objs, err := s.List(ctx, "jobs/j1/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 2 || objs[0].Path != "dist/app.json" || objs[1].Path != "readme" || objs[1].ContentType != "application/octet-stream" {
		t.Errorf("List = %+v", objs)
	}

	// Overwrite replaces the whole file.
	put(t, s, "jobs/j1/readme", "hi")
	rc, obj, err = s.Get(ctx, "jobs/j1/readme")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(rc)
	rc.Close()
	if string(body) != "hi" || obj.Size != 2 {
		t.Errorf("after overwrite Get = %q %+v", body, obj)
	}

	if _, _, err := s.Get(ctx, "jobs/j1/missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Get(ctx, "jobs/j1/readme/below-a-file"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get below a file err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Get(ctx, "jobs/j1/dist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a directory err = %v, want ErrNotFound", err)
	}
	if objs, err := s.List(ctx, "jobs/none/"); err != nil || len(objs) != 0 {
		t.Errorf("List of an absent prefix = %v, %v; want empty, nil", objs, err)
	}
}

func TestFSStore_PutSizeMismatchLeavesNothing(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(context.Background(), "jobs/j/short", strings.NewReader("abc"), 10, "")
	if err == nil {
		t.Fatal("expected an error for a short body")
	}
	if _, _, err := s.Get(context.Background(), "jobs/j/short"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a failed Put must not leave a file: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Dir(), tmpDir))
	if len(entries) != 0 {
		t.Errorf("a failed Put must not leave a staged file, found %d", len(entries))
	}
	// size < 0 means "until EOF"; size limits reading to exactly that many bytes.
	if err := s.Put(context.Background(), "jobs/j/eof", strings.NewReader("abcdef"), -1, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "jobs/j/lim", strings.NewReader("abcdef"), 3, ""); err != nil {
		t.Fatal(err)
	}
	objs, _ := s.List(context.Background(), "jobs/j/")
	if len(objs) != 2 || objs[0].Size != 6 || objs[1].Size != 3 {
		t.Errorf("List = %+v", objs)
	}
}

func TestFSStore_RejectsTraversal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	bad := []string{
		"", "/etc/passwd", "../outside", "jobs/../../outside", "jobs/j/../../../x", "jobs//x",
		"jobs/./x", "jobs/x/", ".", "..", "jobs/x\\..\\..\\y", "jobs/\x00", ".tmp/put-x", "jobs/j/..",
	}
	for _, key := range bad {
		if err := s.Put(ctx, key, strings.NewReader("x"), 1, ""); err == nil {
			t.Errorf("Put(%q) succeeded, want rejection", key)
		}
		if _, _, err := s.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) succeeded, want rejection", key)
		}
	}
	for _, prefix := range []string{"", "/", "../", "jobs/../../", "/etc/", "jobs//"} {
		if err := s.DeletePrefix(ctx, prefix); err == nil {
			t.Errorf("DeletePrefix(%q) succeeded, want rejection", prefix)
		}
	}
	for _, prefix := range []string{"../", "jobs/../../x", "/etc/"} {
		if _, err := s.List(ctx, prefix); err == nil {
			t.Errorf("List(%q) succeeded, want rejection", prefix)
		}
	}
	// Nothing escaped next to the root.
	entries, _ := os.ReadDir(filepath.Dir(s.Dir()))
	if len(entries) != 1 {
		t.Errorf("expected only the store dir next to it, found %v", entries)
	}
}

func TestFSStore_DoesNotFollowSymlinksOut(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("do not serve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(), "jobs", "j"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A symlinked file and a symlinked directory pointing outside the root.
	if err := os.Symlink(secret, filepath.Join(s.Dir(), "jobs", "j", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.Dir(), "jobs", "evil")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Get(ctx, "jobs/j/link"); err == nil {
		t.Error("Get through a symlinked file must fail")
	}
	if _, _, err := s.Get(ctx, "jobs/evil/secret"); err == nil {
		t.Error("Get through a symlinked directory out of the root must fail")
	}
	if err := s.Put(ctx, "jobs/evil/planted", strings.NewReader("x"), 1, ""); err == nil {
		t.Error("Put through a symlinked directory out of the root must fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted")); !os.IsNotExist(err) {
		t.Error("Put wrote outside the root")
	}
	objs, err := s.List(ctx, "jobs/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 0 {
		t.Errorf("List must skip symlinks, got %+v", objs)
	}
	if err := s.DeletePrefix(ctx, "jobs/evil/"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("DeletePrefix followed a symlink out of the root: %v", err)
	}
}

func TestFSStore_DeletePrefix(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	put(t, s, "jobs/a/x", "1")
	put(t, s, "jobs/a/sub/y", "2")
	put(t, s, "jobs/ab/z", "3")
	put(t, s, "jobs/b/w", "4")
	put(t, s, "elsewhere/keep", "5")

	if err := s.DeletePrefix(ctx, "jobs/a/"); err != nil {
		t.Fatal(err)
	}
	objs, _ := s.List(ctx, "")
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Path)
	}
	if got := strings.Join(keys, ","); got != "elsewhere/keep,jobs/ab/z,jobs/b/w" {
		t.Errorf("after DeletePrefix(jobs/a/) keys = %s", got)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "jobs", "a")); !os.IsNotExist(err) {
		t.Error("the job's directory should be gone")
	}

	// A prefix that isn't a directory boundary matches like an object-store prefix.
	if err := s.DeletePrefix(ctx, "jobs/a"); err != nil {
		t.Fatal(err)
	}
	objs, _ = s.List(ctx, "jobs/")
	if len(objs) != 1 || objs[0].Path != "b/w" {
		t.Errorf("after DeletePrefix(jobs/a) = %+v", objs)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "jobs", "ab")); !os.IsNotExist(err) {
		t.Error("emptied directories should be pruned")
	}
	// Deleting what isn't there is not an error.
	if err := s.DeletePrefix(ctx, "jobs/none/"); err != nil {
		t.Errorf("DeletePrefix of an absent prefix: %v", err)
	}
}

func TestFSStore_ListReportsModTimes(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "jobs/old/a", "1")
	put(t, s, "jobs/new/b", "2")
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(s.Dir(), "jobs", "old", "a"), old, old); err != nil {
		t.Fatal(err)
	}
	objs, err := s.List(context.Background(), "jobs/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].Path != "new/b" || objs[1].Path != "old/a" {
		t.Fatalf("List = %+v", objs)
	}
	if !objs[1].ModTime.Equal(old) {
		t.Errorf("old ModTime = %v, want %v", objs[1].ModTime, old)
	}
	if time.Since(objs[0].ModTime) > time.Minute {
		t.Errorf("new ModTime = %v, want about now", objs[0].ModTime)
	}
}

func TestFSStore_Permissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	s := newTestStore(t)
	put(t, s, "jobs/j/deep/file", "x")
	err := filepath.WalkDir(s.Dir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", p, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFSStore_ConcurrentPutsAreAtomic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := bytes.Repeat([]byte("a"), 1<<16)
	b := bytes.Repeat([]byte("b"), 1<<16)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := a
			if i%2 == 1 {
				body = b
			}
			if err := s.Put(ctx, "jobs/j/f", bytes.NewReader(body), int64(len(body)), ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	rc, _, err := s.Get(ctx, "jobs/j/f")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
		t.Error("concurrent Puts produced a mixed file")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Dir(), tmpDir))
	if len(entries) != 0 {
		t.Errorf("staged files left behind: %d", len(entries))
	}
}

func TestNewFS_SweepsStaleStagedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(filepath.Join(dir, tmpDir), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, tmpDir, "put-stale")
	fresh := filepath.Join(dir, tmpDir, "put-fresh")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-2 * staleTmpAge)
	if err := os.Chtimes(stale, long, long); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFS(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale staged file should be removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent staged file may be an upload in progress and must be kept")
	}
}

func TestNew_DirAndDefault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custom")
	c, err := New(config.ArtifactsConfig{Dir: dir, Endpoint: "http://legacy:9000", Bucket: "grove"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.(*FSStore).Dir(); got != dir {
		t.Errorf("Dir = %q, want %q (legacy keys ignored)", got, dir)
	}

	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	c, err = New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("New default: %v", err)
	}
	if got, want := c.(*FSStore).Dir(), filepath.Join(state, "grove", "artifacts"); got != want {
		t.Errorf("default Dir = %q, want %q", got, want)
	}
	if info, err := os.Stat(filepath.Join(state, "grove", "artifacts")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("default dir should be created 0700: %v %v", info, err)
	}

	if _, err := NewFS("relative/dir"); err == nil {
		t.Error("NewFS should refuse a relative directory")
	}
}
