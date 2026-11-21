package dispatch

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/nomad"
)

// fsNomad is a fakeNomad whose allocations have directories, keyed by path relative to the
// allocation directory.
type fsNomad struct {
	*fakeNomad
	dirs  map[string][]nomad.AllocFile
	files map[string]string
	reads []string
	lists int
}

func (f *fsNomad) ListAllocDir(_ context.Context, _, path string) ([]nomad.AllocFile, error) {
	f.lists++
	entries, ok := f.dirs[path]
	if !ok {
		return nil, fmt.Errorf("Unexpected response code: 500 (open %s: no such file or directory)", path)
	}
	return entries, nil
}

func (f *fsNomad) ReadAllocFile(_ context.Context, _, path string, limit int64) (io.ReadCloser, error) {
	f.reads = append(f.reads, path)
	return io.NopCloser(io.LimitReader(strings.NewReader(f.files[path]), limit)), nil
}

func sharedArtifactsService(t *testing.T, nc nomad.Client, pools []PoolConfig) (Service, *artifacts.FSStore) {
	t.Helper()
	store, err := artifacts.NewFS(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	svc, err := New(nc, Options{
		StorePath: filepath.Join(t.TempDir(), "jobs.json"), StatusTTL: time.Millisecond,
		Artifacts: store, Pools: pools,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, store
}

func finishJob(nc *fakeNomad, code int) {
	jobID := nc.dispatchCalls[len(nc.dispatchCalls)-1].jobName + "/dispatch-1"
	nc.allocationsByJob = map[string][]nomad.Allocation{
		jobID: {{ID: "alloc-1", ClientStatus: "complete", ExitCode: &code, CreatedAt: time.Now()}},
	}
}

// A shared pool's VM can't write to the artifact store, so the control plane pulls an agent
// job's ./artifacts/** out of its allocation once it ends: regular files only, symlinks and odd
// names skipped, and the job's artifacts list says what came back.
func TestSharedPoolAgentJobArtifactsAreCollected(t *testing.T) {
	const dir = sharedArtifactsDir
	nc := &fsNomad{
		fakeNomad: &fakeNomad{},
		dirs: map[string][]nomad.AllocFile{
			dir: {
				{Name: "work.bundle.b64", Size: 6, Mode: "-rw-r--r--"},
				{Name: "work.none", Size: 0, Mode: "-rw-r--r--"},
				{Name: "sub", IsDir: true, Mode: "drwxr-xr-x"},
				{Name: "escape", Size: 20, Mode: "Lrwxr-xr-x"},
				{Name: "..", IsDir: true, Mode: "drwxr-xr-x"},
				{Name: "fifo", Mode: "prw-r--r--"},
			},
			dir + "/sub": {{Name: "log.txt", Size: 2, Mode: "-rw-r--r--"}},
		},
		files: map[string]string{dir + "/work.bundle.b64": "YnVuZA", dir + "/sub/log.txt": "ok"},
	}
	svc, store := sharedArtifactsService(t, nc, nil)
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindAgent, Pool: "macos", Repo: "https://example.com/r.git", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finishJob(nc.fakeNomad, 0)

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var paths []string
	for _, a := range got.Artifacts {
		paths = append(paths, a.Path)
	}
	sort.Strings(paths)
	if want := []string{"sub/log.txt", "work.bundle.b64", "work.none"}; strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("artifacts = %v, want %v", paths, want)
	}
	rc, _, err := store.Get(context.Background(), artifactPrefix(job.ID)+"work.bundle.b64")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "YnVuZA" {
		t.Fatalf("stored bundle = %q", b)
	}
	for _, r := range nc.reads {
		if strings.HasSuffix(r, "/escape") || strings.HasSuffix(r, "/fifo") || strings.Contains(r, "..") {
			t.Fatalf("read %s, which is not a regular file under artifacts", r)
		}
	}

	// Collected once: a terminal job's record is never walked again.
	lists := nc.lists
	if _, err := svc.Get(context.Background(), job.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if nc.lists != lists {
		t.Fatalf("a finished job's allocation was listed again")
	}
}

// A file that comes back shorter than its listing said is not stored as if it were whole.
func TestSharedPoolArtifactCutShortIsNotStored(t *testing.T) {
	const dir = sharedArtifactsDir
	nc := &fsNomad{
		fakeNomad: &fakeNomad{},
		dirs:      map[string][]nomad.AllocFile{dir: {{Name: "work.bundle.b64", Size: 100, Mode: "-rw-r--r--"}}},
		files:     map[string]string{dir + "/work.bundle.b64": "short"},
	}
	svc, _ := sharedArtifactsService(t, nc, nil)
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://example.com/r.git", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finishJob(nc.fakeNomad, 0)
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Artifacts) != 0 {
		t.Fatalf("a truncated artifact was stored: %+v", got.Artifacts)
	}
}

// No ./artifacts is no artifacts; a shell job and a sealed pool's job are never walked (the
// sandbox runner collects a sealed job's itself).
func TestSharedPoolArtifactsOnlyForSharedBuildAndAgentJobs(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  JobRequest
	}{
		{"shell", JobRequest{Kind: KindShell, Pool: "linux", Script: "true"}},
		{"sealed", JobRequest{Kind: KindBuild, Pool: "sealed", Repo: "https://example.com/r.git", Script: "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc := &fsNomad{fakeNomad: &fakeNomad{}, dirs: map[string][]nomad.AllocFile{
				sharedArtifactsDir: {{Name: "x", Size: 1, Mode: "-rw-r--r--"}},
			}}
			svc, _ := sharedArtifactsService(t, nc, []PoolConfig{{Name: "linux"}, {Name: "sealed", Isolated: true}})
			job, _, err := svc.Submit(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			finishJob(nc.fakeNomad, 0)
			if _, err := svc.Get(context.Background(), job.ID); err != nil {
				t.Fatalf("Get: %v", err)
			}
			if nc.lists != 0 {
				t.Fatalf("%s job's allocation was walked for artifacts", tc.name)
			}
		})
	}

	nc := &fsNomad{fakeNomad: &fakeNomad{}}
	svc, _ := sharedArtifactsService(t, nc, nil)
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://example.com/r.git", Script: "true"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finishJob(nc.fakeNomad, 1)
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed || len(got.Artifacts) != 0 {
		t.Fatalf("job with no ./artifacts = %s with %v", got.Status, got.Artifacts)
	}
}
