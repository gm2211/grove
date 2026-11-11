package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A value no quoting shortcut survives: spaces, both quotes, command substitutions, a backslash
// and a newline.
const nastySecret = "sp ace 'single' \"double\" $(echo sealed-marker) `echo sealed-tick` \\ end\nsecond line"

// secretsPayload is the version 2 dispatch payload of a job with one named secret.
func secretsPayload(t *testing.T, kind, token, script string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"NASTY": nastySecret})
	if err != nil {
		t.Fatal(err)
	}
	return []byte("grove-payload/2\n" + token + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n" + script)
}

// A sealed Linux job's named secret comes out of the payload and reaches the job only in the
// bundle's env.sh: no command sent to the VM and nothing the runner prints carries it.
func TestRun_NamedSecretReachesSealedJobOnlyInTheBundle(t *testing.T) {
	h := newHarness(t)
	req := shellRequest()
	if err := req.ApplyPayload(secretsPayload(t, KindShell, "", "echo hi\n")); err != nil {
		t.Fatalf("ApplyPayload: %v", err)
	}
	if req.Script != "echo hi\n" || req.CloneToken != "" {
		t.Fatalf("script %q, token %q", req.Script, req.CloneToken)
	}
	if code := h.runner.Run(context.Background(), req); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderrString())
	}
	if h.vm.pushed["grove/script.sh"] != "echo hi\n" {
		t.Errorf("script.sh = %q, want the script without the payload's frame", h.vm.pushed["grove/script.sh"])
	}

	// env.sh, sourced by a real bash, yields the value byte for byte and runs nothing in it.
	dir := t.TempDir()
	envSH := filepath.Join(dir, "env.sh")
	if err := os.WriteFile(envSH, []byte(h.vm.pushed["grove/env.sh"]), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/bash", "-c", `. "$1"; printf '%s' "$NASTY"; printf '|%s' "$FOO"`, "bash", envSH).Output()
	if err != nil {
		t.Fatalf("source env.sh: %v", err)
	}
	if string(out) != nastySecret+"|it's" {
		t.Fatalf("the job's environment = %q, want the secret intact beside the caller's env", out)
	}

	for _, c := range h.vm.commands {
		if strings.Contains(c, "sealed-marker") || strings.Contains(c, "second line") {
			t.Errorf("a command sent to the VM carries the secret: %s", c)
		}
	}
	for name, text := range map[string]string{"stdout": h.stdout.String(), "stderr": h.stderrString()} {
		if strings.Contains(text, "sealed-marker") {
			t.Errorf("the runner's %s carries the secret: %s", name, text)
		}
	}
}

// The same on a sealed macOS pool, where the fake VM really runs the job: the script sees the
// value intact, and it went in only through the bundle.
func TestRunMacOS_NamedSecretReachesTheJob(t *testing.T) {
	h := newMacHarness(t)
	req := macRequest("")
	if err := req.ApplyPayload(secretsPayload(t, KindShell, "", "printf 'got=[%s]' \"$NASTY\"\n")); err != nil {
		t.Fatalf("ApplyPayload: %v", err)
	}
	if code := h.runner.Run(context.Background(), req); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, h.stderr.String())
	}
	if got := h.stdout.String(); got != "got=["+nastySecret+"]" {
		t.Fatalf("stdout = %q, want the secret intact in the job's environment", got)
	}
	for _, c := range h.vm.commands {
		if strings.Contains(c, "sealed-marker") || strings.Contains(c, "second line") {
			t.Errorf("a command sent to the VM carries the secret: %s", c)
		}
	}
	if strings.Contains(h.stderr.String(), "sealed-marker") {
		t.Errorf("the runner's stderr carries the secret: %s", h.stderr.String())
	}
}

// A build job's version 2 payload still yields its clone token, and a payload whose secrets can't
// be read stops the job before anything runs.
func TestApplyPayload(t *testing.T) {
	req := Request{Kind: KindBuild, Env: map[string]string{"CI": "1"}}
	if err := req.ApplyPayload(secretsPayload(t, KindBuild, "clone-tok", "make\n")); err != nil {
		t.Fatalf("ApplyPayload: %v", err)
	}
	if req.CloneToken != "clone-tok" || req.Script != "make\n" || req.Env["NASTY"] != nastySecret || req.Env["CI"] != "1" {
		t.Fatalf("request = token %q script %q env %v", req.CloneToken, req.Script, req.Env)
	}

	req = Request{Kind: KindShell}
	if err := req.ApplyPayload([]byte("echo plain\n")); err != nil || req.Script != "echo plain\n" || len(req.Env) != 0 {
		t.Fatalf("plain shell payload: script %q env %v err %v", req.Script, req.Env, err)
	}

	req = Request{Kind: KindShell}
	if err := req.ApplyPayload([]byte("grove-payload/2\n\n%%%\necho hi\n")); err == nil {
		t.Fatal("ApplyPayload accepted a secrets line it can't read")
	}
	if req.Script != "" {
		t.Fatalf("a refused payload left a script behind: %q", req.Script)
	}
}
