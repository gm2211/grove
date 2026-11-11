package nomadjobs

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Named job secrets travel in the dispatch payload ("grove-payload/2", see internal/dispatch
// payload.go), never in dispatch meta. These tests run the rendered scripts the way an
// allocation does and check that a value reaches the caller's script intact, that nothing in it
// is ever evaluated, and that no file holding it is left behind.

// secretValue is a value no quoting shortcut survives. If any part of it were evaluated, the
// command substitutions would create files under $OUT.
const secretValue = "sp ace 'single' \"double\" $(touch \"$OUT/pwned\") `touch \"$OUT/pwned2\"` \\ end\nsecond line; touch \"$OUT/pwned3\""

// secretScript is the caller's script: it records what it was given under $OUT.
const secretScript = `printf '%s' "$NASTY" > "$OUT/nasty"
printf '%s' "${EMPTY-unset}" > "$OUT/empty"
printf '%s' "${PLAIN:-}" > "$OUT/plain"
env > "$OUT/env"
echo script-ran
`

func secretsLine(t *testing.T, secrets map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(secrets)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func secretsFrame(t *testing.T, token, script string) (payload, line string) {
	t.Helper()
	line = secretsLine(t, map[string]string{"NASTY": secretValue, "EMPTY": ""})
	return "grove-payload/2\n" + token + "\n" + line + "\n" + script, line
}

func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"jq", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

// secretJob is one allocation on a fake host.
type secretJob struct {
	t    *testing.T
	h    *macHost
	id   string
	out  string
	meta []string
}

func newSecretJob(t *testing.T) *secretJob {
	t.Helper()
	needTools(t)
	h := newMacHost(t)
	out := filepath.Join(h.root, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.bin, "git"), []byte(fakeGit), 0o755); err != nil {
		t.Fatal(err)
	}
	return &secretJob{t: t, h: h, id: "alloc-s", out: out, meta: []string{
		"OUT=" + out,
		"NOMAD_META_timeout_seconds=60",
		`NOMAD_META_env_json={"PLAIN":"from-env"}`,
		"FAKE_GIT_LOG=" + filepath.Join(h.root, "git"),
	}}
}

func (j *secretJob) taskDir(task string) string {
	return filepath.Join(j.h.root, "allocs", j.id, task, "local")
}

func (j *secretJob) state() string { return filepath.Join(j.h.allocDir(j.id), "data", "grove") }

// payload writes the dispatch payload where Nomad writes it for task.
func (j *secretJob) payload(task, file, body string) {
	j.t.Helper()
	if err := os.MkdirAll(j.taskDir(task), 0o755); err != nil {
		j.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.taskDir(task), file), []byte(body), 0o644); err != nil {
		j.t.Fatal(err)
	}
}

func (j *secretJob) run(task, script string, env ...string) (int, string) {
	j.t.Helper()
	return j.h.run(j.id, task, script, append(append([]string{}, j.meta...), env...)...)
}

func (j *secretJob) read(name string) string {
	j.t.Helper()
	b, err := os.ReadFile(filepath.Join(j.out, name))
	if err != nil {
		j.t.Fatalf("the script left no %s: %v", name, err)
	}
	return string(b)
}

// arrived checks the caller's script got the secret byte for byte, beside its env_json env, and
// that nothing in the value was evaluated on the way.
func (j *secretJob) arrived() {
	j.t.Helper()
	if got := j.read("nasty"); got != secretValue {
		j.t.Errorf("NASTY = %q, want %q", got, secretValue)
	}
	if got := j.read("empty"); got != "" {
		j.t.Errorf("EMPTY = %q, want it set and empty", got)
	}
	if got := j.read("plain"); got != "from-env" {
		j.t.Errorf("PLAIN = %q, want the env_json value", got)
	}
	for _, name := range []string{"pwned", "pwned2", "pwned3"} {
		if _, err := os.Stat(filepath.Join(j.out, name)); err == nil {
			j.t.Errorf("part of the secret's value was evaluated (%s exists)", name)
		}
	}
}

// nothingLeft checks no file of the allocation still holds the secret, encoded or not.
func (j *secretJob) nothingLeft(line string) {
	j.t.Helper()
	filepath.Walk(filepath.Join(j.h.root, "allocs", j.id), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), line) || strings.Contains(string(b), "second line; touch") {
			j.t.Errorf("%s still holds the job's secrets", path)
		}
		return nil
	})
}

func sourceAndMain(t *testing.T, kind, pool string, h *macHost) (sourceSH, runSH string) {
	t.Helper()
	job := parseJob(t, kind, pool)
	file := kind + ".nomad.hcl"
	sourceSH = taskScript(t, job, file, "source")
	if pool == "macos" {
		sourceSH = withJobsDir(t, sourceSH, h.jobs)
	}
	return sourceSH, taskScript(t, job, file, "main")
}

// build and agent: only "source" gets the payload; it leaves the secrets for "main" in a file
// only main's user can read, and run.sh exports them and deletes it before the script runs.
func TestSourceTaskHandsNamedSecretsToMain(t *testing.T) {
	for _, kind := range []string{"build", "agent"} {
		for _, pool := range []string{"linux", "macos"} {
			t.Run(kind+"/"+pool, func(t *testing.T) {
				j := newSecretJob(t)
				sourceSH, runSH := sourceAndMain(t, kind, pool, j.h)
				payload, line := secretsFrame(t, "", secretScript)
				j.payload("source", "request", payload)

				if code, out := j.run("source", sourceSH); code != 0 {
					t.Fatalf("source exit %d\n%s", code, out)
				}
				if _, err := os.Stat(filepath.Join(j.taskDir("source"), "request")); !os.IsNotExist(err) {
					t.Errorf("source left the payload behind (stat err %v)", err)
				}
				secrets := filepath.Join(j.state(), "secrets")
				info, err := os.Stat(secrets)
				if err != nil || info.Mode().Perm() != 0o400 {
					t.Fatalf("secrets file = %v, %v, want mode 0400", info, err)
				}
				if b, _ := os.ReadFile(filepath.Join(j.state(), "script.sh")); string(b) != secretScript {
					t.Errorf("script.sh = %q, want the script without the payload's frame", b)
				}
				if b, _ := os.ReadFile(filepath.Join(j.state(), "source.log")); strings.Contains(string(b), line) {
					t.Errorf("source.log holds the secrets: %s", b)
				}

				code, out := j.run("main", runSH)
				if code != 0 || !strings.Contains(out, "script-ran") {
					t.Fatalf("run.sh exit %d\n%s", code, out)
				}
				j.arrived()
				if _, err := os.Lstat(secrets); !os.IsNotExist(err) {
					t.Errorf("run.sh left the secrets file behind (stat err %v)", err)
				}
				j.nothingLeft(line)
				if strings.Contains(out, "second line") {
					t.Errorf("run.sh printed the secret:\n%s", out)
				}
			})
		}
	}
}

// With a repository: the clone token rides in the same payload and still reaches only git, and
// the secrets are not in git's environment.
func TestSourceTaskNamedSecretsBesideTheCloneToken(t *testing.T) {
	const token = "ghp_armed_secret"
	j := newSecretJob(t)
	sourceSH, runSH := sourceAndMain(t, "build", "linux", j.h)
	payload, line := secretsFrame(t, token, secretScript)
	j.payload("source", "request", payload)
	repo := "NOMAD_META_repo=https://github.com/gm2211/private"

	if code, out := j.run("source", sourceSH, repo); code != 0 {
		t.Fatalf("source exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(j.h.root, "git.env")); string(b) != token {
		t.Fatalf("clone env token = %q, want the armed token", b)
	}
	code, out := j.run("main", runSH, repo)
	if code != 0 {
		t.Fatalf("run.sh exit %d\n%s", code, out)
	}
	j.arrived()
	j.nothingLeft(line)
	if env := j.read("env"); strings.Contains(env, token) {
		t.Errorf("the caller's script saw the clone token:\n%s", env)
	}
}

// A named secret called GH_TOKEN serves the clone when no token is armed, as it did while
// secrets were part of env_json, and still reaches the script.
func TestSourceTaskClonesWithANamedGHToken(t *testing.T) {
	j := newSecretJob(t)
	sourceSH, runSH := sourceAndMain(t, "build", "linux", j.h)
	line := secretsLine(t, map[string]string{"GH_TOKEN": "ghp_named"})
	j.payload("source", "request", "grove-payload/2\n\n"+line+"\nprintf '%s' \"$GH_TOKEN\" > \"$OUT/gh\"\n")
	repo := "NOMAD_META_repo=https://github.com/gm2211/private"
	if code, out := j.run("source", sourceSH, repo); code != 0 {
		t.Fatalf("source exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(j.h.root, "git.env")); string(b) != "ghp_named" {
		t.Fatalf("clone env token = %q, want the named secret", b)
	}
	if code, out := j.run("main", runSH, repo); code != 0 || j.read("gh") != "ghp_named" {
		t.Fatalf("run.sh exit %d\n%s", code, out)
	}
}

// A clone that fails never writes the secrets file: the job will not run.
func TestSourceTaskWritesNoSecretsForAJobThatWillNotRun(t *testing.T) {
	j := newSecretJob(t)
	sourceSH, runSH := sourceAndMain(t, "build", "linux", j.h)
	payload, line := secretsFrame(t, "", secretScript)
	j.payload("source", "request", payload)
	repo := "NOMAD_META_repo=https://github.com/gm2211/private"
	if code, out := j.run("source", sourceSH, repo, "FAKE_GIT_FAIL=1"); code != 0 {
		t.Fatalf("source exit %d\n%s", code, out)
	}
	if _, err := os.Lstat(filepath.Join(j.state(), "secrets")); !os.IsNotExist(err) {
		t.Errorf("a failed source step left a secrets file (stat err %v)", err)
	}
	j.nothingLeft(line)
	if code, out := j.run("main", runSH, repo); code != 128 || strings.Contains(out, "script-ran") {
		t.Fatalf("run.sh exit %d, want the clone's 128\n%s", code, out)
	}
}

// A secrets file run.sh can't read fails the job; it never runs the script without them.
func TestRunSHFailsClosedOnUnreadableSecrets(t *testing.T) {
	for name, content := range map[string]string{
		"not base64": "%%%%\n",
		"not json":   base64.StdEncoding.EncodeToString([]byte("nope")) + "\n",
		"empty":      "",
	} {
		t.Run(name, func(t *testing.T) {
			j := newSecretJob(t)
			sourceSH, runSH := sourceAndMain(t, "build", "linux", j.h)
			j.payload("source", "request", "grove-payload/1\n\n"+secretScript)
			if code, out := j.run("source", sourceSH); code != 0 {
				t.Fatalf("source exit %d\n%s", code, out)
			}
			if err := os.WriteFile(filepath.Join(j.state(), "secrets"), []byte(content), 0o400); err != nil {
				t.Fatal(err)
			}
			code, out := j.run("main", runSH)
			if code != 1 || strings.Contains(out, "script-ran") || !strings.Contains(out, "could not read this job's secrets") {
				t.Fatalf("run.sh exit %d\n%s", code, out)
			}
		})
	}
}

// shell: "main" is the payload's only reader. It exports the secrets from the frame and runs the
// script from a copy without it.
func TestShellJobReadsNamedSecretsFromItsPayload(t *testing.T) {
	for _, pool := range []string{"linux", "macos"} {
		t.Run(pool, func(t *testing.T) {
			j := newSecretJob(t)
			runSH := j.h.runSH
			if pool == "macos" {
				if code, out := j.run("prepare", j.h.prepare); code != 0 {
					t.Fatalf("prepare exit %d\n%s", code, out)
				}
			} else {
				runSH = taskScript(t, parseJob(t, "shell", "linux"), "shell.nomad.hcl", "main")
			}
			payload, line := secretsFrame(t, "", secretScript+"head -n 1 \"$0\" > \"$OUT/first\"\n")
			j.payload("main", "script.sh", payload)

			code, out := j.run("main", runSH, "TMPDIR="+t.TempDir())
			if code != 0 || !strings.Contains(out, "script-ran") {
				t.Fatalf("run.sh exit %d\n%s", code, out)
			}
			j.arrived()
			if got := j.read("first"); !strings.HasPrefix(got, "printf '%s' \"$NASTY\"") {
				t.Errorf("the script ran from a file starting %q, want the script itself", got)
			}
			j.nothingLeft(line)
			if strings.Contains(out, "second line") {
				t.Errorf("run.sh printed the secret:\n%s", out)
			}
		})
	}
}

// A shell payload with no frame is the script, as it always was; one whose frame can't be read
// fails the job.
func TestShellJobPayloadWithoutSecretsAndBadFrame(t *testing.T) {
	j := newSecretJob(t)
	runSH := taskScript(t, parseJob(t, "shell", "linux"), "shell.nomad.hcl", "main")
	j.payload("main", "script.sh", "echo plain-ran \"$0\"\n")
	code, out := j.run("main", runSH)
	if code != 0 || !strings.Contains(out, "plain-ran "+filepath.Join(j.taskDir("main"), "script.sh")) {
		t.Fatalf("plain payload: exit %d\n%s", code, out)
	}

	for _, bad := range []string{"%%%%", base64.StdEncoding.EncodeToString([]byte("[1]"))} {
		j := newSecretJob(t)
		j.payload("main", "script.sh", "grove-payload/2\n\n"+bad+"\necho script-ran\n")
		code, out := j.run("main", runSH)
		if code != 1 || strings.Contains(out, "script-ran") || !strings.Contains(out, "could not read this job's secrets") {
			t.Fatalf("bad frame %q: exit %d\n%s", bad, code, out)
		}
	}
}

// No template reads a secret from dispatch meta: the only NOMAD_META_env_json readers are the
// ones that were there for the caller's own env, and nothing declares a meta key for secrets.
func TestTemplatesDeclareNoSecretMeta(t *testing.T) {
	for _, file := range []string{"build.nomad.hcl", "agent.nomad.hcl", "shell.nomad.hcl", "sandbox.nomad.hcl"} {
		b, err := FS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "meta_optional") || strings.Contains(line, "meta_required") {
				if strings.Contains(strings.ToLower(line), "secret") {
					t.Errorf("%s declares a dispatch meta key for secrets: %s", file, line)
				}
			}
			if strings.Contains(line, "NOMAD_META_") && strings.Contains(strings.ToLower(line), "secret") {
				t.Errorf("%s reads a secret from dispatch meta: %s", file, line)
			}
		}
	}
}
