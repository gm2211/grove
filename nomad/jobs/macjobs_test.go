package nomadjobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/jobspec2"
)

// Jobs sharing a macOS VM: every job there runs as the one job user, in a VM that is reused, so
// the templates give each job its own home and have root tasks around "main" kill what earlier
// jobs left running. These tests run the rendered scripts against stand-ins for the host's
// process tools; nothing here runs on a real macOS VM.

const jobsDirLine = "grove_jobs=/private/var/run/grove-jobs"

// withJobsDir points a rendered macOS script's record of running jobs (its grove:jobs block) into
// dir, so that a test never touches /private/var/run.
func withJobsDir(t *testing.T, script, dir string) string {
	t.Helper()
	if strings.Count(script, jobsDirLine) != 1 {
		t.Fatalf("script has %d %q lines, want 1", strings.Count(script, jobsDirLine), jobsDirLine)
	}
	script = strings.Replace(script, jobsDirLine, "grove_jobs="+dir, 1)
	// The clone token's directory: a test's own, never the host's /private/var/tmp.
	if strings.Contains(script, srcTmpLine) {
		srcTmp := filepath.Join(filepath.Dir(dir), "srctmp")
		if err := os.MkdirAll(srcTmp, 0o755); err != nil {
			t.Fatal(err)
		}
		script = strings.Replace(script, srcTmpLine, "grove_srctmp="+srcTmp, 1)
	}
	return script
}

const srcTmpLine = "grove_srctmp=/private/var/tmp"

// On macOS the clone runs as _grovesrc but the task as root, whose $TMPDIR (/var/folders/zz/…/T)
// is closed to every other user: the token's directory must not be made there, or _grovesrc
// can't read the token and every private clone fails ("cat: …/token: Permission denied").
func TestMacOSCloneTokenDirIsReachableByTheCloneUser(t *testing.T) {
	for _, file := range []string{"build.nomad.hcl", "agent.nomad.hcl"} {
		kind := strings.TrimSuffix(file, ".nomad.hcl")
		hcl := render(t, file, renderData{Kind: kind, Pool: "macos"})
		if strings.Contains(hcl, `tokdir="$(mktemp -d)"`) {
			t.Errorf("%s: the token directory is made in root's own $TMPDIR, which _grovesrc can't enter", file)
		}
		if !strings.Contains(hcl, srcTmpLine) || !strings.Contains(hcl, `mktemp -d "$grove_srctmp/grove-src.XXXXXX"`) {
			t.Errorf("%s: the token directory is not made under %s", file, srcTmpLine)
		}
	}
}

// fakeMacHost writes stand-ins for the commands a macOS job's root tasks use on the host into
// bin. They work on files under $FAKE_HOST: `procs` exists while the job user has processes,
// `ps.out` is what ps prints, and `proc.log` records every pkill.
func fakeMacHost(t *testing.T, bin string) {
	t.Helper()
	for name, body := range map[string]string{
		"chown": "#!/bin/sh\nexit 0\n",
		"id":    "#!/bin/sh\n[ \"$*\" = \"-u _grovejob\" ] || exit 1\necho 7460\n",
		"pgrep": "#!/bin/sh\n[ \"$*\" = \"-U _grovejob\" ] || exit 2\n[ -e \"$FAKE_HOST/procs\" ]\n",
		"pkill": "#!/bin/sh\necho \"pkill $*\" >> \"$FAKE_HOST/proc.log\"\nrm -f \"$FAKE_HOST/procs\"\n",
		"ps":    "#!/bin/sh\necho \"ps $*\" >> \"$FAKE_HOST/proc.log\"\ncat \"$FAKE_HOST/ps.out\" 2>/dev/null\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func parseJob(t *testing.T, kind, pool string) *nomadapi.Job {
	t.Helper()
	file := kind + ".nomad.hcl"
	job, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{
		Path: "job.hcl", Body: []byte(render(t, file, renderData{Kind: kind, Pool: pool})),
	})
	if err != nil {
		t.Fatalf("parse %s/%s: %v", kind, pool, err)
	}
	return job
}

// firstTask is the root task that runs before a macOS job's main.
func firstTask(kind string) string {
	if kind == "shell" {
		return "prepare"
	}
	return "source"
}

func jobsBlock(t *testing.T, script string) string {
	t.Helper()
	begin := strings.Index(script, "# grove:jobs begin")
	end := strings.Index(script, "# grove:jobs end")
	if begin < 0 || end < begin {
		t.Fatalf("script has no grove:jobs block")
	}
	return script[begin:end]
}

// macHost is one fake VM: the jobs directory and process state its jobs' root tasks share.
type macHost struct {
	t                *testing.T
	root, bin, jobs  string
	prepare, cleanup string // the shell job's rendered scripts
	runSH            string
}

func newMacHost(t *testing.T) *macHost {
	t.Helper()
	root := t.TempDir()
	h := &macHost{t: t, root: root, bin: filepath.Join(root, "bin"), jobs: filepath.Join(root, "grove-jobs")}
	if err := os.MkdirAll(h.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeMacHost(t, h.bin)
	// sudo -n -u _grovejob <cmd>: recorded, then run.
	sudo := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> \"$FAKE_HOST/sudo.log\"\nwhile [ $# -gt 0 ]; do case \"$1\" in -n|-H) shift ;; -u) shift 2 ;; *) break ;; esac; done\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(h.bin, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	job := parseJob(t, "shell", "macos")
	h.prepare = withJobsDir(t, taskScript(t, job, "shell.nomad.hcl", "prepare"), h.jobs)
	h.cleanup = withJobsDir(t, taskScript(t, job, "shell.nomad.hcl", "cleanup"), h.jobs)
	h.runSH = taskScript(t, job, "shell.nomad.hcl", "main")
	return h
}

func (h *macHost) allocDir(id string) string { return filepath.Join(h.root, "allocs", id, "alloc") }

// run executes one task's script for allocation id, the way Nomad lays an allocation out.
func (h *macHost) run(id, task, script string, env ...string) (int, string) {
	h.t.Helper()
	taskDir := filepath.Join(h.root, "allocs", id, task, "local")
	for _, d := range []string{taskDir, filepath.Join(h.root, "allocs", id, task, "tmp"), filepath.Join(h.allocDir(id), "data")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	path := filepath.Join(taskDir, task+".sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", path)
	cmd.Env = append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"), "FAKE_HOST="+h.root,
		"NOMAD_ALLOC_DIR="+h.allocDir(id), "NOMAD_TASK_DIR="+taskDir, "NOMAD_ALLOC_ID="+id)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	}
	h.t.Fatalf("%s: %v", task, err)
	return 0, ""
}

func (h *macHost) leaveProcesses() {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.root, "procs"), nil, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *macHost) hasProcesses() bool {
	_, err := os.Stat(filepath.Join(h.root, "procs"))
	return err == nil
}

func (h *macHost) procLog() string {
	b, _ := os.ReadFile(filepath.Join(h.root, "proc.log"))
	return string(b)
}

func (h *macHost) record(id string) (at int64, ok bool) {
	b, err := os.ReadFile(filepath.Join(h.jobs, id))
	if err != nil {
		return 0, false
	}
	at, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		h.t.Fatalf("record %s = %q", id, b)
	}
	return at, true
}

// The rendered templates: a macOS job's HOME is a directory of its own allocation, never the
// account's shared home, for every kind; a root poststop task cleans up after it, on macOS only.
func TestMacOSJobsGetTheirOwnHomeAndACleanupTask(t *testing.T) {
	var cleanups, blocks []string
	for _, kind := range []string{"build", "agent", "shell"} {
		file := kind + ".nomad.hcl"
		hcl := render(t, file, renderData{Kind: kind, Pool: "macos"})
		if strings.Contains(hcl, "/private/var/grove-job") {
			t.Errorf("%s/macos still names the job user's shared home", kind)
		}
		job := parseJob(t, kind, "macos")
		tasks := map[string]*nomadapi.Task{}
		for _, task := range job.TaskGroups[0].Tasks {
			tasks[task.Name] = task
		}

		runSH := taskScript(t, job, file, "main")
		if !strings.Contains(runSH, "export HOME=\"$state/home\" USER=_grovejob LOGNAME=_grovejob\n") ||
			!strings.Contains(runSH, "state=\"$NOMAD_ALLOC_DIR/data/grove\"\n") {
			t.Errorf("%s/macos run.sh does not take its HOME from its own allocation:\n%s", kind, runSH)
		}
		if strings.Count(runSH, "HOME=") != 1+strings.Count(runSH, "CARGO_HOME=")+strings.Count(runSH, "RUSTUP_HOME=") {
			t.Errorf("%s/macos run.sh sets HOME more than once", kind)
		}

		first := tasks[firstTask(kind)]
		if first == nil || first.User != "" || first.Driver != "raw_exec" ||
			first.Lifecycle == nil || first.Lifecycle.Hook != "prestart" || first.Lifecycle.Sidecar {
			t.Fatalf("%s/macos: task before main = %+v, want a root raw_exec prestart", kind, first)
		}
		firstSH := taskScript(t, job, file, first.Name)
		for _, want := range []string{"mkdir \"$state/home\"", "chown -R _grovejob \"$state\"", "grove_jobs_sweep\n", "grove_jobs_mark $((grove_job_secs + 900))"} {
			if !strings.Contains(firstSH, want) {
				t.Errorf("%s/macos %s.sh lacks %q", kind, first.Name, want)
			}
		}
		if kind == "shell" && first.DispatchPayload != nil {
			t.Errorf("shell prepare takes the payload; only main runs the caller's script")
		}

		cleanup := tasks["cleanup"]
		if cleanup == nil {
			t.Fatalf("%s/macos has no cleanup task", kind)
		}
		if cleanup.Lifecycle == nil || cleanup.Lifecycle.Hook != "poststop" {
			t.Errorf("%s/macos cleanup lifecycle = %+v, want poststop", kind, cleanup.Lifecycle)
		}
		if cleanup.User != "" || cleanup.Driver != "raw_exec" || cleanup.DispatchPayload != nil {
			t.Errorf("%s/macos cleanup = user %q driver %q payload %+v, want root raw_exec with no payload", kind, cleanup.User, cleanup.Driver, cleanup.DispatchPayload)
		}
		// Lifecycle tasks never run beside main, so the group needs no more than main does.
		if *cleanup.Resources.CPU != *tasks["main"].Resources.CPU || *cleanup.Resources.MemoryMB != *tasks["main"].Resources.MemoryMB {
			t.Errorf("%s/macos cleanup asks for different resources than main", kind)
		}
		cleanupSH := taskScript(t, job, file, "cleanup")
		if !strings.HasSuffix(strings.TrimSpace(cleanupSH), "exit 0") {
			t.Errorf("%s/macos cleanup.sh can fail the allocation", kind)
		}
		cleanups = append(cleanups, cleanupSH)
		blocks = append(blocks, jobsBlock(t, firstSH), jobsBlock(t, cleanupSH))

		for _, script := range []string{firstSH, runSH, cleanupSH} {
			check := exec.Command("/bin/bash", "-n", "/dev/stdin")
			check.Stdin = strings.NewReader(script)
			if out, err := check.CombinedOutput(); err != nil {
				t.Errorf("%s/macos: bash -n: %v\n%s", kind, err, out)
			}
		}

		// Linux jobs run in containers of their own: none of this is rendered for them.
		linux := render(t, file, renderData{Kind: kind, Pool: "linux"})
		for _, absent := range []string{"cleanup", "poststop", "grove:jobs", "grove_jobs", "_grovejob", "task \"prepare\""} {
			if strings.Contains(linux, absent) {
				t.Errorf("%s/linux renders %q", kind, absent)
			}
		}
	}
	// One protocol: every root task of every kind reads and writes the records the same way.
	for i := range blocks {
		if blocks[i] != blocks[0] {
			t.Errorf("grove:jobs block %d differs from the first:\n%s", i, blocks[i])
		}
	}
	for i := range cleanups {
		if cleanups[i] != cleanups[0] {
			t.Errorf("cleanup.sh of kind %d differs from build's", i)
		}
	}
}

// A shell job on macOS, start to finish: prepare makes its home and records it, run.sh runs the
// script with that home, cleanup removes the home and the record.
func TestMacOSShellJobLifecycle(t *testing.T) {
	h := newMacHost(t)
	if code, out := h.run("alloc-a", "prepare", h.prepare, "NOMAD_META_timeout_seconds=120"); code != 0 {
		t.Fatalf("prepare exit %d\n%s", code, out)
	}
	home := filepath.Join(h.allocDir("alloc-a"), "data", "grove", "home")
	info, err := os.Stat(home)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("home = %v, %v, want a 0700 directory", info, err)
	}
	if info, err := os.Stat(h.jobs); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("jobs directory = %v, %v, want 0700", info, err)
	}
	at, ok := h.record("alloc-a")
	if now := time.Now().Unix(); !ok || at < now+120+800 || at > now+120+1000 {
		t.Fatalf("record = %d, %v, want about now+timeout+900", at, ok)
	}

	taskDir := filepath.Join(h.root, "allocs", "alloc-a", "main", "local")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "script.sh"), []byte("echo home=$HOME user=$USER tmp=$TMPDIR\ntouch \"$HOME/.npmrc\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := h.run("alloc-a", "main", h.runSH, "HOME=/somewhere/shared", "NOMAD_META_timeout_seconds=120")
	if code != 0 || !strings.Contains(out, "home="+home+" user=_grovejob tmp="+filepath.Join(h.root, "allocs", "alloc-a", "main", "tmp")) {
		t.Fatalf("run.sh exit %d\n%s", code, out)
	}

	if code, out := h.run("alloc-a", "cleanup", h.cleanup); code != 0 {
		t.Fatalf("cleanup exit %d\n%s", code, out)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("cleanup left the job's home behind (stat err %v)", err)
	}
	if _, ok := h.record("alloc-a"); ok {
		t.Errorf("cleanup left the job's record behind")
	}
	sudo, _ := os.ReadFile(filepath.Join(h.root, "sudo.log"))
	if strings.TrimSpace(string(sudo)) != "-n -u _grovejob /bin/rm -rf -- "+home {
		t.Errorf("sudo calls = %q, want the home deleted as the job user", sudo)
	}
	if entries, _ := os.ReadDir(h.jobs); len(entries) != 0 {
		t.Errorf("jobs directory not empty after the only job ended: %v", entries)
	}
}

// run.sh never falls back to a shared home, and the task before it never adopts a state
// directory it did not make.
func TestMacOSJobRefusesAHomeItWasNotGiven(t *testing.T) {
	h := newMacHost(t)
	code, out := h.run("alloc-a", "main", h.runSH)
	if code != 1 || !strings.Contains(out, "no home directory of its own") {
		t.Fatalf("run.sh with no home: exit %d\n%s", code, out)
	}
	state := filepath.Join(h.allocDir("alloc-a"), "data", "grove")
	target := filepath.Join(h.root, "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(state, "home")); err != nil {
		t.Fatal(err)
	}
	if code, out := h.run("alloc-a", "main", h.runSH); code != 1 || !strings.Contains(out, "no home directory of its own") {
		t.Fatalf("run.sh with a symlinked home: exit %d\n%s", code, out)
	}

	// Someone else made this allocation's state directory first (with a home in it).
	if code, out := h.run("alloc-a", "prepare", h.prepare); code != 1 || !strings.Contains(out, "refusing to run a job in it") {
		t.Fatalf("prepare over an existing state directory: exit %d\n%s", code, out)
	}
	if _, ok := h.record("alloc-a"); ok {
		t.Errorf("a refused job was recorded as running")
	}

	for _, kind := range []string{"build", "agent"} {
		job := parseJob(t, kind, "macos")
		sourceSH := withJobsDir(t, taskScript(t, job, kind+".nomad.hcl", "source"), h.jobs)
		id := "alloc-" + kind
		if err := os.MkdirAll(filepath.Join(h.allocDir(id), "data", "grove"), 0o755); err != nil {
			t.Fatal(err)
		}
		if code, out := h.run(id, "source", sourceSH); code != 1 || !strings.Contains(out, "refusing to run a job in it") {
			t.Errorf("%s source over an existing state directory: exit %d\n%s", kind, code, out)
		}
	}
}

// What an earlier job left running is killed before the next job starts and after a job ends,
// but only while no other job is running in the VM: those share the job user and can't be told
// apart from leftovers.
func TestMacOSJobsSweepLeftoversOnlyWhenTheVMIsIdle(t *testing.T) {
	t.Run("before a job starts on an idle VM", func(t *testing.T) {
		h := newMacHost(t)
		h.leaveProcesses()
		if code, out := h.run("alloc-b", "prepare", h.prepare); code != 0 {
			t.Fatalf("prepare exit %d\n%s", code, out)
		}
		if h.hasProcesses() || !strings.Contains(h.procLog(), "pkill -KILL -U _grovejob") {
			t.Fatalf("leftover processes survived the next job's start; calls:\n%s", h.procLog())
		}
	})

	t.Run("never while another job is running", func(t *testing.T) {
		h := newMacHost(t)
		if code, out := h.run("alloc-a", "prepare", h.prepare); code != 0 {
			t.Fatalf("prepare a: exit %d\n%s", code, out)
		}
		h.leaveProcesses() // job a's own, running
		if code, out := h.run("alloc-b", "prepare", h.prepare); code != 0 {
			t.Fatalf("prepare b: exit %d\n%s", code, out)
		}
		if code, out := h.run("alloc-b", "cleanup", h.cleanup); code != 0 {
			t.Fatalf("cleanup b: exit %d\n%s", code, out)
		}
		if !h.hasProcesses() || strings.Contains(h.procLog(), "pkill") {
			t.Fatalf("a running job's processes were killed; calls:\n%s", h.procLog())
		}
		if _, ok := h.record("alloc-a"); !ok {
			t.Fatalf("job b removed job a's record")
		}
		// Job a ends: now the VM is idle and everything of the job user goes.
		if code, out := h.run("alloc-a", "cleanup", h.cleanup); code != 0 {
			t.Fatalf("cleanup a: exit %d\n%s", code, out)
		}
		if h.hasProcesses() {
			t.Fatalf("leftovers survived the last job's cleanup; calls:\n%s", h.procLog())
		}
	})

	t.Run("a record whose time is up does not hold the sweep off", func(t *testing.T) {
		h := newMacHost(t)
		if err := os.MkdirAll(h.jobs, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"dead-alloc":    strconv.FormatInt(time.Now().Unix()-5, 10) + "\n",
			"garbage-alloc": "soon\n",
		} {
			if err := os.WriteFile(filepath.Join(h.jobs, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		h.leaveProcesses()
		if code, out := h.run("alloc-b", "prepare", h.prepare); code != 0 {
			t.Fatalf("prepare exit %d\n%s", code, out)
		}
		if h.hasProcesses() {
			t.Fatalf("a dead job's record kept leftovers alive")
		}
		entries, _ := os.ReadDir(h.jobs)
		if len(entries) != 1 || entries[0].Name() != "alloc-b" {
			t.Fatalf("jobs directory = %v, want only this job's record", entries)
		}
	})

	t.Run("a lock its holder never released is taken over", func(t *testing.T) {
		if testing.Short() {
			t.Skip("waits 30 seconds")
		}
		h := newMacHost(t)
		if err := os.MkdirAll(filepath.Join(h.jobs, ".lock"), 0o700); err != nil {
			t.Fatal(err)
		}
		h.leaveProcesses()
		if code, out := h.run("alloc-b", "prepare", h.prepare); code != 0 {
			t.Fatalf("prepare exit %d\n%s", code, out)
		}
		if _, ok := h.record("alloc-b"); !ok || h.hasProcesses() {
			t.Fatalf("a stale lock stopped the job being recorded or the sweep")
		}
		if _, err := os.Stat(filepath.Join(h.jobs, ".lock")); !os.IsNotExist(err) {
			t.Fatalf("the lock was left held")
		}
	})

	t.Run("a jobs directory that is not root's own is not used", func(t *testing.T) {
		h := newMacHost(t)
		target := filepath.Join(h.root, "planted")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, h.jobs); err != nil {
			t.Fatal(err)
		}
		h.leaveProcesses()
		code, out := h.run("alloc-b", "prepare", h.prepare)
		if code != 0 || !strings.Contains(out, "could not record this job") {
			t.Fatalf("prepare exit %d\n%s", code, out)
		}
		if strings.Contains(h.procLog(), "pkill") {
			t.Fatalf("swept on the word of a directory that isn't root's")
		}
		if entries, _ := os.ReadDir(target); len(entries) != 0 {
			t.Fatalf("wrote through the symlink: %v", entries)
		}
	})
}

// cleanup kills the job user's processes that carry this allocation's id in their environment,
// whatever else is running, and nothing else: not another job's, and not another user's process
// that happens to carry the id (this task's own root processes do).
func TestMacOSCleanupKillsThisJobsOwnLeftovers(t *testing.T) {
	h := newMacHost(t)
	if code, out := h.run("alloc-a", "prepare", h.prepare); code != 0 {
		t.Fatalf("prepare a: exit %d\n%s", code, out)
	}
	if code, out := h.run("alloc-b", "prepare", h.prepare); code != 0 {
		t.Fatalf("prepare b: exit %d\n%s", code, out)
	}
	// pids no process can have, so the script's real `kill` finds nothing.
	ps := strings.Join([]string{
		" 99999991  7460 /usr/bin/node server.js PATH=/usr/bin NOMAD_ALLOC_ID=alloc-b HOME=/x",
		" 99999992  7460 sleep 600 NOMAD_ALLOC_ID=alloc-a HOME=/y",
		" 99999993     0 /bin/bash cleanup.sh NOMAD_ALLOC_ID=alloc-b",
		" 99999994  7460 sh -c x NOMAD_ALLOC_ID=alloc-bb",
		" 99999995  7460 daemon XNOMAD_ALLOC_ID=alloc-b",
		" 99999996  7460 /usr/bin/python3 -m http.server NOMAD_ALLOC_ID=alloc-b",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(h.root, "ps.out"), []byte(ps), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := h.run("alloc-b", "cleanup", h.cleanup)
	if code != 0 {
		t.Fatalf("cleanup exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "killing processes this job left behind: 99999991 99999996\n") {
		t.Fatalf("cleanup output:\n%s\nwant exactly this job's two processes killed", out)
	}
	if !strings.Contains(h.procLog(), "ps -x -ww -E -U _grovejob -o pid=,uid=,command=") {
		t.Errorf("ps calls:\n%s", h.procLog())
	}
	if strings.Contains(h.procLog(), "pkill") {
		t.Errorf("cleanup swept the job user while job a was running:\n%s", h.procLog())
	}
}

// A build's source task does the same as a shell job's prepare, and resets the record's time
// when it hands over to main.
func TestMacOSSourceTaskRecordsTheJobAndMakesItsHome(t *testing.T) {
	for _, kind := range []string{"build", "agent"} {
		h := newMacHost(t)
		job := parseJob(t, kind, "macos")
		sourceSH := withJobsDir(t, taskScript(t, job, kind+".nomad.hcl", "source"), h.jobs)
		taskDir := filepath.Join(h.root, "allocs", "alloc-a", "source", "local")
		if err := os.MkdirAll(taskDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(taskDir, "request"), []byte("grove-payload/1\n\necho hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		h.leaveProcesses()
		if code, out := h.run("alloc-a", "source", sourceSH, "NOMAD_META_timeout_seconds=60"); code != 0 {
			t.Fatalf("%s source exit %d\n%s", kind, code, out)
		}
		state := filepath.Join(h.allocDir("alloc-a"), "data", "grove")
		if b, _ := os.ReadFile(filepath.Join(state, "source.exit")); strings.TrimSpace(string(b)) != "0" {
			log, _ := os.ReadFile(filepath.Join(state, "source.log"))
			t.Fatalf("%s source.exit = %q\n%s", kind, b, log)
		}
		if info, err := os.Stat(filepath.Join(state, "home")); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: home = %v, %v, want a 0700 directory", kind, info, err)
		}
		if h.hasProcesses() {
			t.Errorf("%s: leftover processes survived the next job's start", kind)
		}
		at, ok := h.record("alloc-a")
		if now := time.Now().Unix(); !ok || at < now+60+800 || at > now+60+1000 {
			t.Errorf("%s: record = %d, %v, want about now+timeout+900 once source is done", kind, at, ok)
		}
		if entries, _ := os.ReadDir(h.jobs); len(entries) != 1 {
			t.Errorf("%s: jobs directory = %v, want only the record", kind, entries)
		}
	}
}
