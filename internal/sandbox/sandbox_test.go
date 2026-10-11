package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePayload(t *testing.T) {
	token, script := ParsePayload(KindBuild, []byte("grove-payload/1\ntok\necho a\necho b\n"))
	if token != "tok" || script != "echo a\necho b\n" {
		t.Fatalf("build payload = %q, %q", token, script)
	}
	token, script = ParsePayload(KindAgent, []byte("grove-payload/1\n\nrun\n"))
	if token != "" || script != "run\n" {
		t.Fatalf("agent payload without token = %q, %q", token, script)
	}
	token, script = ParsePayload(KindShell, []byte("grove-payload/1\ntok\nx"))
	if token != "" || script != "grove-payload/1\ntok\nx" {
		t.Fatalf("a shell payload is all script: %q, %q", token, script)
	}
}

func TestLogStream_DedupesReattachAndKeepsPartialLines(t *testing.T) {
	var out bytes.Buffer
	l := newLogStream(&out)
	l.Write([]byte("2026-10-11T12:00:00.000000001Z a\n2026-10-11T12:00:00.000000002Z b"))
	l.reset() // dropped mid-line: "b" must not be written half
	if out.String() != "a\n" {
		t.Fatalf("out = %q", out.String())
	}
	if got := since(l); got != "1791720000.000000002" {
		t.Fatalf("since = %q, want the dropped line's time", got)
	}
	l.Write([]byte("2026-10-11T12:00:00.000000001Z a\n2026-10-11T12:00:00.000000002Z bb\n2026-10-11T12:00:00.000000003Z c"))
	l.flush()
	if out.String() != "a\nbb\nc" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestSince_NothingYetMeansFromTheStart(t *testing.T) {
	a, b := newLogStream(&bytes.Buffer{}), newLogStream(&bytes.Buffer{})
	a.Write([]byte("2026-10-11T12:00:00Z x\n"))
	if got := since(a, b); got != "" {
		t.Fatalf("since = %q, want everything while one stream has had nothing", got)
	}
}

func TestSlots_SpreadAndCap(t *testing.T) {
	s := Slots{Dir: t.TempDir(), PerVM: 2}
	vms := []string{"linux-a-0", "linux-b-0"}
	var held []*Slot
	seen := map[string]int{}
	for range 4 {
		slot, err := s.TryAcquire(vms)
		if err != nil || slot == nil {
			t.Fatalf("TryAcquire = %v, %v", slot, err)
		}
		seen[slot.VM]++
		held = append(held, slot)
	}
	if seen["linux-a-0"] != 2 || seen["linux-b-0"] != 2 {
		t.Fatalf("slots per VM = %v, want 2 each", seen)
	}
	if slot, err := s.TryAcquire(vms); err != nil || slot != nil {
		t.Fatalf("TryAcquire with every slot taken = %v, %v", slot, err)
	}
	held[0].Release()
	slot, err := s.TryAcquire(vms)
	if err != nil || slot == nil || slot.VM != held[0].VM {
		t.Fatalf("TryAcquire after a release = %v, %v", slot, err)
	}
}

func TestClone_RefusesBeforeRunningGit(t *testing.T) {
	var log bytes.Buffer
	if code := Clone(context.Background(), "/nonexistent/git", "git@github.com:o/r.git", "", "", t.TempDir(), &log); code != cloneRefused {
		t.Fatalf("ssh remote: code %d", code)
	}
	if code := Clone(context.Background(), "/nonexistent/git", "https://github.com/o/r", "--upload-pack=x", "", t.TempDir(), &log); code != cloneRefused {
		t.Fatalf("option-like ref: code %d", code)
	}
}

// TestClone_TokenOnlyInEnvironment runs Clone against a stand-in git that records its argv and
// the credential it would hand to a credential helper.
func TestClone_TokenOnlyInEnvironment(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	git := filepath.Join(dir, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\nprintf 'token=%s\\n' \"$GROVE_CLONE_TOKEN\" >> " + record + "\n"
	if err := os.WriteFile(git, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if code := Clone(context.Background(), git, "https://github.com/o/r", "main", "s3cret", filepath.Join(dir, "work"), &log); code != 0 {
		t.Fatalf("code %d, log %s", code, log.String())
	}
	data, _ := os.ReadFile(record)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[1] != "token=s3cret" {
		t.Fatalf("calls = %q, want one git call with the token in its environment", data)
	}
	argv := lines[0]
	if strings.Contains(argv, "s3cret") {
		t.Errorf("token in argv: %s", argv)
	}
	if !strings.HasPrefix(argv, "-c credential.helper= -c core.hooksPath=/dev/null -c credential.helper=!f()") {
		t.Errorf("argv = %s, want helpers reset, hooks off, then grove's helper", argv)
	}
	// Nothing is checked out on the control plane: the job's container does that.
	if !strings.HasSuffix(argv, "clone --no-checkout --depth 50 -- https://github.com/o/r "+filepath.Join(dir, "work")) {
		t.Errorf("clone argv = %s", argv)
	}
}

func TestRunScript(t *testing.T) {
	got := runScript(90*time.Second, "", false)
	if !strings.Contains(got, "timeout --kill-after=10 90 bash") || !strings.Contains(got, ". /grove/env.sh") || strings.Contains(got, "git") {
		t.Fatalf("run.sh without a repo = %s", got)
	}
	got = runScript(time.Hour, "it's", true)
	checkout := strings.Index(got, `if ! git checkout --force --quiet 'it'\''s' --; then`)
	if checkout < 0 || checkout > strings.Index(got, ". /grove/env.sh") {
		t.Fatalf("run.sh with a repo must check out the quoted ref before the job's environment:\n%s", got)
	}
}

// TestRunScript_ChecksOutInTheContainer runs run.sh's checkout against a real repository fetched
// the way Clone does (--no-checkout), standing in for the job's container.
func TestRunScript_ChecksOutInTheContainer(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	src := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one"), 0o644)
	run(src, "add", ".")
	run(src, "commit", "-q", "-m", "one")
	work := filepath.Join(t.TempDir(), "work")
	run("", "clone", "-q", "--no-checkout", "file://"+src, work)
	if _, err := os.Stat(filepath.Join(work, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("a --no-checkout clone wrote a.txt (stat err %v)", err)
	}

	script := strings.Replace(runScript(time.Minute, "", true), "cd /workspace", "cd "+shQuote(work), 1)
	script = script[:strings.Index(script, ". /grove/env.sh")] + "exit 0\n"
	if out, err := exec.Command("/bin/bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("checkout: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(filepath.Join(work, "a.txt")); string(data) != "one" {
		t.Fatalf("a.txt = %q after the checkout", data)
	}

	script = strings.Replace(runScript(time.Minute, "no-such-ref", true), "cd /workspace", "cd "+shQuote(work), 1)
	cmd := exec.Command("/bin/bash", "-c", script)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(out), "grove: could not check out no-such-ref") {
		t.Fatalf("bad ref: %v\n%s", err, out)
	}
}
