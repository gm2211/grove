package dispatch

import (
	"context"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/config"
)

// bucketFake is an in-memory bucket keyed by full object key, with prefix deletes.
type bucketFake struct {
	objects map[string]time.Time // key -> last modified
	deleted []string
}

func (b *bucketFake) List(_ context.Context, prefix string) ([]artifacts.Object, error) {
	var out []artifacts.Object
	for key, mod := range b.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, artifacts.Object{Path: strings.TrimPrefix(key, prefix), ModTime: mod})
		}
	}
	return out, nil
}

func (b *bucketFake) Get(context.Context, string) (io.ReadCloser, artifacts.Object, error) {
	return nil, artifacts.Object{}, artifacts.ErrNotFound
}

func (b *bucketFake) DeletePrefix(_ context.Context, prefix string) error {
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

	bucket := &bucketFake{objects: map[string]time.Time{
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
	svc, err := New(&fakeNomad{}, Options{
		StorePath:         filepath.Join(t.TempDir(), "jobs.json"),
		Artifacts:         bucket,
		ArtifactRetention: config.ArtifactRetention{}, // defaults: 15m success, 1h failure
	})
	if err != nil {
		t.Fatal(err)
	}
	s := svc.(*service)
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

	sort.Strings(bucket.deleted)
	want := []string{"jobs/fail-old/", "jobs/ok-old/", "jobs/orphan-old/"}
	if strings.Join(bucket.deleted, ",") != strings.Join(want, ",") {
		t.Fatalf("deleted %v, want %v", bucket.deleted, want)
	}
	if _, ok := bucket.objects["elsewhere/not-a-job.txt"]; !ok {
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
	bucket.deleted = nil
	s.expireArtifacts(context.Background(), now)
	if len(bucket.deleted) != 0 {
		t.Errorf("second sweep deleted %v", bucket.deleted)
	}
}

func TestExpireArtifactsCustomRetention(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-20 * time.Minute)
	bucket := &bucketFake{objects: map[string]time.Time{"jobs/j/a": finished}}
	svc, err := New(&fakeNomad{}, Options{
		StorePath:         filepath.Join(t.TempDir(), "jobs.json"),
		Artifacts:         bucket,
		ArtifactRetention: config.ArtifactRetention{Success: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := svc.(*service)
	if err := s.store.put(&record{Job: Job{ID: "j", Status: StatusSuccess, FinishedAt: &finished}}); err != nil {
		t.Fatal(err)
	}
	s.expireArtifacts(context.Background(), now)
	if len(bucket.deleted) != 0 {
		t.Errorf("deleted %v before the configured hour", bucket.deleted)
	}
}

func TestExpireArtifactsWithoutDeleterIsNoop(t *testing.T) {
	svc, err := New(&fakeNomad{}, Options{
		StorePath: filepath.Join(t.TempDir(), "jobs.json"),
		Artifacts: &fakeArtifacts{},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.(*service).expireArtifacts(context.Background(), time.Now())
}
