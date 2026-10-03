package nomadjobs

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/jobspec2"
)

// renderData mirrors the fields the grove server is expected to supply when rendering a job
// template (see README.md). CPU/Memory are optional — each template falls back to a default when
// they're zero.
type renderData struct {
	Kind              string
	Pool              string
	CPU               int
	Memory            int
	AllowDockerSocket bool
	RunnerImage       string
}

func render(t *testing.T, file string, data renderData) string {
	t.Helper()
	if data.RunnerImage == "" {
		data.RunnerImage = "ghcr.io/gm2211/grove-runner:latest"
	}
	tmpl, err := template.New(file).ParseFS(FS, file)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var sb strings.Builder
	if err := tmpl.ExecuteTemplate(&sb, file, data); err != nil {
		t.Fatalf("execute %s: %v", file, err)
	}
	return sb.String()
}

func TestRenderAllKindsAndPools(t *testing.T) {
	files := map[string]string{
		"build": "build.nomad.hcl",
		"agent": "agent.nomad.hcl",
		"shell": "shell.nomad.hcl",
	}
	pools := []string{"linux", "macos"}

	for kind, file := range files {
		for _, pool := range pools {
			t.Run(kind+"/"+pool, func(t *testing.T) {
				out := render(t, file, renderData{Kind: kind, Pool: pool})

				wantJob := `job "grove-` + kind + `-` + pool + `"`
				if !strings.Contains(out, wantJob) {
					t.Errorf("rendered %s/%s missing job name %q\n---\n%s", kind, pool, wantJob, out)
				}

				if !strings.Contains(out, `${meta.pool}`) {
					t.Errorf("rendered %s/%s missing ${meta.pool} constraint attribute\n---\n%s", kind, pool, out)
				}
				if !strings.Contains(out, `value     = "`+pool+`"`) {
					t.Errorf("rendered %s/%s constraint value missing pool %q\n---\n%s", kind, pool, pool, out)
				}

				// Driver must match the pool: macos -> raw_exec, everything else -> docker.
				if pool == "macos" {
					if !strings.Contains(out, `driver = "raw_exec"`) {
						t.Errorf("rendered %s/%s expected raw_exec driver\n---\n%s", kind, pool, out)
					}
					if strings.Contains(out, `driver = "docker"`) {
						t.Errorf("rendered %s/%s unexpectedly contains docker driver\n---\n%s", kind, pool, out)
					}
				} else {
					if !strings.Contains(out, `driver = "docker"`) {
						t.Errorf("rendered %s/%s expected docker driver\n---\n%s", kind, pool, out)
					}
					if strings.Contains(out, `driver = "raw_exec"`) {
						t.Errorf("rendered %s/%s unexpectedly contains raw_exec driver\n---\n%s", kind, pool, out)
					}
				}

				// Shared dispatch contract, identical across kinds/pools.
				for _, want := range []string{
					`payload       = "required"`,
					`meta_required = ["requester"]`,
					`meta_optional = ["repo", "ref", "env_json", "timeout_seconds", "grove_meta_json", "artifact_prefix", "image"]`,
					`attempts = 0`, // reschedule/restart
					`dispatch_payload`,
					`destination = "local/run.sh"`,
				} {
					if !strings.Contains(out, want) {
						t.Errorf("rendered %s/%s missing %q\n---\n%s", kind, pool, want, out)
					}
				}

				// The payload lands in the source task for kinds that clone, else in main.
				wantPayloadFile := `file = "request"`
				if kind == "shell" {
					wantPayloadFile = `file = "script.sh"`
				}
				if !strings.Contains(out, wantPayloadFile) {
					t.Errorf("rendered %s/%s missing %q", kind, pool, wantPayloadFile)
				}

				// kind-specific shape.
				switch kind {
				case "agent":
					if !strings.Contains(out, `kill_timeout = "10m"`) {
						t.Errorf("rendered agent/%s missing kill_timeout = \"10m\"", pool)
					}
				case "build":
					if !strings.Contains(out, "mc alias set") {
						t.Errorf("rendered build/%s missing artifact upload via mc", pool)
					}
				case "shell":
					if strings.Contains(out, "git clone --depth") {
						t.Errorf("rendered shell/%s unexpectedly clones a repo", pool)
					}
				}
			})
		}
	}
}

func TestRenderRespectsCPUAndMemoryOverrides(t *testing.T) {
	out := render(t, "build.nomad.hcl", renderData{Kind: "build", Pool: "linux", CPU: 8000, Memory: 16384})
	if !strings.Contains(out, "cpu    = 8000") {
		t.Errorf("expected overridden cpu, got:\n%s", out)
	}
	if !strings.Contains(out, "memory = 16384") {
		t.Errorf("expected overridden memory, got:\n%s", out)
	}
}

func TestRenderDefaultsCPUAndMemoryWhenZero(t *testing.T) {
	out := render(t, "shell.nomad.hcl", renderData{Kind: "shell", Pool: "macos"})
	if !strings.Contains(out, "cpu    = 2000") {
		t.Errorf("expected default cpu 2000, got:\n%s", out)
	}
	if !strings.Contains(out, "memory = 4096") {
		t.Errorf("expected default memory 4096, got:\n%s", out)
	}
}

// dockerRunMarkers maps each kind to the nested-`docker run` invocation form it uses in run.sh
// ("build" uses plain `docker run --rm`, "agent"/"shell" use `exec docker run --rm` since their
// run.sh replaces the shell process instead of capturing an exit code).
var dockerRunMarkers = map[string]string{
	"build": "docker run --rm",
	"agent": "exec docker run --rm",
	"shell": "exec docker run --rm",
}

// TestRenderDockerSocketOptOut asserts that with AllowDockerSocket left at its zero value (the
// fleet.yaml default), the rendered HCL for every kind's docker-driver ("linux") task neither
// mounts the VM's docker socket nor ever attempts a nested `docker run` — see
// internal/fleet.Pool.AllowDockerSocket and docs/JOBS.md.
func TestRenderDockerSocketOptOut(t *testing.T) {
	files := map[string]string{
		"build": "build.nomad.hcl",
		"agent": "agent.nomad.hcl",
		"shell": "shell.nomad.hcl",
	}
	for kind, file := range files {
		t.Run(kind, func(t *testing.T) {
			out := render(t, file, renderData{Kind: kind, Pool: "linux", AllowDockerSocket: false})
			if strings.Contains(out, "/var/run/docker.sock") {
				t.Errorf("rendered %s/linux with AllowDockerSocket=false unexpectedly mounts the docker socket\n---\n%s", kind, out)
			}
			if strings.Contains(out, dockerRunMarkers[kind]) {
				t.Errorf("rendered %s/linux with AllowDockerSocket=false unexpectedly attempts a nested docker run\n---\n%s", kind, out)
			}
		})
	}
}

// TestRenderDockerSocketOptIn asserts that with AllowDockerSocket true, the rendered HCL restores
// exactly the previous (pre-opt-in) behavior: the docker socket is mounted and run.sh is able to
// nest a `docker run` for a per-dispatch NOMAD_META_image override.
func TestRenderDockerSocketOptIn(t *testing.T) {
	files := map[string]string{
		"build": "build.nomad.hcl",
		"agent": "agent.nomad.hcl",
		"shell": "shell.nomad.hcl",
	}
	for kind, file := range files {
		t.Run(kind, func(t *testing.T) {
			out := render(t, file, renderData{Kind: kind, Pool: "linux", AllowDockerSocket: true})
			if !strings.Contains(out, "/var/run/docker.sock:/var/run/docker.sock") {
				t.Errorf("rendered %s/linux with AllowDockerSocket=true missing docker socket mount\n---\n%s", kind, out)
			}
			if !strings.Contains(out, dockerRunMarkers[kind]) {
				t.Errorf("rendered %s/linux with AllowDockerSocket=true missing nested docker run invocation\n---\n%s", kind, out)
			}
		})
	}
}

// TestRenderedHCLParsesWithJobspec2 is the real regression test for the bash-vs-HCL interpolation
// bug: `grove serve` calls EnsureJobs (internal/dispatch/nomadjobs.go), which renders each
// nomad/jobs/*.nomad.hcl template and registers it with Nomad. Every bash expansion inside the
// embedded run.sh `template { data = <<-EOF ... EOF }` heredoc (e.g. `${NOMAD_META_x:-default}`)
// is, syntactically, also valid Nomad HCL2 template interpolation — so an un-escaped `${` there
// breaks parsing at registration time (see docs/JOBS.md "HCL escaping"). This parses every
// rendered template with Nomad's own jobspec2 library (the same parser `nomad job validate`/
// `nomad job run` use) offline, so a future edit that reintroduces an unescaped `${` fails `go
// test` instead of only failing at `grove serve` runtime against a live Nomad cluster.
func TestRenderedHCLParsesWithJobspec2(t *testing.T) {
	files := map[string]string{
		"build": "build.nomad.hcl",
		"agent": "agent.nomad.hcl",
		"shell": "shell.nomad.hcl",
	}
	pools := []string{"linux", "macos"}
	sockets := []bool{false, true}

	for kind, file := range files {
		for _, pool := range pools {
			for _, allowDockerSocket := range sockets {
				name := kind + "/" + pool + "/socket=" + boolToStr(allowDockerSocket)
				t.Run(name, func(t *testing.T) {
					hcl := render(t, file, renderData{
						Kind:              kind,
						Pool:              pool,
						AllowDockerSocket: allowDockerSocket,
					})

					job, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{
						Path:    "job.hcl",
						Body:    []byte(hcl),
						AllowFS: false,
					})
					if err != nil {
						t.Fatalf("jobspec2 failed to parse rendered %s (pool=%s, AllowDockerSocket=%v): %v\n---\n%s", file, pool, allowDockerSocket, err, hcl)
					}

					wantID := "grove-" + kind + "-" + pool
					if job.ID == nil || *job.ID != wantID {
						gotID := "<nil>"
						if job.ID != nil {
							gotID = *job.ID
						}
						t.Errorf("job.ID = %q, want %q", gotID, wantID)
					}

					if len(job.TaskGroups) != 1 {
						t.Fatalf("expected exactly one group, got %d", len(job.TaskGroups))
					}
					tasks := map[string]*nomadapi.Task{}
					var names []string
					for _, task := range job.TaskGroups[0].Tasks {
						tasks[task.Name] = task
						names = append(names, task.Name)
					}
					wantNames := []string{"main"}
					if kind != "shell" {
						wantNames = []string{"source", "main"}
					}
					if strings.Join(names, ",") != strings.Join(wantNames, ",") {
						t.Fatalf("tasks = %v, want %v", names, wantNames)
					}

					wantDriver := "docker"
					if pool == "macos" {
						wantDriver = "raw_exec"
					}
					for _, task := range tasks {
						if task.Driver != wantDriver {
							t.Errorf("task %s driver = %q, want %q", task.Name, task.Driver, wantDriver)
						}
					}

					main := tasks["main"]
					if kind == "shell" {
						return
					}
					// Only the source task may receive the payload (it carries the clone token),
					// and only it runs before main.
					if main.DispatchPayload != nil {
						t.Error("main task declares dispatch_payload; the clone token would reach the caller's script")
					}
					source := tasks["source"]
					if source.DispatchPayload == nil || source.DispatchPayload.File != "request" {
						t.Errorf("source task dispatch_payload = %+v, want file \"request\"", source.DispatchPayload)
					}
					if source.Lifecycle == nil || source.Lifecycle.Hook != "prestart" || source.Lifecycle.Sidecar {
						t.Errorf("source task lifecycle = %+v, want a non-sidecar prestart", source.Lifecycle)
					}
					// macOS jobs never run the caller's script as root.
					wantUser := ""
					if pool == "macos" {
						wantUser = "_grovejob"
					}
					if main.User != wantUser {
						t.Errorf("main task user = %q, want %q", main.User, wantUser)
					}
					if source.User != "" {
						t.Errorf("source task user = %q, want the agent's own (root) user", source.User)
					}
				})
			}
		}
	}
}

func boolToStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// runRunSH writes runSH and scriptBody to a fresh NOMAD_TASK_DIR-shaped temp dir and executes
// run.sh with /bin/bash, the same way raw_exec (macOS) and the docker driver's
// args = ["${NOMAD_TASK_DIR}/run.sh"] invoke it in production. It returns the process's real exit
// code (extracted from *exec.ExitError, not the Go-side error), combined stdout+stderr, and how
// long the run took.
func runRunSH(t *testing.T, runSH, scriptBody, timeoutSeconds string) (code int, output string, elapsed time.Duration) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "script.sh"), []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write script.sh: %v", err)
	}
	runPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(runPath, []byte(runSH), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}

	cmd := exec.Command("/bin/bash", runPath)
	cmd.Env = append(os.Environ(),
		"NOMAD_TASK_DIR="+dir,
		"NOMAD_META_timeout_seconds="+timeoutSeconds,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	err := cmd.Run()
	elapsed = time.Since(start)

	if err == nil {
		return 0, out.String(), elapsed
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), out.String(), elapsed
	}
	t.Fatalf("run run.sh: %v\noutput:\n%s", err, out.String())
	return -1, out.String(), elapsed
}

// renderedRunSH renders file for (kind, pool) and returns the real, HCL-unescaped run.sh body —
// i.e. the same string Nomad would write to ${NOMAD_TASK_DIR}/run.sh at dispatch time. It goes
// through jobspec2 (not the raw Go-template output used elsewhere in this file) specifically
// because `$${...}` only collapses to `${...}` during HCL2 parsing (see docs/JOBS.md "HCL
// escaping") — executing the raw template output directly would leave literal `$${` sequences
// bash would misinterpret as `$$` (the shell's own PID) followed by stray `{...}` text.
func renderedRunSH(t *testing.T, file string, data renderData) string {
	t.Helper()
	hcl := render(t, file, data)
	job, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{
		Path:    "job.hcl",
		Body:    []byte(hcl),
		AllowFS: false,
	})
	if err != nil {
		t.Fatalf("jobspec2 failed to parse rendered %s: %v", file, err)
	}
	return taskScript(t, job, file, "main")
}

// taskScript returns the single embedded template body of the named task in a parsed job.
func taskScript(t *testing.T, job *nomadapi.Job, file, name string) string {
	t.Helper()
	for _, task := range job.TaskGroups[0].Tasks {
		if task.Name != name {
			continue
		}
		if len(task.Templates) != 1 || task.Templates[0].EmbeddedTmpl == nil {
			t.Fatalf("rendered %s: expected exactly one template with embedded data on task %q", file, task.Name)
		}
		return *task.Templates[0].EmbeddedTmpl
	}
	t.Fatalf("rendered %s: no task %q", file, name)
	return ""
}

// TestRunSHPortableTimeout is the regression test for the live defect this fix addresses: a
// `shell` job dispatched to a macOS (raw_exec) node failed every time with exit 127
// ("…/run.sh: line N: exec: timeout: not found") because GNU coreutils' `timeout(1)` does not
// exist on stock macOS. run.sh's run_with_timeout() prefers the real `timeout` when the host has
// one, and otherwise runs the command in the background under a watchdog. This test extracts the
// real run.sh body (via renderedRunSH, so it exercises exactly what Nomad would run) and executes
// it directly with /bin/bash against a temp script — on a host with `timeout` on PATH this
// exercises run_with_timeout's fast path, and on one without it (e.g. a stock macOS runner, the
// scenario that was broken) it exercises the watchdog fallback. Both must satisfy the same
// contract: propagate the script's real exit code, and exit 124 on a timeout so
// internal/dispatch's 124 -> Job.TimedOut mapping keeps working regardless of which path ran.
func TestRunSHPortableTimeout(t *testing.T) {
	runSH := renderedRunSH(t, "shell.nomad.hcl", renderData{Kind: "shell", Pool: "macos"})

	t.Run("exits 0 and echoes", func(t *testing.T) {
		code, out, _ := runRunSH(t, runSH, "#!/bin/bash\necho hello-from-script\n", "60")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		if !strings.Contains(out, "hello-from-script") {
			t.Fatalf("output missing expected echo; output:\n%s", out)
		}
	})

	t.Run("propagates a non-timeout exit code", func(t *testing.T) {
		code, out, _ := runRunSH(t, runSH, "#!/bin/bash\nexit 3\n", "60")
		if code != 3 {
			t.Fatalf("exit code = %d, want 3; output:\n%s", code, out)
		}
	})

	t.Run("times out and exits 124", func(t *testing.T) {
		code, out, elapsed := runRunSH(t, runSH, "#!/bin/bash\nsleep 30\n", "1")
		if code != 124 {
			t.Fatalf("exit code = %d, want 124; output:\n%s", code, out)
		}
		if elapsed > 3*time.Second {
			t.Fatalf("timeout took %s, want well under the 30s sleep (watchdog fires ~1s after T, plus up to a 10s SIGKILL grace only if SIGTERM didn't work)", elapsed)
		}
	})
}

// fakeGit stands in for git in TestSourceTaskKeepsCloneTokenFromScript: `clone` records its argv,
// its GROVE_CLONE_TOKEN and what its credential helper answers, then creates the destination (or
// fails like a missing repository when FAKE_GIT_FAIL is set); every other subcommand succeeds.
const fakeGit = `#!/bin/bash
args=("$@")
helper=""
i=0
while [ $i -lt ${#args[@]} ]; do
  case "${args[$i]}" in
    -c) case "${args[$((i+1))]}" in credential.helper=?*) helper="${args[$((i+1))]#credential.helper=}" ;; esac; i=$((i+2)) ;;
    -C) i=$((i+2)) ;;
    *) break ;;
  esac
done
[ "${args[$i]}" = clone ] || exit 0
printf '%s\n' "$@" > "$FAKE_GIT_LOG.argv"
printf '%s' "${GROVE_CLONE_TOKEN:-}" > "$FAKE_GIT_LOG.env"
if [ -n "$helper" ]; then
  sh -c "${helper#!} get" > "$FAKE_GIT_LOG.helper"
fi
if [ -n "${FAKE_GIT_FAIL:-}" ]; then
  echo "fatal: repository not found" >&2
  exit 128
fi
mkdir -p "${@: -1}"
`

// TestSourceTaskKeepsCloneTokenFromScript runs the real source.sh and run.sh (as rendered for
// Nomad) back to back, the way a build allocation does: the clone gets the token, while the
// caller's script sees it in no environment variable, argv or file it can read.
func TestSourceTaskKeepsCloneTokenFromScript(t *testing.T) {
	const token = "ghp_armed_secret"
	job, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{
		Path: "job.hcl", Body: []byte(render(t, "build.nomad.hcl", renderData{Kind: "build", Pool: "linux"})),
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sourceSH := taskScript(t, job, "build.nomad.hcl", "source")
	runSH := taskScript(t, job, "build.nomad.hcl", "main")

	run := func(t *testing.T, payload string, extraEnv ...string) (alloc, gitLog string, code int, out string) {
		t.Helper()
		root := t.TempDir()
		alloc = filepath.Join(root, "alloc")
		sourceDir := filepath.Join(root, "source", "local")
		mainDir := filepath.Join(root, "main", "local")
		bin := filepath.Join(root, "bin")
		for _, dir := range []string{alloc, sourceDir, mainDir, bin} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write := func(path, body string) {
			if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(bin, "git"), fakeGit)
		write(filepath.Join(sourceDir, "request"), payload)
		write(filepath.Join(sourceDir, "source.sh"), sourceSH)
		write(filepath.Join(mainDir, "run.sh"), runSH)
		gitLog = filepath.Join(root, "git")

		env := append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"),
			"NOMAD_ALLOC_DIR="+alloc,
			"NOMAD_META_repo=https://github.com/gm2211/private",
			"NOMAD_META_env_json={\"CI\":\"1\"}",
			"FAKE_GIT_LOG="+gitLog,
		)
		env = append(env, extraEnv...)

		source := exec.Command("/bin/bash", filepath.Join(sourceDir, "source.sh"))
		source.Env = append(env, "NOMAD_TASK_DIR="+sourceDir)
		if b, err := source.CombinedOutput(); err != nil {
			t.Fatalf("source.sh: %v\n%s", err, b)
		}
		if _, err := os.Stat(filepath.Join(sourceDir, "request")); !os.IsNotExist(err) {
			t.Fatalf("source.sh left the payload behind (stat err %v)", err)
		}

		main := exec.Command("/bin/bash", filepath.Join(mainDir, "run.sh"))
		main.Env = append(env, "NOMAD_TASK_DIR="+mainDir)
		// A file, not a pipe: where the host has no timeout(1) (macOS), run.sh's watchdog leaves a
		// `sleep` behind that would hold a pipe open, and Run would wait on it.
		outFile, err := os.Create(filepath.Join(root, "main.out"))
		if err != nil {
			t.Fatal(err)
		}
		defer outFile.Close()
		main.Stdout, main.Stderr = outFile, outFile
		err = main.Run()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exitErr):
			code = exitErr.ExitCode()
		default:
			t.Fatalf("run.sh: %v", err)
		}
		got, _ := os.ReadFile(outFile.Name())
		return alloc, gitLog, code, string(got)
	}

	t.Run("token reaches the clone only", func(t *testing.T) {
		script := "echo in-checkout=$(basename \"$PWD\"); env\n"
		alloc, gitLog, code, out := run(t, "grove-payload/1\n"+token+"\n"+script)
		if code != 0 {
			t.Fatalf("run.sh exit = %d\n%s", code, out)
		}
		if !strings.Contains(out, "in-checkout=work") || !strings.Contains(out, "CI=1") {
			t.Fatalf("script did not run in the checkout with the caller's env:\n%s", out)
		}
		if strings.Contains(out, token) {
			t.Fatalf("the caller's script saw the clone token:\n%s", out)
		}
		filepath.Walk(alloc, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				if b, _ := os.ReadFile(path); strings.Contains(string(b), token) {
					t.Errorf("%s holds the clone token", path)
				}
			}
			return nil
		})
		argv, _ := os.ReadFile(gitLog + ".argv")
		if strings.Contains(string(argv), token) {
			t.Fatalf("the token was on git's command line: %s", argv)
		}
		if !strings.Contains(string(argv), "--\nhttps://github.com/gm2211/private\n") {
			t.Fatalf("clone did not end options before the repo URL: %s", argv)
		}
		if b, _ := os.ReadFile(gitLog + ".env"); string(b) != token {
			t.Fatalf("clone env token = %q, want the armed token", b)
		}
		if b, _ := os.ReadFile(gitLog + ".helper"); !strings.Contains(string(b), "password="+token) {
			t.Fatalf("credential helper answered %q", b)
		}
	})

	t.Run("caller env token still clones", func(t *testing.T) {
		_, gitLog, code, out := run(t, "grove-payload/1\n\ntrue\n", "NOMAD_META_env_json={\"GH_TOKEN\":\"ghp_caller\"}")
		if code != 0 {
			t.Fatalf("run.sh exit = %d\n%s", code, out)
		}
		if b, _ := os.ReadFile(gitLog + ".env"); string(b) != "ghp_caller" {
			t.Fatalf("clone env token = %q, want the caller's", b)
		}
	})

	t.Run("failed clone shows in the job's output and exit code", func(t *testing.T) {
		_, _, code, out := run(t, "grove-payload/1\n"+token+"\necho should-not-run\n", "FAKE_GIT_FAIL=1")
		if code != 128 {
			t.Fatalf("run.sh exit = %d, want git's 128\n%s", code, out)
		}
		if !strings.Contains(out, "repository not found") || strings.Contains(out, "should-not-run") {
			t.Fatalf("output = %q", out)
		}
	})

	t.Run("non-https repo is refused", func(t *testing.T) {
		_, _, code, out := run(t, "grove-payload/1\n\ntrue\n", "NOMAD_META_repo=git@github.com:gm2211/private.git")
		if code == 0 || !strings.Contains(out, "https://") {
			t.Fatalf("run.sh exit = %d, output %q", code, out)
		}
	})
}
