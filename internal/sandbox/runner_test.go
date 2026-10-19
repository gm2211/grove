package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
	"github.com/gm2211/grove/internal/orchard"
)

func sealedVM(name string) orchard.VM {
	return orchard.VM{Name: name, Status: "running", Softnet: true, SoftnetBlock: []string{"out @host"}}
}

type harness struct {
	vm       *fakeVM
	orchard  *fakeOrchard
	runner   *Runner
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	uploader *fakeUploader
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{vm: newFakeVM(), uploader: &fakeUploader{}}
	h.orchard = &fakeOrchard{
		vms: []orchard.VM{
			// Same pool but not sealed: must never get the job.
			{Name: "linux-mac1-0", Status: "running"},
			// Sealed, but another pool.
			sealedVM("other-mac1-0"),
			sealedVM("linux-mac1-1"),
		},
		docker: map[string]*fakeVM{"linux-mac1-1": h.vm},
	}
	h.runner = &Runner{
		Orchard:      h.orchard,
		Uploader:     h.uploader,
		Slots:        Slots{Dir: t.TempDir(), PerVM: 2},
		Stdout:       &h.stdout,
		Stderr:       &h.stderr,
		PollInterval: 5 * time.Millisecond,
		SlotWait:     5 * time.Millisecond,
		StopGrace:    time.Second,
	}
	return h
}

func shellRequest() Request {
	return Request{
		Kind: KindShell, Pool: "linux", AllocID: "alloc1", Image: "ghcr.io/gm2211/grove-runner:latest",
		CPU: 1000, Memory: 512, Script: "echo hi\n",
		Env: map[string]string{"FOO": "it's", "GH_TOKEN": "secret-token", "bad-key": "x"},
	}
}

func TestRun_ShellJobInSealedVM(t *testing.T) {
	h := newHarness(t)
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h.vm.logs = []fakeLog{
		{at: base, text: "one"},
		{at: base.Add(time.Millisecond), stderr: true, text: "warn"},
		{at: base.Add(2 * time.Millisecond), text: "two"},
		{at: base.Add(3 * time.Millisecond), text: "three"},
	}
	h.vm.dropFollow = 2 // the first attach drops halfway through "two"
	h.vm.runningInspects = 3
	h.vm.exitCode = 7

	code := h.runner.Run(context.Background(), shellRequest())
	if code != 7 {
		t.Fatalf("Run = %d, want the job's own 7; stderr:\n%s", code, h.stderr.String())
	}
	if got := h.stdout.String(); got != "one\ntwo\nthree\n" {
		t.Errorf("stdout = %q, want each line once, without docker's timestamps", got)
	}
	if !strings.Contains(h.stderr.String(), "warn\n") || strings.Count(h.stderr.String(), "warn") != 1 {
		t.Errorf("stderr = %q, want the job's stderr line once", h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "running in sealed VM linux-mac1-1 (pool linux)") {
		t.Errorf("stderr = %q, want the VM named", h.stderr.String())
	}

	if h.vm.pushed["grove/script.sh"] != "echo hi\n" {
		t.Errorf("script.sh = %q", h.vm.pushed["grove/script.sh"])
	}
	env := h.vm.pushed["grove/env.sh"]
	if !strings.Contains(env, `export FOO='it'\''s'`) {
		t.Errorf("env.sh = %q, want FOO exported and quoted", env)
	}
	if strings.Contains(env, "secret-token") || strings.Contains(env, "bad-key") {
		t.Errorf("env.sh = %q: the clone credential and unexportable keys must not enter the VM", env)
	}
	if !strings.Contains(h.vm.pushed["grove/run.sh"], "timeout --kill-after=10 3600 bash -eo pipefail /grove/script.sh") {
		t.Errorf("run.sh = %q, want the default timeout", h.vm.pushed["grove/run.sh"])
	}
	if _, ok := h.vm.pushed["workspace/"]; !ok {
		t.Errorf("pushed %v, want an (empty) workspace directory", h.vm.pushed)
	}

	create := ""
	for _, c := range h.vm.commands {
		if strings.Contains(c, "docker create ") {
			create = c
		}
	}
	for _, want := range []string{"--init", "--name grove-alloc1", "--cpus 0.50", "--memory 512m", "--label grove.pool=linux", "ghcr.io/gm2211/grove-runner:latest /grove/run.sh"} {
		if !strings.Contains(create, want) {
			t.Errorf("docker create = %q, missing %q", create, want)
		}
	}
	if !strings.HasPrefix(create, "if [ -e /run/grove-draining ]; then exit 75; fi; docker create ") {
		t.Errorf("docker create = %q, want it refused by a draining VM", create)
	}
	if strings.Contains(create, "docker.sock") {
		t.Errorf("docker create = %q mounts the docker socket", create)
	}
	if !h.vm.removed {
		t.Error("the job's container was not removed")
	}
	if h.vm.ran("docker cp grove-alloc1:/workspace/artifacts") {
		t.Error("a shell job's artifacts were collected")
	}
}

func TestRun_LostVM(t *testing.T) {
	h := newHarness(t)
	h.vm.runningInspects = 1 << 30
	h.vm.failInspects = 1 << 30
	h.runner.MissedPolls = 3

	if code := h.runner.Run(context.Background(), shellRequest()); code != ExitLost {
		t.Fatalf("Run = %d, want ExitLost", code)
	}
	if !strings.Contains(h.stderr.String(), "lost the job") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestRun_ScriptExit125IsReportedAs1(t *testing.T) {
	h := newHarness(t)
	h.vm.exitCode = ExitLost
	if code := h.runner.Run(context.Background(), shellRequest()); code != 1 {
		t.Fatalf("Run = %d, want 1", code)
	}
}

func TestRun_Timeout(t *testing.T) {
	h := newHarness(t)
	h.vm.runningInspects = 1 << 30
	h.vm.stopCode = 143
	h.runner.TimeoutSlack = time.Millisecond
	req := shellRequest()
	req.Timeout = time.Millisecond

	if code := h.runner.Run(context.Background(), req); code != ExitTimedOut {
		t.Fatalf("Run = %d, want ExitTimedOut", code)
	}
	if !h.vm.stopped {
		t.Error("the container was not stopped")
	}
}

func TestRun_Cancel(t *testing.T) {
	h := newHarness(t)
	h.vm.runningInspects = 1 << 30
	h.vm.stopCode = 130
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !h.vm.ran("docker start") {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	if code := h.runner.Run(ctx, shellRequest()); code != 130 {
		t.Fatalf("Run = %d, want the stopped container's 130", code)
	}
	if !h.vm.stopped || !h.vm.removed {
		t.Errorf("stopped=%v removed=%v, want both", h.vm.stopped, h.vm.removed)
	}
}

func TestRun_BuildArtifacts(t *testing.T) {
	h := newHarness(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name, body string, typ byte) {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Size: int64(len(body)), Mode: 0o644, Linkname: "/etc/passwd"})
		tw.Write([]byte(body))
	}
	add("artifacts/", "", tar.TypeDir)
	add("artifacts/out.txt", "hello", tar.TypeReg)
	add("artifacts/sub/b.json", "{}", tar.TypeReg)
	add("artifacts/../escape", "no", tar.TypeReg)
	add("artifacts/link", "", tar.TypeSymlink)
	tw.Close()
	h.vm.artifacts = buf.Bytes()

	req := shellRequest()
	req.Kind = KindBuild
	req.ArtifactPrefix = "jobs/j1/"
	if code := h.runner.Run(context.Background(), req); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderr.String())
	}
	want := map[string]string{"jobs/j1/out.txt": "hello", "jobs/j1/sub/b.json": "{}"}
	if len(h.uploader.objects) != len(want) {
		t.Fatalf("uploaded %v, want %v", h.uploader.objects, want)
	}
	for k, v := range want {
		if h.uploader.objects[k] != v {
			t.Errorf("object %s = %q, want %q", k, h.uploader.objects[k], v)
		}
	}
	if h.uploader.types["jobs/j1/out.txt"] != "text/plain; charset=utf-8" {
		t.Errorf("content type = %q", h.uploader.types["jobs/j1/out.txt"])
	}
}

func TestRun_BuildWithoutArtifacts(t *testing.T) {
	h := newHarness(t)
	req := shellRequest()
	req.Kind = KindBuild
	req.ArtifactPrefix = "jobs/j1/"
	if code := h.runner.Run(context.Background(), req); code != 0 {
		t.Fatalf("Run = %d", code)
	}
	if strings.Contains(h.stderr.String(), "artifact") {
		t.Errorf("stderr = %q, want silence about a job with no artifacts", h.stderr.String())
	}
}

func TestRun_RefusesBadRequest(t *testing.T) {
	h := newHarness(t)
	req := shellRequest()
	req.AllocID = "x; rm -rf /"
	if code := h.runner.Run(context.Background(), req); code != ExitSetup {
		t.Fatalf("Run = %d, want ExitSetup", code)
	}
	if len(h.vm.commands) != 0 {
		t.Errorf("ran %v in the VM for a refused request", h.vm.commands)
	}
}

func TestRun_WaitsForASealedVM(t *testing.T) {
	h := newHarness(t)
	h.orchard.vms = []orchard.VM{{Name: "linux-mac1-0", Status: "running"}}
	h.runner.NoVMTimeout = 20 * time.Millisecond
	if code := h.runner.Run(context.Background(), shellRequest()); code != ExitLost {
		t.Fatalf("Run = %d, want ExitLost when the pool has no sealed VM", code)
	}
	if !strings.Contains(h.stderr.String(), "no sealed VM of pool linux is running yet") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

// TestRun_SkipsDrainingVM: a VM whose shutdown script has started refuses the job, and Run takes
// it to another sealed VM of the pool.
func TestRun_SkipsDrainingVM(t *testing.T) {
	h := newHarness(t)
	draining := newFakeVM()
	draining.draining = true
	old := sealedVM("linux-mac0-0")
	old.UID = "uid-old"
	h.orchard.vms = append(h.orchard.vms, old)
	h.orchard.docker["linux-mac0-0"] = draining
	h.runner.Slots.PerVM = 1

	if code := h.runner.Run(context.Background(), shellRequest()); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderr.String())
	}
	if draining.created || !h.vm.created {
		t.Fatalf("created in draining VM = %v, in the other = %v", draining.created, h.vm.created)
	}
	if !strings.Contains(h.stderr.String(), "linux-mac0-0 is shutting down; picking another VM") {
		t.Errorf("stderr = %q", h.stderr.String())
	}

	// Only that VM is skipped: one recreated under the same name is used again.
	r := h.runner
	recreated := sealedVM("linux-mac0-0")
	recreated.UID = "uid-new"
	h.orchard.vms = []orchard.VM{recreated}
	slot, uid, _ := r.acquire(context.Background(), "linux", map[string]bool{"linux-mac0-0/uid-old": true})
	if slot == nil || slot.VM != "linux-mac0-0" || uid != "uid-new" {
		t.Fatalf("acquire = %v, %q; want the recreated VM", slot, uid)
	}
	slot.Release()
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"docker", "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", "it's"})
	want := `docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' 'it'\''s'`
	if got != want {
		t.Fatalf("shellJoin = %s, want %s", got, want)
	}
}

func TestArtifactPath(t *testing.T) {
	cases := map[string]string{
		"artifacts/a.txt":     "a.txt",
		"artifacts/d/e/f":     "d/e/f",
		"./artifacts/x":       "x",
		"artifacts/./y":       "y",
		"artifacts/":          "",
		"artifacts/../x":      "",
		"artifacts/d/../../x": "",
		"artifacts//etc/x":    "",
		"artifacts/a\\b":      "",
	}
	for in, want := range cases {
		got, ok := artifactPath(in)
		if got != want || ok != (want != "") {
			t.Errorf("artifactPath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// TestUploadTarIntoFSStore feeds a VM's artifacts tar through uploadTar into the real on-disk
// store: regular files land under the job's prefix, links and escaping names are skipped.
func TestUploadTarIntoFSStore(t *testing.T) {
	store, err := artifacts.NewFS(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(hdr *tar.Header, body string) {
		hdr.Size = int64(len(body))
		if hdr.Typeflag != tar.TypeReg {
			hdr.Size = 0
			body = ""
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Name: "artifacts/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "artifacts/dist/app.txt", Typeflag: tar.TypeReg, Mode: 0o644}, "app")
	add(&tar.Header{Name: "artifacts/../escape", Typeflag: tar.TypeReg, Mode: 0o644}, "no")
	add(&tar.Header{Name: "artifacts/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, "")
	add(&tar.Header{Name: "artifacts/w\\in", Typeflag: tar.TypeReg, Mode: 0o644}, "no")
	add(&tar.Header{Name: "artifacts/last.log", Typeflag: tar.TypeReg, Mode: 0o644}, "done")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	j := &job{r: &Runner{Uploader: store, ArtifactLimit: 1 << 20}, req: Request{ArtifactPrefix: "jobs/j1"}}
	n, err := j.uploadTar(&buf)
	if err != nil || n != 2 {
		t.Fatalf("uploadTar = %d, %v; want 2, nil", n, err)
	}
	objs, err := store.List(context.Background(), "jobs/j1/")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		got = append(got, o.Path)
	}
	if strings.Join(got, ",") != "dist/app.txt,last.log" {
		t.Errorf("stored %v", got)
	}
}
