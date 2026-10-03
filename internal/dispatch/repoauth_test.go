package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRepoAuth is an armed private-repo credential for exactly one repo URL.
type fakeRepoAuth struct {
	repo  string
	token string

	calls []string
}

func (f *fakeRepoAuth) TokenFor(repoURL string) (string, bool) {
	f.calls = append(f.calls, repoURL)
	if repoURL != f.repo {
		return "", false
	}
	return f.token, true
}

func newServiceWithRepoAuth(t *testing.T, nc *fakeNomad, auth RepoAuth) Service {
	t.Helper()
	svc, err := New(nc, Options{
		StorePath: filepath.Join(t.TempDir(), "jobs.json"),
		StatusTTL: time.Millisecond,
		RepoAuth:  auth,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func dispatchedEnv(t *testing.T, nc *fakeNomad) map[string]string {
	t.Helper()
	if len(nc.dispatchCalls) != 1 {
		t.Fatalf("got %d dispatch calls, want 1", len(nc.dispatchCalls))
	}
	raw := nc.dispatchCalls[0].meta["env_json"]
	if raw == "" {
		return map[string]string{}
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal env_json %q: %v", raw, err)
	}
	return env
}

// dispatchedToken returns the clone token line of the single dispatch's payload.
func dispatchedToken(t *testing.T, nc *fakeNomad) string {
	t.Helper()
	if len(nc.dispatchCalls) != 1 {
		t.Fatalf("got %d dispatch calls, want 1", len(nc.dispatchCalls))
	}
	lines := strings.SplitN(string(nc.dispatchCalls[0].payload), "\n", 3)
	if len(lines) < 3 || lines[0] != "grove-payload/1" {
		return ""
	}
	return lines[1]
}

func TestSubmit_InjectsArmedRepoCredential(t *testing.T) {
	nc := &fakeNomad{}
	auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_secret"}
	svc := newServiceWithRepoAuth(t, nc, auth)

	job, _, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove", Ref: "main",
		Script: "make test", Env: map[string]string{"CI": "1"}, Requester: "cli",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if got, want := dispatchedToken(t, nc), "ghp_secret"; got != want {
		t.Fatalf("payload clone token = %q, want the armed token", got)
	}
	if want := "grove-payload/1\nghp_secret\nmake test"; string(nc.dispatchCalls[0].payload) != want {
		t.Fatalf("payload = %q, want %q", nc.dispatchCalls[0].payload, want)
	}
	env := dispatchedEnv(t, nc)
	if _, ok := env["GH_TOKEN"]; ok {
		t.Fatalf("the armed token reached env_json, which the caller's script sees: %+v", env)
	}
	for key, value := range nc.dispatchCalls[0].meta {
		if strings.Contains(value, "ghp_secret") {
			t.Fatalf("the armed token reached dispatch meta %q, which every task sees", key)
		}
	}
	if env["CI"] != "1" {
		t.Fatalf("injection dropped the caller's env: %+v", env)
	}
	if !job.RepoCredentialUsed {
		t.Fatal("Job.RepoCredentialUsed is false though a credential was injected")
	}
}

// The token belongs in the Nomad dispatch and nowhere else: it must not reach the caller's
// JobRequest, the Job returned by Submit/Get, or the on-disk job history.
func TestSubmit_ArmedCredentialNeverLandsOnTheJobRecord(t *testing.T) {
	nc := &fakeNomad{}
	auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_secret"}
	storePath := filepath.Join(t.TempDir(), "jobs.json")
	svc, err := New(nc, Options{StorePath: storePath, StatusTTL: time.Millisecond, RepoAuth: auth})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := JobRequest{
		Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove",
		Script: "make test", Env: map[string]string{"CI": "1"}, Requester: "cli",
	}
	job, _, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, ok := req.Env["GH_TOKEN"]; ok {
		t.Fatal("Submit mutated the caller's Env map")
	}
	if _, ok := job.Request.Env["GH_TOKEN"]; ok {
		t.Fatalf("the returned Job carries the credential: %+v", job.Request.Env)
	}

	data, err := readStoreFile(storePath)
	if err != nil {
		t.Fatalf("read job store: %v", err)
	}
	if strings.Contains(data, "ghp_secret") {
		t.Fatalf("the credential was persisted to the job store: %s", data)
	}
}

func TestSubmit_NoCredentialWhenNotArmedOrOutOfScope(t *testing.T) {
	t.Run("no RepoAuth configured", func(t *testing.T) {
		nc := &fakeNomad{}
		svc := newTestService(t, nc)
		job, _, err := svc.Submit(context.Background(), JobRequest{
			Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove", Script: "true",
		})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if token := dispatchedToken(t, nc); token != "" {
			t.Fatalf("a control plane with no armed credential injected one: %q", token)
		}
		if job.RepoCredentialUsed {
			t.Fatal("RepoCredentialUsed set with no RepoAuth configured")
		}
	})

	t.Run("repo out of scope", func(t *testing.T) {
		nc := &fakeNomad{}
		auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_secret"}
		svc := newServiceWithRepoAuth(t, nc, auth)
		if _, _, err := svc.Submit(context.Background(), JobRequest{
			Kind: KindBuild, Pool: "linux", Repo: "https://github.com/someone/else", Script: "true",
		}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if token := dispatchedToken(t, nc); token != "" {
			t.Fatalf("an out-of-scope repo got the credential: %q", token)
		}
	})

	t.Run("shell job never clones", func(t *testing.T) {
		nc := &fakeNomad{}
		auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_secret"}
		svc := newServiceWithRepoAuth(t, nc, auth)
		if _, _, err := svc.Submit(context.Background(), JobRequest{
			Kind: KindShell, Pool: "linux", Repo: "https://github.com/gm2211/grove", Script: "true",
		}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if payload := string(nc.dispatchCalls[0].payload); payload != "true" {
			t.Fatalf("a shell job's payload = %q, want the bare script", payload)
		}
		if len(auth.calls) != 0 {
			t.Fatalf("the store was consulted for a shell job: %v", auth.calls)
		}
	})
}

// A caller that brought its own credential keeps it: an explicit per-job token is more specific
// than the fleet-wide armed one.
func TestSubmit_ExplicitJobTokenWins(t *testing.T) {
	for _, name := range []string{"GH_TOKEN", "GIT_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			nc := &fakeNomad{}
			auth := &fakeRepoAuth{repo: "https://github.com/gm2211/grove", token: "ghp_armed"}
			svc := newServiceWithRepoAuth(t, nc, auth)
			job, _, err := svc.Submit(context.Background(), JobRequest{
				Kind: KindBuild, Pool: "linux", Repo: "https://github.com/gm2211/grove", Script: "true",
				Env: map[string]string{name: "ghp_caller"},
			})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			env := dispatchedEnv(t, nc)
			if env[name] != "ghp_caller" {
				t.Fatalf("%s = %q, want the caller's own token", name, env[name])
			}
			if env["GH_TOKEN"] == "ghp_armed" || dispatchedToken(t, nc) != "" {
				t.Fatal("the armed credential overrode the caller's own")
			}
			if job.RepoCredentialUsed {
				t.Fatal("RepoCredentialUsed set though the caller supplied its own token")
			}
		})
	}
}

func readStoreFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
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
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			nc := &fakeNomad{}
			svc := newTestService(t, nc)
			if _, _, err := svc.Submit(context.Background(), req); err == nil {
				t.Fatal("Submit accepted the request")
			}
			if len(nc.dispatchCalls) != 0 {
				t.Fatal("a rejected request was dispatched")
			}
		})
	}
}
