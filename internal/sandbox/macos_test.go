//go:build unix

package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

type macHarness struct {
	vm       *fakeMacVM
	orchard  *fakeOrchard
	runner   *Runner
	stdout   bytes.Buffer
	stderr   lockedBuffer
	uploader *fakeUploader
}

// lockedBuffer is a bytes.Buffer safe to read while the runner may still write grove's messages.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newMacHarness(t *testing.T) *macHarness {
	t.Helper()
	oldPoll, oldBackoff := macFollowPoll, macDeleteBackoff
	macFollowPoll, macDeleteBackoff = "0.05", time.Millisecond
	t.Cleanup(func() { macFollowPoll, macDeleteBackoff = oldPoll, oldBackoff })

	h := &macHarness{vm: newFakeMacVM(t), uploader: &fakeUploader{}}
	h.orchard = &fakeOrchard{
		vms: []orchard.VM{
			// Same pool but not sealed: must never get the job.
			{Name: "macos-mac1-1", Status: "running"},
			sealedVM("macos-mac1-0"),
		},
		mac: map[string]*fakeMacVM{"macos-mac1-0": h.vm},
	}
	h.runner = &Runner{
		Orchard:      h.orchard,
		Uploader:     h.uploader,
		Slots:        Slots{Dir: t.TempDir(), PerVM: 1},
		Stdout:       &h.stdout,
		Stderr:       &h.stderr,
		PollInterval: 5 * time.Millisecond,
		SlotWait:     5 * time.Millisecond,
		StopGrace:    time.Second,
	}
	return h
}

func macRequest(script string) Request {
	return Request{
		Kind: KindShell, Pool: "macos", OS: OSDarwin, AllocID: "alloc1", Image: "ghcr.io/gm2211/grove-runner:latest",
		Script: script,
		Env:    map[string]string{"FOO": "it's", "GH_TOKEN": "secret-token", "GIT_TOKEN": "other-secret", "bad-key": "x"},
	}
}

func TestRunMacOS_ShellJob(t *testing.T) {
	h := newMacHarness(t)
	script := `echo one
echo "warn" >&2
echo "cwd=$(pwd -P)"
echo "env=$FOO token=${GH_TOKEN:-none}/${GIT_TOKEN:-none} home=$HOME user=$USER tmp=$TMPDIR"
printf 'last'
exit 7
`
	code := h.runner.Run(context.Background(), macRequest(script))
	if code != 7 {
		t.Fatalf("Run = %d, want the job's own 7; stderr:\n%s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.HasPrefix(out, "one\ncwd=") || !strings.HasSuffix(out, "\nlast") || strings.Count(out, "one\n") != 1 {
		t.Errorf("stdout = %q, want each line once and the final unterminated line", out)
	}
	if !strings.Contains(out, "/private/var/grove-job/runs/alloc1/workspace\n") {
		t.Errorf("stdout = %q, want the job to run in its run directory's workspace", out)
	}
	if !strings.Contains(out, "env=it's token=none/none home=/private/var/grove-job user=_grovejob tmp=") ||
		!strings.Contains(out, "/private/var/grove-job/runs/alloc1/tmp\n") {
		t.Errorf("stdout = %q, want the caller's env without clone credentials, and the job user's HOME/USER/TMPDIR", out)
	}
	if strings.Count(h.stderr.String(), "warn\n") != 1 {
		t.Errorf("stderr = %q, want the job's stderr line once", h.stderr.String())
	}
	for _, want := range []string{"running in sealed VM macos-mac1-0 (pool macos)", "GH_TOKEN and GIT_TOKEN served only the checkout",
		"deleted macos-mac1-0"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr = %q, missing %q", h.stderr.String(), want)
		}
	}

	// What went into the VM: the bundle, with no credential anywhere.
	env := h.vm.pushed["grove/env.sh"]
	if !strings.Contains(env, `export FOO='it'\''s'`) || strings.Contains(env, "secret") || strings.Contains(env, "bad-key") {
		t.Errorf("env.sh = %q", env)
	}
	run := h.vm.pushed["grove/run.sh"]
	for _, want := range []string{`cd "$grove_run/workspace"`, `. "$grove_run/grove"/env.sh`, `exec bash -eo pipefail "$grove_run/grove"/script.sh`,
		"export PATH=" + macPath} {
		if !strings.Contains(run, want) {
			t.Errorf("run.sh missing %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "/workspace\n") || strings.Contains(run, " /grove/") {
		t.Errorf("run.sh uses the container paths:\n%s", run)
	}
	for _, c := range h.vm.commands {
		if strings.Contains(c, "secret") {
			t.Errorf("a command sent to the VM carries a credential: %s", c)
		}
	}

	// The job ran as the job user; grove's own steps ran as root.
	log := h.vm.sudoLog()
	if !strings.Contains(log, "-n -u _grovejob -H /usr/bin/env -i /bin/bash /") || !strings.Contains(log, "/private/var/grove-job/runs/alloc1/grove/run.sh") {
		t.Errorf("sudo calls:\n%s\nwant run.sh started as _grovejob", log)
	}
	if !strings.Contains(log, "-n -u _grovejob /bin/bash -c tar -x -f - -C ") {
		t.Errorf("sudo calls:\n%s\nwant the bundle unpacked as _grovejob", log)
	}

	// Afterwards: the VM is marked draining, emptied, and deleted, never reused.
	if _, err := os.Stat(h.vm.path("private/var/run/grove-draining")); err != nil {
		t.Errorf("draining marker: %v", err)
	}
	for _, dir := range []string{"private/var/grove-job/runs/alloc1", "private/var/grove-sandbox/alloc1"} {
		if _, err := os.Stat(h.vm.path(dir)); !os.IsNotExist(err) {
			t.Errorf("%s left behind (%v)", dir, err)
		}
	}
	if !h.orchard.wasDeleted("macos-mac1-0") || h.orchard.wasDeleted("macos-mac1-1") {
		t.Errorf("deleted %v, want only the job's VM", h.orchard.deleted)
	}
	if h.vm.ran("artifacts") {
		t.Error("a shell job's artifacts were collected")
	}
}

// TestRunMacOS_LogsResumeAfterDrop: the first follow attach drops halfway through its first chunk;
// the reattach (two seconds later, while the job still runs) resumes at the exact byte, so every
// line arrives once and whole.
func TestRunMacOS_LogsResumeAfterDrop(t *testing.T) {
	h := newMacHarness(t)
	h.vm.dropFirstFollow = true
	code := h.runner.Run(context.Background(), macRequest("echo first-line\nsleep 1.5\necho second-line\nsleep 1.5\necho third-line\n"))
	if code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderr.String())
	}
	if got := h.stdout.String(); got != "first-line\nsecond-line\nthird-line\n" {
		t.Errorf("stdout = %q", got)
	}
	if h.vm.follows < 2 {
		t.Errorf("follows = %d, want a reattach", h.vm.follows)
	}
}

func TestRunMacOS_ScriptExit125IsReportedAs1(t *testing.T) {
	h := newMacHarness(t)
	if code := h.runner.Run(context.Background(), macRequest("exit 125\n")); code != 1 {
		t.Fatalf("Run = %d, want 1", code)
	}
}

func TestRunMacOS_Timeout(t *testing.T) {
	h := newMacHarness(t)
	h.runner.TimeoutSlack = 10 * time.Millisecond
	req := macRequest("echo started\nsleep 30\n")
	req.Timeout = 10 * time.Millisecond

	start := time.Now()
	if code := h.runner.Run(context.Background(), req); code != ExitTimedOut {
		t.Fatalf("Run = %d, want ExitTimedOut; stderr:\n%s", code, h.stderr.String())
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("Run took %s: the job wasn't stopped", time.Since(start))
	}
	if !h.vm.ran("kill -s TERM") || !h.orchard.wasDeleted("macos-mac1-0") {
		t.Errorf("want the job stopped and its VM deleted; deleted %v", h.orchard.deleted)
	}
}

func TestRunMacOS_Cancel(t *testing.T) {
	h := newMacHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !h.vm.ran("nohup /bin/bash -c ") {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	if code := h.runner.Run(ctx, macRequest("trap 'exit 130' TERM\nwhile :; do sleep 0.05; done\n")); code != 130 {
		t.Fatalf("Run = %d, want the job's own 130 from its TERM trap; stderr:\n%s", code, h.stderr.String())
	}
	if !h.orchard.wasDeleted("macos-mac1-0") {
		t.Error("the VM was not deleted after a canceled job")
	}
}

func TestRunMacOS_LostVM(t *testing.T) {
	h := newMacHarness(t)
	h.vm.failAfterStart = true
	h.runner.MissedPolls = 3

	if code := h.runner.Run(context.Background(), macRequest("sleep 1\n")); code != ExitLost {
		t.Fatalf("Run = %d, want ExitLost; stderr:\n%s", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "lost the job") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	if !h.orchard.wasDeleted("macos-mac1-0") {
		t.Error("a lost VM must still be deleted, so it is never reused")
	}
}

func TestRunMacOS_BuildArtifacts(t *testing.T) {
	h := newMacHarness(t)
	req := macRequest(`mkdir -p artifacts/sub
printf hello > artifacts/out.txt
printf '{}' > artifacts/sub/b.json
ln -s /etc/passwd artifacts/link
`)
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
	if !strings.Contains(h.vm.sudoLog(), "-n -u _grovejob /bin/bash -c if [ ! -d ") {
		t.Errorf("artifacts must be read as the job user:\n%s", h.vm.sudoLog())
	}
}

func TestRunMacOS_BuildWithoutArtifacts(t *testing.T) {
	h := newMacHarness(t)
	req := macRequest("true\n")
	req.Kind = KindBuild
	req.ArtifactPrefix = "jobs/j1/"
	if code := h.runner.Run(context.Background(), req); code != 0 {
		t.Fatalf("Run = %d", code)
	}
	if strings.Contains(h.stderr.String(), "artifact") || len(h.uploader.objects) != 0 {
		t.Errorf("stderr = %q, want silence about a job with no artifacts", h.stderr.String())
	}
}

// TestRunMacOS_SkipsDrainingVM: a VM marked draining refuses the job untouched (and isn't
// deleted by this runner); Run takes the job to another sealed VM of the pool.
func TestRunMacOS_SkipsDrainingVM(t *testing.T) {
	h := newMacHarness(t)
	draining := newFakeMacVM(t)
	if err := os.WriteFile(draining.path("private/var/run/grove-draining"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := sealedVM("macos-mac0-0") // sorts first, so it is tried first
	old.UID = "uid-old"
	h.orchard.vms = append(h.orchard.vms, old)
	h.orchard.mac["macos-mac0-0"] = draining

	if code := h.runner.Run(context.Background(), macRequest("echo ok\n")); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderr.String())
	}
	if !draining.ran("if [ -e /private/var/run/grove-draining ]; then exit 75; fi;") {
		t.Fatalf("the draining VM was never tried: %v", draining.commands)
	}
	if _, err := os.Stat(draining.path("private/var/grove-sandbox")); !os.IsNotExist(err) {
		t.Errorf("the draining VM was touched (%v)", err)
	}
	if len(draining.commands) != 1 || h.orchard.wasDeleted("macos-mac0-0") {
		t.Errorf("draining VM: commands %v, deleted %v; want only the refused create", draining.commands, h.orchard.deleted)
	}
	if !strings.Contains(h.stderr.String(), "macos-mac0-0 is shutting down; picking another VM") || h.stdout.String() != "ok\n" {
		t.Errorf("stdout = %q, stderr = %q", h.stdout.String(), h.stderr.String())
	}
	if !h.orchard.wasDeleted("macos-mac1-0") {
		t.Error("the VM that ran the job was not deleted")
	}
}

// A VM whose startup script hasn't created the job user yet is reported lost, not a bad job.
func TestRunMacOS_NoJobUser(t *testing.T) {
	h := newMacHarness(t)
	sudo := filepath.Join(h.vm.bin, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\ncase \" $* \" in *\" -u \"*) echo 'sudo: unknown user _grovejob' >&2; exit 1;; esac\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := h.runner.Run(context.Background(), macRequest("true\n")); code != ExitLost {
		t.Fatalf("Run = %d, want ExitLost; stderr:\n%s", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "has no _grovejob account yet") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

// DeleteVM is retried; a VM that can't be deleted stays marked draining.
func TestRunMacOS_DeleteRetried(t *testing.T) {
	h := newMacHarness(t)
	h.orchard.deleteErr = 2
	if code := h.runner.Run(context.Background(), macRequest("true\n")); code != 0 {
		t.Fatalf("Run = %d", code)
	}
	if !h.orchard.wasDeleted("macos-mac1-0") {
		t.Errorf("VM not deleted after retries; stderr:\n%s", h.stderr.String())
	}

	h = newMacHarness(t)
	h.orchard.deleteErr = macDeleteAttempts
	if code := h.runner.Run(context.Background(), macRequest("true\n")); code != 0 {
		t.Fatalf("Run = %d", code)
	}
	if !strings.Contains(h.stderr.String(), "could not delete macos-mac1-0 after its job") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	if _, err := os.Stat(h.vm.path("private/var/run/grove-draining")); err != nil {
		t.Errorf("an undeletable VM must stay marked draining: %v", err)
	}
}

func TestOffsetStream_ResumesAtTheExactByte(t *testing.T) {
	var out bytes.Buffer
	s := &offsetStream{out: &out}
	s.Write([]byte("a\nbb"))
	if out.String() != "a\n" || s.next() != 5 {
		t.Fatalf("out = %q, next = %d", out.String(), s.next())
	}
	l := &macLogs{dir: "/c", stdout: s, stderr: &offsetStream{out: &out}}
	if cmd := l.command(true); !strings.Contains(cmd, "tail -c +5 -f /c/stdout") || !strings.Contains(cmd, "tail -c +1 -f /c/stderr >&2") {
		t.Fatalf("follow command = %s", cmd)
	}
	s.Write([]byte("b\nc"))
	l.flush()
	if out.String() != "a\nbbb\nc" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestRunScriptAt_MacOS(t *testing.T) {
	script := runScriptAt(macPaths, time.Minute, "main", true)
	for _, want := range []string{
		"grove_run=$(cd \"$(dirname \"$0\")/..\" && pwd -P) || exit 1\n",
		"export HOME=/private/var/grove-job USER=_grovejob LOGNAME=_grovejob\n",
		"cd \"$grove_run/workspace\"\nif ! git checkout --force --quiet 'main' --; then",
		"exec timeout --kill-after=10 60 bash -eo pipefail \"$grove_run/grove\"/script.sh\nfi\nexec bash -eo pipefail \"$grove_run/grove\"/script.sh\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("run.sh missing %q:\n%s", want, script)
		}
	}
}

// TestRunMacOS_UsedVMIsDeletedNotReused: a VM still holding an earlier job's control directory
// (its runner died before deleting it) is never given another job: it is marked draining and
// deleted, and the job goes to a fresh VM.
func TestRunMacOS_UsedVMIsDeletedNotReused(t *testing.T) {
	h := newMacHarness(t)
	used := newFakeMacVM(t)
	if err := os.MkdirAll(used.path("private/var/grove-sandbox/earlier-alloc"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.orchard.vms = append(h.orchard.vms, sealedVM("macos-mac0-0"))
	h.orchard.mac["macos-mac0-0"] = used

	if code := h.runner.Run(context.Background(), macRequest("echo ok\n")); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderr.String())
	}
	if !h.orchard.wasDeleted("macos-mac0-0") || !h.orchard.wasDeleted("macos-mac1-0") {
		t.Errorf("deleted %v, want both the used VM and the one that ran the job", h.orchard.deleted)
	}
	if _, err := os.Stat(used.path("private/var/run/grove-draining")); err != nil {
		t.Errorf("the used VM was not marked draining: %v", err)
	}
	if _, err := os.Stat(used.path("private/var/grove-sandbox/alloc1")); !os.IsNotExist(err) {
		t.Errorf("the job was prepared in the used VM (%v)", err)
	}
	if !strings.Contains(h.stderr.String(), "macos-mac0-0 already ran an earlier job") || h.stdout.String() != "ok\n" {
		t.Errorf("stdout = %q, stderr = %q", h.stdout.String(), h.stderr.String())
	}
}
