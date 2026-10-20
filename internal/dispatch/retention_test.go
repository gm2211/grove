package dispatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/config"
)

// storeFake is an in-memory artifact store keyed by full key, with prefix deletes.
type storeFake struct {
	objects map[string]time.Time // key -> last modified
	deleted []string
}

func (b *storeFake) List(_ context.Context, prefix string) ([]artifacts.Object, error) {
	var out []artifacts.Object
	for key, mod := range b.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, artifacts.Object{Path: strings.TrimPrefix(key, prefix), ModTime: mod})
		}
	}
	return out, nil
}

func (b *storeFake) Get(context.Context, string) (io.ReadCloser, artifacts.Object, error) {
	return nil, artifacts.Object{}, artifacts.ErrNotFound
}

func (b *storeFake) DeletePrefix(_ context.Context, prefix string) error {
	b.deleted = append(b.deleted, prefix)
	for key := range b.objects {
		if strings.HasPrefix(key, prefix) {
			delete(b.objects, key)
		}
	}
	return nil
}

func TestExpireArtifacts(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }

	store := &storeFake{objects: map[string]time.Time{
		"jobs/ok-old/a.txt":       now.Add(-20 * time.Minute),
		"jobs/ok-new/a.txt":       now.Add(-5 * time.Minute),
		"jobs/fail-old/a.txt":     now.Add(-2 * time.Hour),
		"jobs/fail-new/a.txt":     now.Add(-30 * time.Minute),
		"jobs/running/a.txt":      now.Add(-3 * time.Hour),
		"jobs/orphan-old/x/y":     now.Add(-2 * time.Hour),
		"jobs/orphan-new/x/y":     now.Add(-10 * time.Minute),
		"jobs/ok-late-upload/a":   now.Add(-1 * time.Minute),
		"elsewhere/not-a-job.txt": now.Add(-48 * time.Hour),
	}}
	s := newTestServiceWith(t, newFakeExec(), Options{
		StorePath:         filepath.Join(t.TempDir(), "jobs.json"),
		Artifacts:         store,
		ArtifactRetention: config.ArtifactRetention{}, // defaults: 15m success, 1h failure
	})
	for _, r := range []record{
		{Job: Job{ID: "ok-old", Status: StatusSuccess, FinishedAt: ago(20 * time.Minute), Artifacts: []Artifact{{Path: "a.txt"}}}},
		{Job: Job{ID: "ok-new", Status: StatusSuccess, FinishedAt: ago(5 * time.Minute)}},
		{Job: Job{ID: "fail-old", Status: StatusFailed, FinishedAt: ago(2 * time.Hour)}},
		{Job: Job{ID: "fail-new", Status: StatusLost, FinishedAt: ago(30 * time.Minute)}},
		{Job: Job{ID: "running", Status: StatusRunning}},
		// Finished long ago but an object landed a minute ago: retention counts from the later.
		{Job: Job{ID: "ok-late-upload", Status: StatusSuccess, FinishedAt: ago(time.Hour)}},
	} {
		r := r
		if err := s.store.put(&r); err != nil {
			t.Fatal(err)
		}
	}

	s.expireArtifacts(context.Background(), now)

	sort.Strings(store.deleted)
	want := []string{"jobs/fail-old/", "jobs/ok-old/", "jobs/orphan-old/"}
	if strings.Join(store.deleted, ",") != strings.Join(want, ",") {
		t.Fatalf("deleted %v, want %v", store.deleted, want)
	}
	if _, ok := store.objects["elsewhere/not-a-job.txt"]; !ok {
		t.Error("an object outside jobs/ was deleted")
	}

	rec, _ := s.store.get("ok-old")
	if rec.ArtifactsExpiredAt == nil || !rec.ArtifactsExpiredAt.Equal(now) || len(rec.Artifacts) != 0 {
		t.Errorf("ok-old after expiry: expiredAt=%v artifacts=%v", rec.ArtifactsExpiredAt, rec.Artifacts)
	}
	if rec, _ := s.store.get("ok-new"); rec.ArtifactsExpiredAt != nil {
		t.Error("ok-new marked expired before its 15 minutes")
	}

	// A second sweep finds nothing left to do.
	store.deleted = nil
	s.expireArtifacts(context.Background(), now)
	if len(store.deleted) != 0 {
		t.Errorf("second sweep deleted %v", store.deleted)
	}
}

func TestExpireArtifactsCustomRetention(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-20 * time.Minute)
	store := &storeFake{objects: map[string]time.Time{"jobs/j/a": finished}}
	s := newTestServiceWith(t, newFakeExec(), Options{
		StorePath:         filepath.Join(t.TempDir(), "jobs.json"),
		Artifacts:         store,
		ArtifactRetention: config.ArtifactRetention{Success: time.Hour},
	})
	if err := s.store.put(&record{Job: Job{ID: "j", Status: StatusSuccess, FinishedAt: &finished}}); err != nil {
		t.Fatal(err)
	}
	s.expireArtifacts(context.Background(), now)
	if len(store.deleted) != 0 {
		t.Errorf("deleted %v before the configured hour", store.deleted)
	}
}

func TestExpireArtifactsWithoutDeleterIsNoop(t *testing.T) {
	s := newTestServiceWith(t, newFakeExec(), Options{
		StorePath: filepath.Join(t.TempDir(), "jobs.json"),
		Artifacts: &fakeArtifacts{},
	})
	s.expireArtifacts(context.Background(), time.Now())
}

// TestExpireArtifactsAgainstFSStore runs the same sweep against the real on-disk store: retention
// reads its ModTimes and deletes through it unchanged.
func TestExpireArtifactsAgainstFSStore(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	fsStore, err := artifacts.NewFS(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]time.Duration{
		"jobs/ok-old/dist/a.txt": 20 * time.Minute,
		"jobs/ok-new/a.txt":      5 * time.Minute,
		"jobs/fail-old/a.txt":    2 * time.Hour,
		"jobs/fail-new/a.txt":    30 * time.Minute,
		"jobs/running/a.txt":     3 * time.Hour,
		"jobs/orphan-old/x/y":    2 * time.Hour,
		"elsewhere/keep.txt":     48 * time.Hour,
	}
	for key, age := range files {
		if err := fsStore.Put(context.Background(), key, strings.NewReader("x"), 1, ""); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(-age)
		if err := os.Chtimes(filepath.Join(fsStore.Dir(), filepath.FromSlash(key)), mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	s := newTestServiceWith(t, newFakeExec(), Options{
		StorePath: filepath.Join(t.TempDir(), "jobs.json"),
		Artifacts: fsStore,
	})
	for _, r := range []record{
		{Job: Job{ID: "ok-old", Status: StatusSuccess, FinishedAt: ago(20 * time.Minute)}},
		{Job: Job{ID: "ok-new", Status: StatusSuccess, FinishedAt: ago(5 * time.Minute)}},
		{Job: Job{ID: "fail-old", Status: StatusFailed, FinishedAt: ago(2 * time.Hour)}},
		{Job: Job{ID: "fail-new", Status: StatusCanceled, FinishedAt: ago(30 * time.Minute)}},
		{Job: Job{ID: "running", Status: StatusRunning}},
	} {
		r := r
		if err := s.store.put(&r); err != nil {
			t.Fatal(err)
		}
	}

	s.expireArtifacts(context.Background(), now)

	objs, err := fsStore.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, o := range objs {
		left = append(left, o.Path)
	}
	want := "elsewhere/keep.txt,jobs/fail-new/a.txt,jobs/ok-new/a.txt,jobs/running/a.txt"
	if got := strings.Join(left, ","); got != want {
		t.Errorf("left after sweep: %s, want %s", got, want)
	}
	if rec, _ := s.store.get("ok-old"); rec.ArtifactsExpiredAt == nil {
		t.Error("ok-old should be marked expired")
	}
}
