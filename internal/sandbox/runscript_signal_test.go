package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// runRunScript runs run.sh for script with the given time limit, sends its process group TERM
// after cancelAfter (0 = never) the way a sealed macOS VM's stop does, and returns its exit status.
func runRunScript(t *testing.T, limit, cancelAfter time.Duration, script string) int {
	t.Helper()
	dir := t.TempDir()
	grove := filepath.Join(dir, "grove")
	ws := filepath.Join(dir, "workspace")
	for _, d := range []string{grove, ws} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths := jobPaths{workspace: shQuote(ws), grove: shQuote(grove)}
	for name, body := range map[string]string{
		"run.sh": runScriptAt(paths, limit, "", false), "env.sh": "", "script.sh": script,
	} {
		if err := os.WriteFile(filepath.Join(grove, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command(bash, filepath.Join(grove, "run.sh"))
	cmd.Env = []string{"PATH=" + filepath.Dir(bash) + ":/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if cancelAfter > 0 {
		time.AfterFunc(cancelAfter, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) })
	}
	err = cmd.Wait()

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0
}

// run.sh keeps the time limit itself, so a cancel reports the job's own status and an expiry 124
// whatever timeout(1) the machine has, if any (uutils' exits 124 when TERMed, or dies and leaves
// the job running).
func TestRunScript_CancelAndTimeoutStatus(t *testing.T) {
	const trapped = "trap 'exit 130' TERM\nwhile :; do sleep 0.05; done\n"
	for i := 0; i < 3; i++ {
		if code := runRunScript(t, time.Hour, 300*time.Millisecond, trapped); code != 130 {
			t.Errorf("canceled job exited %d, want its own 130", code)
		}
	}
	if code := runRunScript(t, time.Second, 0, trapped); code != ExitTimedOut {
		t.Errorf("timed-out job exited %d, want %d", code, ExitTimedOut)
	}
	if code := runRunScript(t, time.Hour, 0, "exit 7\n"); code != 7 {
		t.Errorf("job exited %d, want 7", code)
	}
	start := time.Now()
	if code := runRunScript(t, time.Hour, 0, "true\n"); code != 0 || time.Since(start) > 5*time.Second {
		t.Errorf("quick job exited %d after %s, want 0 at once (the watchdog must not hold run.sh)", code, time.Since(start))
	}
}

// A job that ignores TERM is KILLed 10s after its limit, children and all.
func TestRunScript_TimeoutKillsAJobThatIgnoresTerm(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 11s")
	}
	start := time.Now()
	if code := runRunScript(t, time.Second, 0, "trap '' TERM\nsleep 60\n"); code != ExitTimedOut {
		t.Errorf("exited %d, want %d", code, ExitTimedOut)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("took %s: the job was not killed", d)
	}
}
