package dispatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRepoAuth is an armed private-repo credential for exactly one repo URL.
type fakeRepoAuth struct {
	repo  string
	token string

	mu    sync.Mutex
	calls []string
}

func (f *fakeRepoAuth) TokenFor(repoURL string) (string, bool) {
	f.mu.Lock()
	f.calls = append(f.calls, repoURL)
	f.mu.Unlock()
	if repoURL != f.repo {
		return "", false
	}
	return f.token, true
}

func (f *fakeRepoAuth) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// runJob submits req, lets it start, has the executor echo everything it was handed into the
// job's log (as a careless executor might), and finishes it. It returns the run the executor got.
func runJob(t *testing.T, svc *service, exec *fakeExec, req JobRequest) (*Job, *Run) {
	t.Helper()
	job := submit(t, svc, req)
	fr := exec.next(t)
	fr.place <- "linux-mac1-0"
	fr.write(t, func(r *Run) {
		for k, v := range r.Request.Env {
			io.WriteString(r.Stdout, k+"="+v+"\n")
		}
		io.WriteString(r.Stdout, r.Request.Script+"\n")
	})
	fr.finish(t, 0)
	got := waitStatus(t, svc, job.ID, StatusSuccess)
	return got, fr.run
}

// The armed credential reaches the executor as Run.CloneToken — the control plane's clone — and
// nothing else: not the job's env (which goes into the VM), not the job record, the history on
// disk, the queue spool, or the job's log.
func TestRepoAuth_TokenOnlyForTheClone(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "jobs.json")
	exec := newFakeExec()
	auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_secret"}
	svc := newTestServiceWith(t, exec, Options{StorePath: storePath, RepoAuth: auth})

	req := JobRequest{
		Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove", Ref: "main",
		Script: "make test", Env: map[string]string{"CI": "1"}, Requester: "cli",
	}
	job, run := runJob(t, svc, exec, req)
	if run.CloneToken != "ghp_secret" {
		t.Fatalf("CloneToken = %q, want the armed token", run.CloneToken)
	}
	for k, v := range run.Request.Env {
		if strings.Contains(v, "ghp_secret") {
			t.Fatalf("the armed token reached the job's env (%s), which enters the VM", k)
		}
	}
	if run.Request.Env["CI"] != "1" {
		t.Fatalf("the caller's env was lost: %v", run.Request.Env)
	}
	if _, ok := req.Env["GH_TOKEN"]; ok {
		t.Fatal("Submit mutated the caller's Env map")
	}
	if !job.RepoCredentialUsed {
		t.Fatal("Job.RepoCredentialUsed is false though a credential was used")
	}
	if _, ok := job.Request.Env["GH_TOKEN"]; ok {
		t.Fatalf("the returned Job carries a credential: %v", job.Request.Env)
	}

	// Nothing grove writes to disk holds it: history, queue spool, logs.
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "ghp_secret") {
			t.Errorf("the credential was written to %s", path)
		}
		return nil
	})
	mustf(t, err, "walk state dir")
}

// The token is resolved when the job starts, so a queued job is armed or not by what holds then,
// and the queue spool never needs it.
func TestRepoAuth_ResolvedWhenTheJobStarts(t *testing.T) {
	exec := newFakeExec()
	auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_secret"}
	svc := newTestServiceWith(t, exec, Options{RepoAuth: auth})
	submit(t, svc, shellJob("linux"))
	build := JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove", Script: "true"}
	queued := submit(t, svc, build)
	frShell := exec.next(t)
	if n := auth.callCount(); n != 0 {
		t.Fatalf("the store was consulted %d times before the build started", n)
	}
	if _, ok := svc.spool.get(queued.ID); !ok {
		t.Fatal("the queued job has no spool entry")
	}
	frShell.place <- "linux-mac1-0"
	fr := exec.next(t)
	if fr.run.CloneToken != "ghp_secret" {
		t.Fatalf("CloneToken = %q", fr.run.CloneToken)
	}
}

func TestRepoAuth_NoCredentialWhenNotArmedOrOutOfScope(t *testing.T) {
	t.Run("no RepoAuth configured", func(t *testing.T) {
		exec := newFakeExec()
		svc := newTestService(t, exec)
		job, run := runJob(t, svc, exec, JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove", Script: "true"})
		if run.CloneToken != "" || job.RepoCredentialUsed {
			t.Fatalf("token %q used %v with nothing armed", run.CloneToken, job.RepoCredentialUsed)
		}
	})
	t.Run("repo out of scope", func(t *testing.T) {
		exec := newFakeExec()
		auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_secret"}
		svc := newTestServiceWith(t, exec, Options{RepoAuth: auth})
		_, run := runJob(t, svc, exec, JobRequest{Kind: KindBuild, Pool: "linux", Repo: "https://github.com/someone/else", Script: "true"})
		if run.CloneToken != "" {
			t.Fatalf("an out-of-scope repo got the credential")
		}
	})
	t.Run("shell job never clones", func(t *testing.T) {
		exec := newFakeExec()
		auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_secret"}
		svc := newTestServiceWith(t, exec, Options{RepoAuth: auth})
		_, run := runJob(t, svc, exec, JobRequest{Kind: KindShell, Pool: "linux", Repo: "https://github.com/gm2211/grove", Script: "true"})
		if run.CloneToken != "" || auth.callCount() != 0 {
			t.Fatalf("a shell job got token %q (store consulted %d times)", run.CloneToken, auth.callCount())
		}
	})
}

// A caller that brought its own credential keeps it: an explicit per-job token is more specific
// than the fleet-wide armed one.
func TestRepoAuth_ExplicitJobTokenWins(t *testing.T) {
	for _, name := range []string{"GH_TOKEN", "GIT_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			exec := newFakeExec()
			auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_armed"}
			svc := newTestServiceWith(t, exec, Options{RepoAuth: auth})
			job, run := runJob(t, svc, exec, JobRequest{
				Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove", Script: "true",
				Env: map[string]string{name: "ghp_caller"},
			})
			if run.CloneToken != "" || run.Request.Env[name] != "ghp_caller" {
				t.Fatalf("token %q env %v, want only the caller's own", run.CloneToken, run.Request.Env)
			}
			if job.RepoCredentialUsed {
				t.Fatal("RepoCredentialUsed set though the caller supplied its own token")
			}
		})
	}
}

func TestSubmit_RejectsUnsafeRequestFields(t *testing.T) {
	cases := map[string]JobRequest{
		"ssh repo":         {Kind: KindBuild, Pool: "linux", Repo: "git@github.com:gm2211/grove.git", Script: "true"},
		"option repo":      {Kind: KindBuild, Pool: "linux", Repo: "--upload-pack=touch /tmp/x", Script: "true"},
		"plain http repo":  {Kind: KindAgent, Pool: "linux", Repo: "http://github.com/gm2211/grove", Script: "true"},
		"option ref":       {Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove", Ref: "--orphan=x", Script: "true"},
		"multi-line repo":  {Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove\nx", Script: "true"},
		"shell in env key": {Kind: KindShell, Pool: "linux", Script: "true", Env: map[string]string{"A=$(id);B": "1"}},
		"digit env key":    {Kind: KindShell, Pool: "linux", Script: "true", Env: map[string]string{"1A": "1"}},
		"named secrets":    {Kind: KindShell, Pool: "linux", Script: "true", Secrets: []string{"GH_TOKEN"}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			exec := newFakeExec()
			svc := newTestService(t, exec)
			if _, _, err := svc.Submit(context.Background(), req); err == nil {
				t.Fatal("Submit accepted the request")
			}
			exec.none(t)
		})
	}
}

// Env values reach the job and nowhere else for good: the history keeps the keys only, and the
// queue spool (0600) holds the values only until the job starts.
func TestSubmit_EnvValuesNeverPersistedInHistory(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "jobs.json")
	exec := newFakeExec()
	svc := newTestServiceWith(t, exec, Options{StorePath: storePath})
	req := JobRequest{Kind: KindShell, Pool: "linux", Script: "true", Env: map[string]string{"API_KEY": "sk-live-value"}}
	job := submit(t, svc, req)
	if req.Env["API_KEY"] != "sk-live-value" {
		t.Fatal("Submit mutated the caller's Env map")
	}
	if got := job.Request.Env["API_KEY"]; got != RedactedEnvValue {
		t.Fatalf("returned job env value = %q, want it redacted", got)
	}
	data, err := os.ReadFile(storePath)
	mustf(t, err, "read job store")
	if strings.Contains(string(data), "sk-live-value") || !strings.Contains(string(data), "API_KEY") {
		t.Fatalf("job store should keep the key and drop the value: %s", data)
	}
	info, err := os.Stat(svc.spool.path(job.ID))
	mustf(t, err, "stat spool")
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spool mode = %v", info.Mode().Perm())
	}

	fr := exec.next(t)
	if fr.run.Request.Env["API_KEY"] != "sk-live-value" {
		t.Fatalf("the job itself lost its env: %v", fr.run.Request.Env)
	}
	fr.place <- "linux-mac1-0"
	waitStatus(t, svc, job.ID, StatusRunning)
	if _, err := os.Stat(svc.spool.path(job.ID)); !os.IsNotExist(err) {
		t.Fatal("the spool entry outlived the job's start")
	}
}
