package dispatch

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSecrets map[string]string

func (f fakeSecrets) Resolve(names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range names {
		v, ok := f[n]
		if !ok {
			return nil, fmt.Errorf("no secret named %q is set", n)
		}
		out[n] = v
	}
	return out, nil
}

func newServiceWithSecrets(t *testing.T, nc *fakeNomad, res SecretResolver) (Service, string) {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "jobs.json")
	svc, err := New(nc, Options{StorePath: storePath, StatusTTL: time.Millisecond, Secrets: res})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, storePath
}

// A named secret reaches the job's env under its own name, and nowhere else: not the stored
// history, not the returned job.
func TestSubmit_ResolvesNamedSecretsIntoEnvOnly(t *testing.T) {
	nc := &fakeNomad{}
	svc, storePath := newServiceWithSecrets(t, nc, fakeSecrets{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-secret"})
	req := JobRequest{
		Kind: KindAgent, Pool: "linux", Repo: "https://github.com/o/r", Script: "claude -p hi",
		Env: map[string]string{"MODE": "fast"}, Secrets: []string{"CLAUDE_CODE_OAUTH_TOKEN"},
	}
	job, _, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	env := dispatchedEnv(t, nc)
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-secret" || env["MODE"] != "fast" {
		t.Fatalf("dispatched env = %+v", env)
	}
	if _, leaked := req.Env["CLAUDE_CODE_OAUTH_TOKEN"]; leaked {
		t.Fatal("Submit wrote the secret into the caller's Env map")
	}
	if _, leaked := job.Request.Env["CLAUDE_CODE_OAUTH_TOKEN"]; leaked {
		t.Fatal("the returned job carries the secret as env")
	}
	if len(job.Request.Secrets) != 1 || job.Request.Secrets[0] != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("job should keep the secret's name: %+v", job.Request.Secrets)
	}
	data, err := readStoreFile(storePath)
	if err != nil {
		t.Fatalf("read job store: %v", err)
	}
	if strings.Contains(data, "sk-ant-oat-secret") {
		t.Fatalf("job store holds the secret value: %s", data)
	}
	if strings.Contains(string(nc.dispatchCalls[0].payload), "sk-ant-oat-secret") {
		t.Fatal("the secret went into the payload, which the source task reads")
	}
}

func TestSubmit_RejectsBadSecretRequests(t *testing.T) {
	cases := map[string]JobRequest{
		"unknown name":    {Kind: KindShell, Pool: "linux", Script: "true", Secrets: []string{"MISSING"}},
		"invalid name":    {Kind: KindShell, Pool: "linux", Script: "true", Secrets: []string{"BAD-NAME"}},
		"named twice":     {Kind: KindShell, Pool: "linux", Script: "true", Secrets: []string{"TOKEN", "TOKEN"}},
		"clashes w/ env":  {Kind: KindShell, Pool: "linux", Script: "true", Secrets: []string{"TOKEN"}, Env: map[string]string{"TOKEN": "inline"}},
		"shell syntax":    {Kind: KindShell, Pool: "linux", Script: "true", Secrets: []string{"A=$(id)"}},
		"empty name only": {Kind: KindShell, Pool: "linux", Script: "true", Secrets: []string{""}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			nc := &fakeNomad{}
			svc, _ := newServiceWithSecrets(t, nc, fakeSecrets{"TOKEN": "v"})
			if _, _, err := svc.Submit(context.Background(), req); err == nil {
				t.Fatal("Submit accepted the request")
			}
			if len(nc.dispatchCalls) != 0 {
				t.Fatal("a rejected request was dispatched")
			}
		})
	}
}
