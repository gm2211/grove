package dispatch

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/broker"
	"github.com/gm2211/grove/internal/nomad"
)

const (
	realGH  = "ghs_REALGITHUBTOKEN"
	realAnt = "sk-ant-oat01-REALCLAUDE"
)

func credRequest() JobRequest {
	return JobRequest{
		Kind: KindAgent, Pool: "linux", Repo: "https://github.com/o/r", Script: "claude -p hi",
		Env: map[string]string{"MODE": "fast"},
		Credentials: []Credential{
			{Kind: broker.KindGitHub, Env: "GH_TOKEN", Token: realGH},
			{Kind: broker.KindAnthropic, Env: "CLAUDE_CODE_OAUTH_TOKEN", Token: realAnt},
		},
	}
}

func newCredService(t *testing.T, nc *fakeNomad, storePath string, pools ...PoolConfig) *service {
	t.Helper()
	if storePath == "" {
		storePath = filepath.Join(t.TempDir(), "jobs.json")
	}
	svc, err := New(nc, Options{StorePath: storePath, StatusTTL: time.Millisecond, Pools: pools})
	if err != nil {
		t.Fatal(err)
	}
	return svc.(*service)
}

// place makes Nomad report the job running on a node of worker.
func place(nc *fakeNomad, nomadJobID, worker, status string) {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	if nc.allocationsByJob == nil {
		nc.allocationsByJob = map[string][]nomad.Allocation{}
	}
	if nc.nodesByID == nil {
		nc.nodesByID = map[string]nomad.Node{}
	}
	nc.nodesByID["node-"+worker] = nomad.Node{ID: "node-" + worker, Meta: map[string]string{"host": worker, "vm": "vm-on-" + worker}}
	var exit *int
	if status == "complete" {
		zero := 0
		exit = &zero
	}
	nc.allocationsByJob[nomadJobID] = []nomad.Allocation{{ID: "alloc-1", NodeID: "node-" + worker, ClientStatus: status, CreatedAt: time.Now(), ExitCode: exit}}
}

func TestSubmit_CredentialsBecomePlaceholders(t *testing.T) {
	nc := &fakeNomad{}
	storePath := filepath.Join(t.TempDir(), "jobs.json")
	svc := newCredService(t, nc, storePath)
	req := credRequest()
	job, _, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	env := dispatchedEnv(t, nc)
	for _, name := range []string{"GH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if !strings.HasPrefix(env[name], broker.PlaceholderPrefix) || len(env[name]) != len(broker.PlaceholderPrefix)+52 {
			t.Fatalf("%s = %q, want a placeholder", name, env[name])
		}
	}
	if env["GH_TOKEN"] == env["CLAUDE_CODE_OAUTH_TOKEN"] || env["MODE"] != "fast" {
		t.Fatalf("env = %v", env)
	}
	call := nc.dispatchCalls[0]
	for _, real := range []string{realGH, realAnt} {
		for k, v := range call.meta {
			if strings.Contains(v, real) {
				t.Fatalf("real token in dispatch meta %s", k)
			}
		}
		if strings.Contains(string(call.payload), real) {
			t.Fatal("real token in the payload")
		}
		data, _ := readStoreFile(storePath)
		if strings.Contains(data, real) {
			t.Fatal("real token in the job history")
		}
	}
	if _, leaked := req.Env["GH_TOKEN"]; leaked || req.Credentials[0].Token != realGH {
		t.Fatal("Submit changed the caller's request")
	}
	if len(job.Request.Credentials) != 2 || job.Request.Credentials[0].Token != "" || job.Request.Credentials[0].Env != "GH_TOKEN" {
		t.Fatalf("returned job credentials = %+v, want kind and env only", job.Request.Credentials)
	}
	if got, _ := svc.Get(context.Background(), job.ID); got.Request.Credentials[1].Token != "" {
		t.Fatal("stored job returns a token")
	}
}

func TestBrokerEntries_FollowPlacementAndJobEnd(t *testing.T) {
	ctx := context.Background()
	nc := &fakeNomad{}
	svc := newCredService(t, nc, "")
	job, _, err := svc.Submit(ctx, credRequest())
	if err != nil {
		t.Fatal(err)
	}
	env := dispatchedEnv(t, nc)
	rec, _ := svc.store.get(job.ID)

	// Not placed yet: no worker gets it.
	if got, _ := svc.BrokerEntries(ctx, "mac-1"); len(got) != 0 {
		t.Fatalf("unplaced job served: %+v", got)
	}

	place(nc, rec.NomadJobID, "mac-1", "running")
	got, err := svc.BrokerEntries(ctx, "mac-1") // no wait: an unplaced job's status is re-read at once
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("mac-1 got %d entries, want 2", len(got))
	}
	tokens := map[string]string{}
	for _, e := range got {
		tokens[e.Placeholder] = e.Token
		if e.Job != job.ID || e.VM != "vm-on-mac-1" || time.Until(e.Expires) < broker.Lease-time.Minute {
			t.Fatalf("entry %+v", e)
		}
	}
	if tokens[env["GH_TOKEN"]] != realGH || tokens[env["CLAUDE_CODE_OAUTH_TOKEN"]] != realAnt {
		t.Fatalf("placeholders don't map to their tokens: %v", tokens)
	}
	if other, _ := svc.BrokerEntries(ctx, "mac-2"); len(other) != 0 {
		t.Fatalf("another worker got this job's tokens: %+v", other)
	}

	// The job ends: no worker gets it, and the control plane forgets the tokens.
	place(nc, rec.NomadJobID, "mac-1", "complete")
	time.Sleep(2 * time.Millisecond) // past StatusTTL
	if got, _ := svc.BrokerEntries(ctx, "mac-1"); len(got) != 0 {
		t.Fatalf("finished job still served: %+v", got)
	}
	if svc.creds.Len() != 0 {
		t.Fatal("finished job's tokens still in memory")
	}
	if err := svc.UpdateCredentials(ctx, job.ID, []Credential{{Env: "GH_TOKEN", Token: "ghs_new"}}); !errors.Is(err, ErrJobEnded) {
		t.Fatalf("update after end: %v, want ErrJobEnded", err)
	}
}

func TestReconcilerForgetsEndedJobsTokens(t *testing.T) {
	ctx := context.Background()
	nc := &fakeNomad{}
	svc := newCredService(t, nc, "")
	job, _, err := svc.Submit(ctx, credRequest())
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := svc.store.get(job.ID)
	place(nc, rec.NomadJobID, "mac-1", "complete")
	time.Sleep(2 * time.Millisecond)
	if _, err := svc.Get(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if svc.creds.Len() != 0 {
		t.Fatal("a job seen ending by reconcile kept its tokens")
	}

	job2, _, err := svc.Submit(ctx, credRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Cancel(ctx, job2.ID); err != nil {
		t.Fatal(err)
	}
	if svc.creds.Len() != 0 {
		t.Fatal("a canceled job kept its tokens")
	}
}

func TestUpdateCredentials(t *testing.T) {
	ctx := context.Background()
	nc := &fakeNomad{}
	storePath := filepath.Join(t.TempDir(), "jobs.json")
	svc := newCredService(t, nc, storePath)
	job, _, err := svc.Submit(ctx, credRequest())
	if err != nil {
		t.Fatal(err)
	}
	env := dispatchedEnv(t, nc)
	rec, _ := svc.store.get(job.ID)
	place(nc, rec.NomadJobID, "mac-1", "running")

	for name, creds := range map[string][]Credential{
		"unknown env":  {{Env: "OPENAI_API_KEY", Token: "sk-x"}},
		"wrong kind":   {{Env: "GH_TOKEN", Kind: broker.KindOpenAI, Token: "x"}},
		"bad token":    {{Env: "GH_TOKEN", Token: "a\nb"}},
		"empty update": nil,
	} {
		if err := svc.UpdateCredentials(ctx, job.ID, creds); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if err := svc.UpdateCredentials(ctx, "nope", []Credential{{Env: "GH_TOKEN", Token: "x"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job: %v", err)
	}

	if err := svc.UpdateCredentials(ctx, job.ID, []Credential{{Env: "GH_TOKEN", Token: "ghs_REFRESHED"}}); err != nil {
		t.Fatal(err)
	}
	tokens := entriesByPlaceholder(t, svc, "mac-1")
	if tokens[env["GH_TOKEN"]] != "ghs_REFRESHED" || tokens[env["CLAUDE_CODE_OAUTH_TOKEN"]] != realAnt {
		t.Fatalf("after refresh: %v", tokens)
	}

	// A control-plane restart forgets every token; the submitter sending them again restores them
	// behind the placeholders the running job already has.
	restarted := newCredService(t, nc, storePath)
	if got := entriesByPlaceholder(t, restarted, "mac-1"); len(got) != 0 {
		t.Fatalf("tokens survived a restart: %v", got)
	}
	if err := restarted.UpdateCredentials(ctx, job.ID, []Credential{{Env: "GH_TOKEN", Token: "ghs_AGAIN"}}); err != nil {
		t.Fatal(err)
	}
	if got := entriesByPlaceholder(t, restarted, "mac-1"); len(got) != 1 || got[env["GH_TOKEN"]] != "ghs_AGAIN" {
		t.Fatalf("after re-sending one token: %v", got)
	}
}

func entriesByPlaceholder(t *testing.T, svc *service, worker string) map[string]string {
	t.Helper()
	time.Sleep(2 * time.Millisecond) // past StatusTTL
	got, err := svc.BrokerEntries(context.Background(), worker)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range got {
		out[e.Placeholder] = e.Token
	}
	return out
}

func TestSubmit_CredentialsRefusedOnSealedPools(t *testing.T) {
	nc := &fakeNomad{}
	svc := newCredService(t, nc, "", PoolConfig{Name: "linux", Fallback: "sealed"}, PoolConfig{Name: "sealed", Isolated: true})
	req := credRequest()
	req.Pool = "sealed"
	if _, _, err := svc.Submit(context.Background(), req); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("credentials on a sealed pool: %v", err)
	}
	if len(nc.dispatchCalls) != 0 || svc.creds.Len() != 0 {
		t.Fatal("refused job was dispatched or registered")
	}

	// A job with credentials never falls back onto a sealed pool.
	rec := &record{Job: Job{Request: credRequest(), SubmittedAt: time.Now().Add(-time.Hour)}}
	if target, _ := svc.fallbackTarget(rec, nil, time.Now()); target != "" {
		t.Fatalf("fell back to %q", target)
	}
	rec.Request.Credentials = nil
	if target, _ := svc.fallbackTarget(rec, nil, time.Now()); target != "sealed" {
		t.Fatalf("a job without credentials should still fall back, got %q", target)
	}
}

func TestSubmit_RejectsBadCredentials(t *testing.T) {
	many := make([]Credential, MaxCredentials+1)
	for i := range many {
		many[i] = Credential{Kind: broker.KindGitHub, Env: "T" + strings.Repeat("X", i), Token: "t"}
	}
	gh := func(env, token string) Credential { return Credential{Kind: broker.KindGitHub, Env: env, Token: token} }
	cases := map[string]JobRequest{
		"unknown kind":     {Credentials: []Credential{{Kind: "gitlab", Env: "T", Token: "t"}}},
		"bad env":          {Credentials: []Credential{gh("A=$(id)", "t")}},
		"twice":            {Credentials: []Credential{gh("T", "a"), gh("T", "b")}},
		"clashes with env": {Credentials: []Credential{gh("T", "a")}, Env: map[string]string{"T": "x"}},
		"clashes w secret": {Credentials: []Credential{gh("T", "a")}, Secrets: []string{"T"}},
		"no token":         {Credentials: []Credential{gh("T", "")}},
		"header split":     {Credentials: []Credential{gh("T", "a\r\nX-Evil: 1")}},
		"too many":         {Credentials: many},
	}
	for name, req := range cases {
		req.Kind, req.Pool, req.Script = KindShell, "linux", "true"
		nc := &fakeNomad{}
		svc := newCredService(t, nc, "")
		if _, _, err := svc.Submit(context.Background(), req); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if len(nc.dispatchCalls) != 0 || svc.creds.Len() != 0 {
			t.Fatalf("%s: dispatched or registered anyway", name)
		}
	}
}

func TestSubmit_DispatchFailureForgetsTokens(t *testing.T) {
	nc := &fakeNomad{dispatchErr: errors.New("nomad down")}
	svc := newCredService(t, nc, "")
	if _, _, err := svc.Submit(context.Background(), credRequest()); err == nil {
		t.Fatal("submit succeeded")
	}
	if svc.creds.Len() != 0 {
		t.Fatal("tokens kept for a job that was never dispatched")
	}
}
