package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePayload(t *testing.T) {
	token, secrets, script, err := ParsePayload(KindBuild, []byte("grove-payload/1\ntok\necho a\necho b\n"))
	if err != nil || token != "tok" || len(secrets) != 0 || script != "echo a\necho b\n" {
		t.Fatalf("build payload = %q, %v, %q, %v", token, secrets, script, err)
	}
	token, _, script, err = ParsePayload(KindAgent, []byte("grove-payload/1\n\nrun\n"))
	if err != nil || token != "" || script != "run\n" {
		t.Fatalf("agent payload without token = %q, %q, %v", token, script, err)
	}
	token, _, script, err = ParsePayload(KindShell, []byte("grove-payload/1\ntok\nx"))
	if err != nil || token != "" || script != "grove-payload/1\ntok\nx" {
		t.Fatalf("a shell payload is all script: %q, %q, %v", token, script, err)
	}

	line := base64.StdEncoding.EncodeToString([]byte(`{"A":"one\ntwo","B":"it's"}`))
	token, secrets, script, err = ParsePayload(KindBuild, []byte("grove-payload/2\ntok\n"+line+"\necho a\n"))
	if err != nil || token != "tok" || script != "echo a\n" || secrets["A"] != "one\ntwo" || secrets["B"] != "it's" {
		t.Fatalf("version 2 build payload = %q, %v, %q, %v", token, secrets, script, err)
	}
	token, secrets, script, err = ParsePayload(KindShell, []byte("grove-payload/2\n\n"+line+"\necho a\n"))
	if err != nil || token != "" || script != "echo a\n" || len(secrets) != 2 {
		t.Fatalf("version 2 shell payload = %q, %v, %q, %v", token, secrets, script, err)
	}
	// A secrets line that can't be read fails the job, and the error doesn't quote it.
	for _, bad := range []string{"!!not-base64!!", base64.StdEncoding.EncodeToString([]byte("not json"))} {
		if _, _, _, err := ParsePayload(KindShell, []byte("grove-payload/2\n\n"+bad+"\necho a\n")); err == nil || strings.Contains(err.Error(), bad) {
			t.Fatalf("ParsePayload(%q) error = %v, want one that doesn't quote the line", bad, err)
		}
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
	// The clone, with the token in its environment, then the local check that ref is there,
	// without it.
	if len(lines) != 4 || lines[1] != "token=s3cret" || lines[3] != "token=" {
		t.Fatalf("calls = %q, want the clone with the token in its environment, then a local check without it", data)
	}
	if want := "-C " + filepath.Join(dir, "work") + " rev-parse --verify --quiet main^{commit}"; lines[2] != want {
		t.Errorf("second call = %s, want %s", lines[2], want)
	}
	argv := lines[0]
	if strings.Contains(argv, "s3cret") {
		t.Errorf("token in argv: %s", argv)
	}
	if !strings.HasPrefix(argv, "-c credential.helper= -c core.hooksPath=/dev/null -c credential.https://github.com.helper=!f()") {
		t.Errorf("argv = %s, want helpers reset, hooks off, then grove's helper for the repo's host only", argv)
	}
	if strings.Count(argv, "helper=!f()") != 1 {
		t.Errorf("argv = %s, want grove's helper set exactly once, never for every host", argv)
	}
	// Nothing is checked out on the control plane: the job's container does that.
	if !strings.HasSuffix(argv, "clone --no-checkout --depth 50 -- https://github.com/o/r "+filepath.Join(dir, "work")) {
		t.Errorf("clone argv = %s", argv)
	}
}

// TestClone_ScopesCredentialToRepoHost: the host the helper is set for comes from the repo URL,
// and a URL whose host isn't a plain name gets no clone at all while there is a token to protect.
func TestClone_ScopesCredentialToRepoHost(t *testing.T) {
	for repo, want := range map[string]string{
		"https://github.com/o/r":            "github.com",
		"https://ghe.example.com:8443/o/r":  "ghe.example.com:8443",
		"https://10.0.0.7/o/r.git":          "10.0.0.7",
		"https://github.com":                "github.com",
		"https://user@github.com/o/r":       "",
		"https://user:pw@github.com/o/r":    "",
		"https://[::1]/o/r":                 "",
		"https:///o/r":                      "",
		"https://github.com.helper=x y/o/r": "",
		"https://-evil.example/o/r":         "",
		"https://evil.example./o/r":         "",
		"https://github.com?x=/o/r":         "",
		"https://github.com#frag/o/r":       "",
		`https://github.com\@evil.test/o/r`: "",
		"https://github.com:/o/r":           "",
		"https://github.com:80a/o/r":        "",
		"https://github.com\n/o/r":          "",
		"http://github.com/o/r":             "",
	} {
		host, ok := credentialHost(repo)
		if want == "" {
			if ok {
				t.Errorf("credentialHost(%q) = %q, want it refused", repo, host)
			}
			continue
		}
		if !ok || host != want {
			t.Errorf("credentialHost(%q) = %q, %v, want %q", repo, host, ok, want)
		}
	}

	var log bytes.Buffer
	if code := Clone(context.Background(), "/nonexistent/git", "https://user@github.com/o/r", "", "s3cret", t.TempDir(), &log); code != cloneRefused {
		t.Fatalf("userinfo URL with a token: code %d", code)
	}
	if !strings.Contains(log.String(), "plain host name") || strings.Contains(log.String(), "s3cret") {
		t.Errorf("log = %q", log.String())
	}
}

// TestCloneHelperAnswersOnlyTheRepoHost asks real git, configured the way Clone configures it,
// for a credential: it gets the token for the repository's host and nothing for any other.
func TestCloneHelperAnswersOnlyTheRepoHost(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "git")
	// Records the first call's arguments NUL-terminated, so the helper's text survives intact.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n[ -s "+record+" ] || printf '%s\\0' \"$@\" > "+record+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if code := Clone(context.Background(), fake, "https://github.com/o/r", "", "s3cret", filepath.Join(dir, "work"), &log); code != 0 {
		t.Fatalf("code %d, log %s", code, log.String())
	}
	data, _ := os.ReadFile(record)
	argv := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	var config []string
	for i := 0; i+1 < len(argv) && argv[i] == "-c"; i += 2 {
		config = append(config, "-c", argv[i+1])
	}
	if len(config) != 6 {
		t.Fatalf("clone argv = %q, want three -c settings first", argv)
	}
	fill := func(host string) (string, error) {
		cmd := exec.Command(gitBin, append(config, "credential", "fill")...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GROVE_CLONE_TOKEN=s3cret"}
		cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
		out, err := cmd.Output()
		return string(out), err
	}
	if out, err := fill("github.com"); err != nil || !strings.Contains(out, "password=s3cret") {
		t.Fatalf("the repo's host got %q, %v, want the token", out, err)
	}
	for _, host := range []string{"evil.example", "github.com.evil.example", "api.github.com", "github.com:8443"} {
		if out, err := fill(host); err == nil || strings.Contains(out, "s3cret") {
			t.Errorf("host %s got %q (err %v), want no credential", host, out, err)
		}
	}
}

// TestFetchRef fetches what a shallow clone of the default branch leaves out: another branch, and a
// commit that is on no branch the clone has (a pull request's head).
func TestFetchRef(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	src := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(src, "init", "-q", "-b", "main")
	run(src, "config", "uploadpack.allowAnySHA1InWant", "true")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("main"), 0o644)
	run(src, "add", ".")
	run(src, "commit", "-q", "-m", "main")
	run(src, "checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("feature"), 0o644)
	run(src, "commit", "-q", "-am", "feature")
	run(src, "checkout", "-q", "-b", "pr")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("pr"), 0o644)
	run(src, "commit", "-q", "-am", "pr")
	prSHA := run(src, "rev-parse", "HEAD")
	run(src, "checkout", "-q", "main")
	run(src, "branch", "-q", "-D", "pr")

	for _, tc := range []struct{ ref, want string }{{"main", "main"}, {"feature", "feature"}, {prSHA, "pr"}} {
		work := filepath.Join(t.TempDir(), "work")
		run("", "clone", "-q", "--no-checkout", "--depth", "50", "file://"+src, work)
		var log bytes.Buffer
		fetchRef(context.Background(), git, nil, os.Environ(), work, tc.ref, &log)
		run(work, "checkout", "-q", "--force", tc.ref, "--")
		if data, _ := os.ReadFile(filepath.Join(work, "a.txt")); string(data) != tc.want {
			t.Errorf("ref %s: a.txt = %q, want %q (log %s)", tc.ref, data, tc.want, log.String())
		}
	}
}

func TestRunScript(t *testing.T) {
	got := runScript(90*time.Second, "", false)
	if !strings.Contains(got, "sleep 90 & grove_sleep=$!") || !strings.Contains(got, ". /grove/env.sh") || strings.Contains(got, "git") {
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
