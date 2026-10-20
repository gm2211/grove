package dispatch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/artifacts"
)

func TestSubmit_Validation(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)

	cases := []struct {
		name string
		req  JobRequest
	}{
		{"missing kind", JobRequest{Pool: "linux", Script: "true"}},
		{"unknown kind", JobRequest{Kind: "bogus", Pool: "linux", Script: "true"}},
		{"missing pool", JobRequest{Kind: KindShell, Script: "true"}},
		{"unknown pool", JobRequest{Kind: KindShell, Pool: "gpu", Script: "true"}},
		{"missing script", JobRequest{Kind: KindShell, Pool: "linux"}},
		{"missing repo for build", JobRequest{Kind: KindBuild, Pool: "linux", Script: "true"}},
		{"missing repo for agent", JobRequest{Kind: KindAgent, Pool: "linux", Script: "true"}},
		{"sub-second timeout", JobRequest{Kind: KindShell, Pool: "linux", Script: "true", Timeout: Duration(500 * time.Millisecond)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := svc.Submit(context.Background(), c.req); err == nil {
				t.Fatal("expected a validation error, got nil")
			}
		})
	}
	exec.none(t)
	if jobs, _ := svc.List(context.Background()); len(jobs) != 0 {
		t.Fatalf("rejected requests were recorded: %+v", jobs)
	}
}

func TestSubmit_RejectsSubSecondTimeout(t *testing.T) {
	svc := newTestService(t, newFakeExec())
	_, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true", Timeout: Duration(100 * time.Millisecond)})
	wantMsg := `timeout must be at least 1s (send a duration string like "30m" or a number of seconds)`
	if err == nil || !strings.Contains(err.Error(), wantMsg) {
		t.Fatalf("error = %v, want it to contain %q", err, wantMsg)
	}
	for _, d := range []time.Duration{0, time.Second} {
		if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true", Timeout: Duration(d)}); err != nil {
			t.Fatalf("timeout %s should be accepted: %v", d, err)
		}
	}
}

func TestSubmit_ResourceHints(t *testing.T) {
	svc := newTestService(t, newFakeExec())
	if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "macos", Script: "true", Resources: &ResourceHint{CPU: 4000}}); err == nil {
		t.Fatal("a hint exceeding the pool's job CPU was accepted")
	}
	if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "macos", Script: "true", Resources: &ResourceHint{CPU: 250, Memory: 512}}); err != nil {
		t.Fatalf("a hint within the pool's sizing was rejected: %v", err)
	}
}

func TestSubmit_QueuesAndHandsTheExecutorTheRealRequest(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	req := JobRequest{
		Kind: KindBuild, Pool: "linux", Repo: "https://example.com/repo.git", Ref: "main",
		Script: "make test", Env: map[string]string{"FOO": "bar"}, Timeout: Duration(30 * time.Second),
		Meta: map[string]string{"bead": "123"}, Requester: "cli",
	}
	job := submit(t, svc, req)
	if job.Status != StatusPending || job.ID == "" {
		t.Fatalf("submitted job = %+v, want a pending job with an id", job)
	}
	if job.Request.Env["FOO"] != RedactedEnvValue {
		t.Fatalf("returned env = %v, want redacted", job.Request.Env)
	}

	fr := exec.next(t)
	run := fr.run
	if run.ID != job.ID || run.Request.Env["FOO"] != "bar" || run.Request.Script != "make test" || run.Request.Ref != "main" {
		t.Fatalf("executor got %+v", run.Request)
	}
	if run.Pool.Name != "linux" || run.Pool.Slots != 2 {
		t.Fatalf("executor got pool %+v", run.Pool)
	}
	if run.ArtifactPrefix != "jobs/"+job.ID+"/" {
		t.Fatalf("artifact prefix = %q", run.ArtifactPrefix)
	}
	got, _ := svc.Get(context.Background(), job.ID)
	if got.Status != StatusPending || !strings.Contains(got.PendingReason, "free slot") {
		t.Fatalf("before placement: status %s reason %q", got.Status, got.PendingReason)
	}

	fr.place <- "linux-mac1-0"
	got = waitStatus(t, svc, job.ID, StatusRunning)
	if got.Node != "linux-mac1-0" || got.Placement == nil || got.Placement.VMID != "linux-mac1-0" || got.Placement.WorkerID != "mac1" || got.StartedAt == nil {
		t.Fatalf("running job = %+v placement %+v", got, got.Placement)
	}
	fr.finish(t, 0)
	got = waitStatus(t, svc, job.ID, StatusSuccess)
	if got.ExitCode == nil || *got.ExitCode != 0 || got.FinishedAt == nil {
		t.Fatalf("finished job = %+v", got)
	}
}

func TestSubmit_IdempotencyKey(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	req := shellJob("linux")
	req.IdempotencyKey = "orch:t1:r1"
	first := submit(t, svc, req)
	second, created, err := svc.Submit(context.Background(), req)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("repeat Submit = %v created=%v err=%v, want the first job", second, created, err)
	}
	exec.next(t)
	exec.none(t)
}

// Jobs of a pool start in submission order, one looking for a VM slot at a time: the next only
// goes once the previous has its slot. Pools don't wait on each other.
func TestQueue_FIFOPerPoolAndPoolsIndependent(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	a := submit(t, svc, shellJob("linux"))
	b := submit(t, svc, shellJob("linux"))
	c := submit(t, svc, shellJob("linux"))
	m := submit(t, svc, shellJob("macos"))

	got := map[string]*fakeRun{}
	for range 2 {
		fr := exec.next(t)
		got[fr.run.ID] = fr
	}
	if got[a.ID] == nil || got[m.ID] == nil {
		t.Fatalf("first runs = %v, want the first linux job and the macos job", keys(got))
	}
	exec.none(t)
	if job, _ := svc.Get(context.Background(), c.ID); !strings.Contains(job.PendingReason, "queued behind 2") {
		t.Fatalf("third job's pending reason = %q", job.PendingReason)
	}

	got[a.ID].place <- "linux-mac1-0"
	frB := exec.next(t)
	if frB.run.ID != b.ID {
		t.Fatalf("second linux run = %s, want %s", frB.run.ID, b.ID)
	}
	exec.none(t)
	// b exits without ever getting a slot (no VM came up): c goes next.
	frB.finish(t, ExitLost)
	if job := waitStatus(t, svc, b.ID, StatusLost); job.FailureReason == nil {
		t.Fatal("a lost job has no failure reason")
	}
	if frC := exec.next(t); frC.run.ID != c.ID {
		t.Fatalf("third linux run = %s, want %s", frC.run.ID, c.ID)
	}
}

func keys(m map[string]*fakeRun) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		code     int
		status   Status
		timedOut bool
		signal   string
	}{
		{0, StatusSuccess, false, ""},
		{7, StatusFailed, false, ""},
		{ExitTimedOut, StatusFailed, true, ""},
		{ExitLost, StatusLost, false, ""},
		{137, StatusFailed, false, "SIGKILL"},
	}
	for _, c := range cases {
		t.Run(string(c.status)+"/"+strconv.Itoa(c.code), func(t *testing.T) {
			exec := newFakeExec()
			svc := newTestService(t, exec)
			job := submit(t, svc, shellJob("linux"))
			fr := exec.next(t)
			fr.place <- "linux-mac1-0"
			waitStatus(t, svc, job.ID, StatusRunning)
			fr.finish(t, c.code)
			got := waitStatus(t, svc, job.ID, c.status)
			if *got.ExitCode != c.code || got.TimedOut != c.timedOut {
				t.Fatalf("job = exit %d timedOut %v", *got.ExitCode, got.TimedOut)
			}
			if (got.Signal == nil) != (c.signal == "") || (got.Signal != nil && *got.Signal != c.signal) {
				t.Fatalf("signal = %v, want %q", got.Signal, c.signal)
			}
		})
	}
}

func TestCancel_RunningJob(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	job := submit(t, svc, shellJob("linux"))
	fr := exec.next(t)
	fr.place <- "linux-mac1-0"
	waitStatus(t, svc, job.ID, StatusRunning)

	if err := svc.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-fr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the executor's context was not canceled")
	}
	got := waitStatus(t, svc, job.ID, StatusCanceled)
	deadline := time.Now().Add(5 * time.Second)
	for got.ExitCode == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		got, _ = svc.Get(context.Background(), job.ID)
	}
	if got.ExitCode == nil || *got.ExitCode != ExitCanceled {
		t.Fatalf("canceled job exit = %v, want %d", got.ExitCode, ExitCanceled)
	}
	// Canceling again, or a finished job, changes nothing.
	if err := svc.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
}

func TestCancel_QueuedJobNeverRuns(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	a := submit(t, svc, shellJob("linux"))
	b := submit(t, svc, shellJob("linux"))
	frA := exec.next(t)
	if err := svc.Cancel(context.Background(), b.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitStatus(t, svc, b.ID, StatusCanceled)
	if _, ok := svc.spool.get(b.ID); ok {
		t.Fatal("a canceled job's queue entry survived")
	}
	frA.place <- "linux-mac1-0"
	frA.finish(t, 0)
	waitStatus(t, svc, a.ID, StatusSuccess)
	exec.none(t)
	if err := svc.Cancel(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cancel(unknown) = %v, want ErrNotFound", err)
	}
	// A finished job stays as it ended.
	if err := svc.Cancel(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, a.ID, StatusSuccess)
}

// The executor enforces a job's timeout itself (exit 124); should it not stop the job, the service
// does, a backstop past the timeout, and reports it timed out.
func TestTimeoutBackstop(t *testing.T) {
	exec := newFakeExec()
	svc := newTestServiceWith(t, exec, Options{TimeoutBackstop: 10 * time.Millisecond})
	req := shellJob("linux")
	req.Timeout = Duration(time.Second)
	job := submit(t, svc, req)
	fr := exec.next(t)
	fr.place <- "linux-mac1-0"
	select {
	case <-fr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the backstop did not stop the job")
	}
	got := waitStatus(t, svc, job.ID, StatusFailed)
	if !got.TimedOut || *got.ExitCode != ExitTimedOut {
		t.Fatalf("job = timedOut %v exit %d, want a timed-out 124", got.TimedOut, *got.ExitCode)
	}
}

// Exit 75 before the job got a VM means "try again": it goes back to the head of its queue. After
// it started, 75 is just the job's own exit code.
func TestTempFailRequeuesOnlyBeforeStart(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	a := submit(t, svc, shellJob("linux"))
	b := submit(t, svc, shellJob("linux"))

	fr := exec.next(t)
	fr.finish(t, ExitTempFail)
	fr = exec.next(t)
	if fr.run.ID != a.ID {
		t.Fatalf("after exit 75 the executor got %s, want %s again (ahead of %s)", fr.run.ID, a.ID, b.ID)
	}
	if job, _ := svc.Get(context.Background(), a.ID); job.Status != StatusPending {
		t.Fatalf("requeued job status = %s", job.Status)
	}
	fr.place <- "linux-mac1-0"
	waitStatus(t, svc, a.ID, StatusRunning)
	fr.finish(t, ExitTempFail)
	got := waitStatus(t, svc, a.ID, StatusFailed)
	if *got.ExitCode != ExitTempFail {
		t.Fatalf("exit = %d", *got.ExitCode)
	}

	// Retries are bounded.
	frB := exec.next(t)
	for range maxTempFailRetries {
		frB.finish(t, ExitTempFail)
		frB = exec.next(t)
	}
	frB.finish(t, ExitTempFail)
	if got := waitStatus(t, svc, b.ID, StatusFailed); *got.ExitCode != ExitTempFail {
		t.Fatalf("exit = %d", *got.ExitCode)
	}
}

// A restart of `grove serve` loses the jobs that were running (and recycles their VMs) and requeues
// the ones still waiting, in order and with their env.
func TestRestart_RunningLostQueuedRequeued(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	exec1 := newFakeExec()
	svc1 := newTestServiceWith(t, exec1, Options{StorePath: path})
	a := submit(t, svc1, shellJob("linux"))
	reqB := shellJob("linux")
	reqB.Env = map[string]string{"SECRET": "value-b"}
	b := submit(t, svc1, reqB)
	c := submit(t, svc1, shellJob("linux"))
	frA := exec1.next(t)
	frA.place <- "linux-mac1-0"
	waitStatus(t, svc1, a.ID, StatusRunning)
	exec1.next(t) // b is looking for a slot
	svc1.Close()  // the process dies: nothing records an end

	// A job an older grove handed to Nomad.
	store, err := newStore(path)
	mustf(t, err, "open store")
	mustf(t, store.put(&record{Job: Job{ID: "legacy", Status: StatusRunning, SubmittedAt: time.Now()}, LegacyNomadJobID: "grove-shell-linux/dispatch-1"}), "put")

	exec2 := newFakeExec()
	svc2 := newTestServiceWith(t, exec2, Options{StorePath: path})
	got := waitStatus(t, svc2, a.ID, StatusLost)
	if got.FailureReason == nil || !strings.Contains(*got.FailureReason, "restarted") {
		t.Fatalf("lost job reason = %v", got.FailureReason)
	}
	if got := waitStatus(t, svc2, "legacy", StatusLost); !strings.Contains(*got.FailureReason, "Nomad") {
		t.Fatalf("legacy job reason = %q", *got.FailureReason)
	}

	frB := exec2.next(t)
	if frB.run.ID != b.ID || frB.run.Request.Env["SECRET"] != "value-b" {
		t.Fatalf("first requeued run = %s env %v, want %s with its env", frB.run.ID, frB.run.Request.Env, b.ID)
	}
	exec2.mu.Lock()
	abandoned := append([]Placement(nil), exec2.abandoned...)
	exec2.mu.Unlock()
	if len(abandoned) != 1 || abandoned[0].VMID != "linux-mac1-0" {
		t.Fatalf("abandoned = %+v, want the lost job's VM", abandoned)
	}
	frB.place <- "linux-mac1-1"
	if frC := exec2.next(t); frC.run.ID != c.ID {
		t.Fatalf("second requeued run = %s, want %s", frC.run.ID, c.ID)
	}
}

func TestStore_PersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	exec := newFakeExec()
	svc1 := newTestServiceWith(t, exec, Options{StorePath: path})
	job := submit(t, svc1, shellJob("linux"))
	fr := exec.next(t)
	fr.place <- "linux-mac1-0"
	fr.finish(t, 0)
	waitStatus(t, svc1, job.ID, StatusSuccess)
	svc1.Close()

	svc2 := newTestServiceWith(t, newFakeExec(), Options{StorePath: path})
	if got := waitStatus(t, svc2, job.ID, StatusSuccess); got.ID != job.ID {
		t.Fatalf("job after restart = %+v", got)
	}
}

func TestArtifactsListedWhenTheJobEnds(t *testing.T) {
	exec := newFakeExec()
	fa := &fakeArtifacts{objectsByPrefix: map[string][]artifacts.Object{}}
	svc := newTestServiceWith(t, exec, Options{Artifacts: fa})
	job := submit(t, svc, JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://example.com/r", Script: "make"})
	fa.objectsByPrefix[artifactPrefix(job.ID)] = []artifacts.Object{{Path: "dist/app bin", Size: 3, ContentType: "application/octet-stream"}}
	fr := exec.next(t)
	fr.place <- "linux-mac1-0"
	fr.finish(t, 0)
	got := waitStatus(t, svc, job.ID, StatusSuccess)
	if len(got.Artifacts) != 1 || got.Artifacts[0].URL != "/api/v1/jobs/"+job.ID+"/artifacts/dist/app%20bin" {
		t.Fatalf("artifacts = %+v", got.Artifacts)
	}
}

func TestLogs_QueuedJobHasNoOutputYet(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	submit(t, svc, shellJob("linux"))
	queued := submit(t, svc, shellJob("linux"))
	exec.next(t)
	if _, err := svc.Logs(context.Background(), queued.ID, false); !errors.Is(err, ErrNoAllocationYet) {
		t.Fatalf("Logs = %v, want ErrNoAllocationYet", err)
	}
	if _, err := svc.LogLines(context.Background(), queued.ID, false); !errors.Is(err, ErrNoAllocationYet) {
		t.Fatalf("LogLines = %v, want ErrNoAllocationYet", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := svc.Logs(ctx, queued.ID, true); !errors.Is(err, ErrNoAllocationYet) {
		t.Fatalf("follow Logs past its deadline = %v, want ErrNoAllocationYet", err)
	}
	if _, err := svc.Logs(context.Background(), "nope", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Logs(unknown) = %v, want ErrNotFound", err)
	}
}

func TestLogs_WrittenToAFileAndReadBack(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	job := submit(t, svc, shellJob("linux"))
	fr := exec.next(t)
	fr.place <- "linux-mac1-0"
	fr.write(t, func(r *Run) {
		io.WriteString(r.Stdout, "one\ntw")
		io.WriteString(r.Stderr, "warn\r\n")
		io.WriteString(r.Stdout, "o\nno newline")
	})
	fr.finish(t, 0)
	waitStatus(t, svc, job.ID, StatusSuccess)

	info, err := os.Stat(svc.logPath(job.ID))
	mustf(t, err, "stat log")
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode = %v, want 0600", info.Mode().Perm())
	}

	rc, err := svc.Logs(context.Background(), job.ID, false)
	mustf(t, err, "Logs")
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "one\nwarn\ntwo\nno newline\n" {
		t.Fatalf("Logs = %q", data)
	}

	lines, err := svc.LogLines(context.Background(), job.ID, true)
	mustf(t, err, "LogLines")
	var got []string
	for ln := range lines {
		got = append(got, ln.Stream+":"+ln.Line)
		if ln.Time.IsZero() {
			t.Fatal("a line without a time")
		}
	}
	if strings.Join(got, ",") != "stdout:one,stderr:warn,stdout:two,stdout:no newline" {
		t.Fatalf("LogLines = %v", got)
	}
}

// A follow stream waits for the job to start, delivers lines as they are written, and ends on its
// own once the job is over.
func TestLogs_FollowStreamsLiveAndEndsWithTheJob(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	first := submit(t, svc, shellJob("linux"))
	job := submit(t, svc, shellJob("linux"))
	frFirst := exec.next(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		lines <-chan LogLine
		err   error
	}
	opened := make(chan result, 1)
	go func() {
		lines, err := svc.LogLines(ctx, job.ID, true)
		opened <- result{lines, err}
	}()
	select {
	case r := <-opened:
		t.Fatalf("follow on a queued job returned early: %v", r.err)
	case <-time.After(30 * time.Millisecond):
	}

	frFirst.place <- "linux-mac1-0"
	fr := exec.next(t)
	r := <-opened
	mustf(t, r.err, "LogLines")
	fr.place <- "linux-mac1-1"
	fr.write(t, func(r *Run) { io.WriteString(r.Stdout, "hello\n") })
	select {
	case ln := <-r.lines:
		if ln.Line != "hello" {
			t.Fatalf("line = %q", ln.Line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a live line was not delivered")
	}
	fr.write(t, func(r *Run) { io.WriteString(r.Stderr, "last words") })
	fr.finish(t, 3)
	var rest []string
	for ln := range r.lines {
		rest = append(rest, ln.Line)
	}
	if strings.Join(rest, ",") != "last words" {
		t.Fatalf("lines after the job ended = %v", rest)
	}
	if ctx.Err() != nil {
		t.Fatal("the stream only ended at its deadline")
	}
	_ = first
}

func TestLogs_ExpireWithArtifactRetention(t *testing.T) {
	exec := newFakeExec()
	svc := newTestService(t, exec)
	ok := submit(t, svc, shellJob("linux"))
	fr := exec.next(t)
	fr.place <- "linux-mac1-0"
	fr.write(t, func(r *Run) { io.WriteString(r.Stdout, "x\n") })
	fr.finish(t, 0)
	failed := submit(t, svc, shellJob("linux"))
	fr = exec.next(t)
	fr.place <- "linux-mac1-0"
	fr.write(t, func(r *Run) { io.WriteString(r.Stdout, "y\n") })
	fr.finish(t, 1)
	waitStatus(t, svc, ok.ID, StatusSuccess)
	waitStatus(t, svc, failed.ID, StatusFailed)
	orphan := filepath.Join(svc.logDir, "orphan.log")
	mustf(t, os.WriteFile(orphan, nil, 0o600), "orphan")

	now := time.Now().UTC()
	svc.expireLogs(now.Add(10 * time.Minute))
	for _, id := range []string{ok.ID, failed.ID} {
		if _, err := os.Stat(svc.logPath(id)); err != nil {
			t.Fatalf("log of %s expired before its time", id)
		}
	}

	svc.expireLogs(now.Add(20 * time.Minute)) // past 15m: the success's log goes
	if _, err := os.Stat(svc.logPath(ok.ID)); !os.IsNotExist(err) {
		t.Fatal("the successful job's log outlived 15 minutes")
	}
	if _, err := os.Stat(svc.logPath(failed.ID)); err != nil {
		t.Fatal("the failed job's log expired before an hour")
	}
	got, _ := svc.Get(context.Background(), ok.ID)
	if got.LogsExpiredAt == nil {
		t.Fatal("LogsExpiredAt not recorded")
	}
	rc, err := svc.Logs(context.Background(), ok.ID, true)
	mustf(t, err, "Logs after expiry")
	if data, _ := io.ReadAll(rc); len(data) != 0 {
		t.Fatalf("expired log still served: %q", data)
	}

	svc.expireLogs(now.Add(2 * time.Hour))
	for _, p := range []string{svc.logPath(failed.ID), orphan} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s outlived an hour", p)
		}
	}
}

func TestList_ReturnsAllJobs(t *testing.T) {
	svc := newTestService(t, newFakeExec())
	submit(t, svc, shellJob("linux"))
	submit(t, svc, shellJob("macos"))
	jobs, err := svc.List(context.Background())
	if err != nil || len(jobs) != 2 {
		t.Fatalf("List = %d jobs, %v", len(jobs), err)
	}
	if _, err := svc.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(unknown) = %v", err)
	}
}
