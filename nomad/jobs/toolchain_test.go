package nomadjobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/jobspec2"
)

const (
	toolchainJSON = `{"node":{"version":"22.11.0"}}`
	toolchainHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// Every template (the sealed one included) accepts the two toolchain meta keys, so a dispatch that
// carries them is never refused by Nomad.
func TestAllTemplatesListToolchainMeta(t *testing.T) {
	for _, file := range []string{"build.nomad.hcl", "agent.nomad.hcl", "shell.nomad.hcl", "sandbox.nomad.hcl"} {
		for _, pool := range []string{"linux", "macos"} {
			kind := strings.TrimSuffix(file, ".nomad.hcl")
			out := render(t, file, renderData{Kind: kind, Pool: pool, GroveBin: "/bin/grove", Slots: 1})
			job, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{Path: "job.hcl", Body: []byte(out)})
			if err != nil {
				t.Fatalf("%s/%s: %v", file, pool, err)
			}
			have := map[string]bool{}
			for _, k := range job.ParameterizedJob.MetaOptional {
				have[k] = true
			}
			if !have["toolchain_json"] || !have["toolchain_hash"] {
				t.Errorf("%s/%s meta_optional = %v", file, pool, job.ParameterizedJob.MetaOptional)
			}
		}
	}
}

// macSource runs the macOS build source.sh against a fake sudo that records how the toolchain
// installer was started: its argv, the environment it inherited, whether the clone token's file
// still existed, and its stdin.
type macSource struct {
	alloc, sudoLog, tcLog string
}

func runMacSource(t *testing.T, env ...string) macSource {
	t.Helper()
	const token = "ghp_armed_secret"
	job, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{
		Path: "job.hcl", Body: []byte(render(t, "build.nomad.hcl", renderData{Kind: "build", Pool: "macos"})),
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sourceSH := taskScript(t, job, "build.nomad.hcl", "source")
	root := t.TempDir()
	r := macSource{alloc: filepath.Join(root, "alloc"), sudoLog: filepath.Join(root, "sudo.log"), tcLog: filepath.Join(root, "toolchain.log")}
	sourceDir := filepath.Join(root, "source", "local")
	bin, tmp := filepath.Join(root, "bin"), filepath.Join(root, "tmp")
	for _, dir := range []string{r.alloc, sourceDir, bin, tmp} {
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
	write(filepath.Join(bin, "chown"), "#!/bin/sh\nexit 0\n")
	write(filepath.Join(bin, "sudo"), `#!/bin/bash
printf '%s\n' "$*" >> "$FAKE_SUDO_LOG"
while [ $# -gt 0 ]; do case "$1" in -n|-H) shift ;; -u) shift 2 ;; *) break ;; esac; done
if [ "$1" = /usr/local/libexec/grove-toolchain ]; then
  {
    echo "argv: $*"
    echo "cwd: $PWD"
    echo "tmp entries: $(ls -A "$TMPDIR" | wc -l | tr -d ' ')"
    echo "stdin: $(cat)"
    echo "env:"
    env
  } > "$FAKE_TC_LOG"
  [ -z "${FAKE_TC_FAIL:-}" ] || { echo "installer exploded" >&2; exit 1; }
  echo "/private/var/grove-toolchains/mise-test-$2"
  exit 0
fi
exec env -i PATH="$PATH" FAKE_GIT_LOG="$FAKE_GIT_LOG" FAKE_UNDER_SUDO=1 "$@"
`)
	write(filepath.Join(sourceDir, "request"), "grove-payload/1\n"+token+"\ntrue\n")
	write(filepath.Join(sourceDir, "source.sh"), sourceSH)
	cmd := exec.Command("/bin/bash", filepath.Join(sourceDir, "source.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+tmp, "NOMAD_ALLOC_DIR="+r.alloc, "NOMAD_TASK_DIR="+sourceDir,
		"NOMAD_META_ref=main", "FAKE_GIT_LOG="+filepath.Join(root, "git"),
		"FAKE_SUDO_LOG="+r.sudoLog, "FAKE_TC_LOG="+r.tcLog,
	)
	cmd.Env = append(cmd.Env, env...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("source.sh: %v\n%s", err, b)
	}
	return r
}

func (r macSource) file(t *testing.T, parts ...string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(append([]string{r.alloc, "data", "grove"}, parts...)...))
	return strings.TrimSpace(string(b))
}

// The installer runs once the clone is done and the token is gone, as _grovetool through sudo,
// from /, with the spec on stdin and the hash as its argument, and no trace of the token.
func TestSourceTaskMacOSInstallsToolchainAfterTokenIsGone(t *testing.T) {
	const token = "ghp_armed_secret"
	r := runMacSource(t,
		"NOMAD_META_repo=https://github.com/gm2211/private",
		"NOMAD_META_toolchain_json="+toolchainJSON, "NOMAD_META_toolchain_hash="+toolchainHash)
	if got := r.file(t, "source.exit"); got != "0" {
		t.Fatalf("source.exit = %q\n%s", got, r.file(t, "source.log"))
	}
	sudo, _ := os.ReadFile(r.sudoLog)
	lines := strings.Split(strings.TrimSpace(string(sudo)), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "-u _grovetool /usr/local/libexec/grove-toolchain "+toolchainHash) {
		t.Fatalf("the installer is not the last sudo call, or not run as _grovetool:\n%s", sudo)
	}
	if strings.Count(string(sudo), "_grovetool") != 1 || strings.Contains(string(sudo), token) {
		t.Fatalf("unexpected sudo calls:\n%s", sudo)
	}
	tc, _ := os.ReadFile(r.tcLog)
	for _, want := range []string{"argv: /usr/local/libexec/grove-toolchain " + toolchainHash, "cwd: /", "tmp entries: 0", "stdin: " + toolchainJSON} {
		if !strings.Contains(string(tc), want) {
			t.Errorf("installer saw no %q:\n%s", want, tc)
		}
	}
	if strings.Contains(string(tc), token) || strings.Contains(string(tc), "GROVE_CLONE_TOKEN") {
		t.Errorf("the clone token reached the installer's environment:\n%s", tc)
	}
	if got := r.file(t, "toolchain.dir"); got != "/private/var/grove-toolchains/mise-test-"+toolchainHash {
		t.Errorf("toolchain.dir = %q", got)
	}
}

// No repo means no clone, but the toolchain is still installed; a job without toolchain meta never
// starts the installer.
func TestSourceTaskMacOSToolchainWithoutRepoAndWithout(t *testing.T) {
	r := runMacSource(t, "NOMAD_META_toolchain_json="+toolchainJSON, "NOMAD_META_toolchain_hash="+toolchainHash)
	if r.file(t, "source.exit") != "0" || r.file(t, "toolchain.dir") == "" {
		t.Errorf("no toolchain for a repo-less job: exit %q dir %q", r.file(t, "source.exit"), r.file(t, "toolchain.dir"))
	}
	r = runMacSource(t, "NOMAD_META_repo=https://github.com/gm2211/private")
	if sudo, _ := os.ReadFile(r.sudoLog); strings.Contains(string(sudo), "grove-toolchain") {
		t.Errorf("the installer ran for a job with no toolchain:\n%s", sudo)
	}
	if _, err := os.Stat(filepath.Join(r.alloc, "data", "grove", "toolchain.dir")); err == nil {
		t.Errorf("toolchain.dir written for a job with no toolchain")
	}
}

// A failed install ends the source step with 70 and its reason in the log, which run.sh replays.
func TestSourceTaskMacOSToolchainFailureIs70(t *testing.T) {
	r := runMacSource(t, "NOMAD_META_repo=https://github.com/gm2211/private",
		"NOMAD_META_toolchain_json="+toolchainJSON, "NOMAD_META_toolchain_hash="+toolchainHash, "FAKE_TC_FAIL=1")
	if r.file(t, "source.exit") != "70" {
		t.Fatalf("source.exit = %q", r.file(t, "source.exit"))
	}
	log := r.file(t, "source.log")
	if !strings.Contains(log, "installer exploded") || !strings.Contains(log, "toolchain failed") {
		t.Errorf("log lacks the installer's output:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(r.alloc, "data", "grove", "toolchain.dir")); err == nil {
		t.Errorf("toolchain.dir written after a failure")
	}
}

// runMacMain runs the macOS run.sh against a toolchain directory made by the test, whose two
// fixed locations (the toolchain root, the job user's home) are pointed into the test's temp dir.
type macMainOpts struct {
	mk       func(dir string)         // fills the toolchain directory; nil makes none
	stateDir func(real string) string // what toolchain.dir holds; nil = the real one, "" = no file
	meta     map[string]string        // NOMAD_META_* for the task
}

type macToolchain struct{ root, dir string }

func runMacMain(t *testing.T, o macMainOpts) (code int, out string, tc macToolchain) {
	t.Helper()
	runSH := renderedRunSH(t, "build.nomad.hcl", renderData{Kind: "build", Pool: "macos"})
	base := t.TempDir()
	tc.root = filepath.Join(base, "grove-toolchains")
	home := filepath.Join(base, "home")
	for old, repl := range map[string]string{
		"tc_root=/private/var/grove-toolchains": "tc_root=" + tc.root,
		"HOME=/private/var/grove-job":           "HOME=" + home,
	} {
		if !strings.Contains(runSH, old) {
			t.Fatalf("run.sh has no %q", old)
		}
		runSH = strings.Replace(runSH, old, repl, 1)
	}
	// The rewritten root must still satisfy the script's own prefix check.
	tc.dir = filepath.Join(tc.root, "mise-test-"+toolchainHash)
	alloc := filepath.Join(base, "alloc")
	state := filepath.Join(alloc, "data", "grove")
	taskDir := filepath.Join(base, "main", "local")
	for _, d := range []string{filepath.Join(tc.root, ".use"), home, state, taskDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(state, "source.exit"), "0\n")
	write(filepath.Join(state, "source.log"), "")
	write(filepath.Join(state, "script.sh"), "echo SCRIPT_RAN\necho PATH=$PATH\necho RUSTUP_HOME=${RUSTUP_HOME:-}\necho CARGO_HOME=${CARGO_HOME:-}\necho TOOLCHAIN_DIR=${GROVE_TOOLCHAIN_DIR:-}\necho RUSTUP_TOOLCHAIN=${RUSTUP_TOOLCHAIN:-unset}\necho AUTO=${RUSTUP_AUTO_INSTALL:-}\nls "+filepath.Join(tc.root, ".use")+"\n")
	write(filepath.Join(taskDir, "run.sh"), runSH)
	if o.mk != nil {
		if err := os.MkdirAll(tc.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		o.mk(tc.dir)
	}
	recorded := tc.dir
	if o.stateDir != nil {
		recorded = o.stateDir(tc.dir)
	}
	if recorded != "" {
		write(filepath.Join(state, "toolchain.dir"), recorded+"\n")
	}
	cmd := exec.Command("/bin/bash", filepath.Join(taskDir, "run.sh"))
	cmd.Env = append(os.Environ(), "NOMAD_ALLOC_DIR="+alloc, "NOMAD_TASK_DIR="+taskDir, "NOMAD_ALLOC_ID=alloc1", "RUSTUP_TOOLCHAIN=stable")
	for k, v := range o.meta {
		cmd.Env = append(cmd.Env, "NOMAD_META_"+k+"="+v)
	}
	outFile, err := os.Create(filepath.Join(base, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	cmd.Stdout, cmd.Stderr = outFile, outFile
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	b, _ := os.ReadFile(outFile.Name())
	return code, string(b), tc
}

func TestRunSHMacOSPutsToolchainOnPath(t *testing.T) {
	code, out, tc := runMacMain(t, macMainOpts{
		mk: func(d string) {
			_ = os.WriteFile(filepath.Join(d, ".complete"), nil, 0o644)
			_ = os.WriteFile(filepath.Join(d, "paths"), []byte(d+"/cargo/bin\n"+d+"/data/installs/node/22.11.0/bin\n"), 0o644)
		},
		meta: map[string]string{"toolchain_hash": toolchainHash},
	})
	if code != 0 || !strings.Contains(out, "SCRIPT_RAN") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"PATH=" + tc.dir + "/cargo/bin:" + tc.dir + "/data/installs/node/22.11.0/bin:",
		"RUSTUP_HOME=" + tc.dir + "/rustup",
		"TOOLCHAIN_DIR=" + tc.dir,
		"RUSTUP_TOOLCHAIN=unset",
		"AUTO=0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("job env lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "CARGO_HOME=") || strings.Contains(out, "CARGO_HOME="+tc.dir) || !strings.Contains(out, "/.cargo-grove") {
		t.Errorf("CARGO_HOME must be the job's own, not inside the toolchain:\n%s", out)
	}
	// A marker tells the installer's eviction the entry is in use while the job runs, and goes after.
	if !strings.Contains(out, "mise-test-"+toolchainHash+".alloc1") {
		t.Errorf("no in-use marker while the job ran:\n%s", out)
	}
	if entries, _ := os.ReadDir(filepath.Join(tc.root, ".use")); len(entries) != 0 {
		t.Errorf("marker left behind: %v", entries)
	}
}

func TestRunSHMacOSRefusesBadToolchain(t *testing.T) {
	complete := func(d string, paths string) {
		_ = os.WriteFile(filepath.Join(d, ".complete"), nil, 0o644)
		_ = os.WriteFile(filepath.Join(d, "paths"), []byte(paths), 0o644)
	}
	cases := map[string]struct {
		mk    func(d string)
		state func(real string) string
	}{
		"path outside the directory": {mk: func(d string) { complete(d, d+"/bin\n/usr/bin\n") }},
		"dot-dot escape":             {mk: func(d string) { complete(d, d+"/../../elsewhere\n") }},
		"prefix lookalike":           {mk: func(d string) { complete(d, d+"-evil/bin\n") }},
		"colon smuggling":            {mk: func(d string) { complete(d, d+"/bin:/tmp/evil\n") }},
		"not complete":               {mk: func(d string) { _ = os.WriteFile(filepath.Join(d, "paths"), []byte(d+"/bin\n"), 0o644) }},
		"no paths file":              {mk: func(d string) { _ = os.WriteFile(filepath.Join(d, ".complete"), nil, 0o644) }},
		"directory outside the root": {mk: func(d string) { complete(d, d+"/bin\n") }, state: func(string) string { return "/tmp/evil" }},
		"no state file":              {mk: func(d string) { complete(d, d+"/bin\n") }, state: func(string) string { return "" }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, _ := runMacMain(t, macMainOpts{mk: c.mk, stateDir: c.state, meta: map[string]string{"toolchain_hash": toolchainHash}})
			if code != 70 || strings.Contains(out, "SCRIPT_RAN") {
				t.Fatalf("exit %d, the script ran or was not refused:\n%s", code, out)
			}
		})
	}
}

// A job with no toolchain meta is untouched by all of this, and the Linux branch ignores the meta.
func TestRunSHToolchainIsMacOSOnlyAndOptional(t *testing.T) {
	code, out, _ := runMacMain(t, macMainOpts{})
	if code != 0 || !strings.Contains(out, "SCRIPT_RAN") || !strings.Contains(out, "TOOLCHAIN_DIR=\n") || !strings.Contains(out, "AUTO=\n") || strings.Contains(out, "grove-toolchains") {
		t.Fatalf("a job without toolchain meta changed: exit %d\n%s", code, out)
	}
	for _, file := range []string{"build.nomad.hcl", "agent.nomad.hcl", "shell.nomad.hcl"} {
		kind := strings.TrimSuffix(file, ".nomad.hcl")
		hcl := render(t, file, renderData{Kind: kind, Pool: "linux"})
		for _, banned := range []string{"grove-toolchain", "toolchain.dir", "GROVE_TOOLCHAIN_DIR", "_grovetool", "NOMAD_META_toolchain"} {
			if strings.Contains(hcl, banned) {
				t.Errorf("%s/linux mentions %q", file, banned)
			}
		}
	}
	// Run the Linux build run.sh with the meta set: it runs the script as ever, and exports nothing.
	runSH := renderedRunSH(t, "build.nomad.hcl", renderData{Kind: "build", Pool: "linux"})
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	base := t.TempDir()
	state := filepath.Join(base, "alloc", "data", "grove")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"source.exit": "0\n", "source.log": "", "script.sh": "echo SCRIPT_RAN ${GROVE_TOOLCHAIN_DIR:-none}\n",
	} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "toolchain.dir"), []byte("/private/var/grove-toolchains/mise-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(base, "local")
	_ = os.MkdirAll(taskDir, 0o755)
	_ = os.WriteFile(filepath.Join(taskDir, "run.sh"), []byte(runSH), 0o755)
	cmd := exec.Command("/bin/bash", filepath.Join(taskDir, "run.sh"))
	cmd.Env = append(os.Environ(), "NOMAD_ALLOC_DIR="+filepath.Join(base, "alloc"), "NOMAD_TASK_DIR="+taskDir,
		"NOMAD_META_toolchain_hash="+toolchainHash, "NOMAD_META_toolchain_json="+toolchainJSON)
	b, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(b), "SCRIPT_RAN none") {
		t.Fatalf("linux run.sh with toolchain meta: %v\n%s", err, b)
	}
}
