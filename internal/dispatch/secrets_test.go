package dispatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
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

// A value no quoting shortcut survives.
const nastySecret = "sp ace 'single' \"double\" $(echo marker-subst) `echo tick` \\ end\nsecond line"

// noSecretInMeta fails if any dispatch meta key or value of call carries one of values, in the
// clear or inside a JSON string.
func noSecretInMeta(t *testing.T, call dispatchCall, values ...string) {
	t.Helper()
	for k, v := range call.meta {
		for _, secret := range values {
			quoted, _ := json.Marshal(secret)
			inner := string(quoted[1 : len(quoted)-1])
			for _, needle := range []string{secret, inner, base64.StdEncoding.EncodeToString([]byte(secret))} {
				if strings.Contains(v, needle) || strings.Contains(k, needle) {
					t.Errorf("dispatch meta %q carries a secret value: %q", k, v)
				}
			}
		}
	}
}

// A named secret's value travels in the dispatch payload and nowhere else: no dispatch meta
// (every task of the job sees those), not the stored history, not the returned job.
func TestSubmit_NamedSecretsTravelOnlyInThePayload(t *testing.T) {
	for _, kind := range []Kind{KindBuild, KindAgent, KindShell} {
		t.Run(string(kind), func(t *testing.T) {
			nc := &fakeNomad{}
			svc, storePath := newServiceWithSecrets(t, nc, fakeSecrets{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-secret", "NASTY": nastySecret})
			req := JobRequest{
				Kind: kind, Pool: "linux", Script: "claude -p hi\necho done\n",
				Env: map[string]string{"MODE": "fast"}, Secrets: []string{"CLAUDE_CODE_OAUTH_TOKEN", "NASTY"},
			}
			if kind != KindShell {
				req.Repo = "https://github.com/o/r"
			}
			job, _, err := svc.Submit(context.Background(), req)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			env := dispatchedEnv(t, nc)
			if len(env) != 1 || env["MODE"] != "fast" {
				t.Fatalf("env_json = %+v, want the caller's env only", env)
			}
			call := nc.dispatchCalls[0]
			noSecretInMeta(t, call, "sk-ant-oat-secret", nastySecret)

			if !strings.HasPrefix(string(call.payload), "grove-payload/2\n\n") {
				t.Fatalf("payload = %q, want version 2 with an empty token line", call.payload)
			}
			if lines := strings.SplitN(string(call.payload), "\n", 4); len(lines) != 4 || lines[3] != req.Script ||
				strings.ContainsAny(lines[2], " '\"$`\\") {
				t.Fatalf("payload = %q, want one opaque secrets line, then the script", call.payload)
			}
			p, err := DecodePayload(string(kind), call.payload)
			if err != nil {
				t.Fatalf("DecodePayload: %v", err)
			}
			if p.Script != req.Script || p.CloneToken != "" || len(p.Secrets) != 2 ||
				p.Secrets["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-secret" || p.Secrets["NASTY"] != nastySecret {
				t.Fatalf("decoded payload = %+v", p)
			}

			if _, leaked := req.Env["CLAUDE_CODE_OAUTH_TOKEN"]; leaked {
				t.Fatal("Submit wrote the secret into the caller's Env map")
			}
			if _, leaked := job.Request.Env["CLAUDE_CODE_OAUTH_TOKEN"]; leaked {
				t.Fatal("the returned job carries the secret as env")
			}
			if len(job.Request.Secrets) != 2 || job.Request.Secrets[0] != "CLAUDE_CODE_OAUTH_TOKEN" {
				t.Fatalf("job should keep the secrets' names: %+v", job.Request.Secrets)
			}
			returned, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			data, err := readStoreFile(storePath)
			if err != nil {
				t.Fatalf("read job store: %v", err)
			}
			for where, text := range map[string]string{"job store": data, "returned job": string(returned)} {
				if strings.Contains(text, "sk-ant-oat-secret") || strings.Contains(text, "marker-subst") {
					t.Fatalf("%s holds a secret value: %s", where, text)
				}
			}
		})
	}
}

// A job with no named secret is dispatched exactly as before version 2 existed.
func TestSubmit_PayloadWithoutSecretsIsUnchanged(t *testing.T) {
	want := map[Kind]string{KindBuild: "grove-payload/1\n\nmake\n", KindAgent: "grove-payload/1\n\nmake\n", KindShell: "make\n"}
	for kind, payload := range want {
		nc := &fakeNomad{}
		svc, _ := newServiceWithSecrets(t, nc, fakeSecrets{})
		req := JobRequest{Kind: kind, Pool: "linux", Script: "make\n"}
		if kind != KindShell {
			req.Repo = "https://github.com/o/r"
		}
		if _, _, err := svc.Submit(context.Background(), req); err != nil {
			t.Fatalf("%s: Submit: %v", kind, err)
		}
		if got := string(nc.dispatchCalls[0].payload); got != payload {
			t.Errorf("%s payload = %q, want %q", kind, got, payload)
		}
	}
}

// Nomad refuses a payload over 16 KiB; Submit says so itself, naming neither script nor secret.
func TestSubmit_RejectsPayloadOverNomadsLimit(t *testing.T) {
	big := strings.Repeat("s", 9000)
	nc := &fakeNomad{}
	svc, _ := newServiceWithSecrets(t, nc, fakeSecrets{"BIG": big})
	script := strings.Repeat("#", 6000) + "\n"

	// Each fits alone; base64 of the secret pushes the two past the limit together.
	if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: script}); err != nil {
		t.Fatalf("script alone: %v", err)
	}
	if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true\n", Secrets: []string{"BIG"}}); err != nil {
		t.Fatalf("secret alone: %v", err)
	}
	_, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: script, Secrets: []string{"BIG"}})
	if err == nil || !strings.Contains(err.Error(), "16384") {
		t.Fatalf("Submit error = %v, want the payload limit named", err)
	}
	if strings.Contains(err.Error(), "ssss") || strings.Contains(err.Error(), "####") {
		t.Fatalf("the error quotes the payload: %v", err)
	}
	if len(nc.dispatchCalls) != 2 {
		t.Fatalf("dispatch calls = %d, want the oversized job never dispatched", len(nc.dispatchCalls))
	}
	if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: strings.Repeat("#", MaxPayloadBytes+1)}); err == nil {
		t.Fatal("Submit accepted a script over the payload limit")
	}
}

// A shell job's payload is its script unless it starts with a payload header, so a script may not.
func TestSubmit_RejectsShellScriptThatLooksLikeAPayloadFrame(t *testing.T) {
	nc := &fakeNomad{}
	svc, _ := newServiceWithSecrets(t, nc, fakeSecrets{})
	if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "grove-payload/2\n\nabc\necho hi\n"}); err == nil {
		t.Fatal("Submit accepted a shell script that starts with a payload header")
	}
	if len(nc.dispatchCalls) != 0 {
		t.Fatal("a rejected request was dispatched")
	}
}

func TestDecodePayload(t *testing.T) {
	for _, kind := range []Kind{KindBuild, KindAgent, KindShell} {
		token := "tok"
		if kind == KindShell {
			token = ""
		}
		in := Payload{CloneToken: token, Secrets: map[string]string{"A": nastySecret, "B": ""}, Script: "line one\nline two"}
		raw, err := encodePayload(kind, in)
		if err != nil {
			t.Fatalf("%s: encode: %v", kind, err)
		}
		out, err := DecodePayload(string(kind), raw)
		if err != nil || out.CloneToken != in.CloneToken || out.Script != in.Script || len(out.Secrets) != 2 || out.Secrets["A"] != nastySecret {
			t.Errorf("%s: round trip = %+v, %v", kind, out, err)
		}
	}
	if _, err := encodePayload(KindShell, Payload{CloneToken: "tok", Script: "x"}); err == nil {
		t.Error("a shell payload took a clone token")
	}
	// What an older grove dispatched still reads.
	if p, err := DecodePayload("build", []byte("grove-payload/1\ntok\nmake\n")); err != nil || p.CloneToken != "tok" || p.Script != "make\n" || p.Secrets != nil {
		t.Errorf("version 1 = %+v, %v", p, err)
	}
	if p, err := DecodePayload("shell", []byte("grove-payload/1\ntok\nmake\n")); err != nil || p.Script != "grove-payload/1\ntok\nmake\n" {
		t.Errorf("a version 1 header on a shell job is script: %+v, %v", p, err)
	}
	// An unreadable secrets line is an error that doesn't quote it.
	for _, bad := range []string{"@@@@", base64.StdEncoding.EncodeToString([]byte(`["a"]`))} {
		if _, err := DecodePayload("shell", []byte("grove-payload/2\n\n"+bad+"\ntrue\n")); err == nil || strings.Contains(err.Error(), bad) {
			t.Errorf("DecodePayload(%q) = %v", bad, err)
		}
	}
}

// secretEvictSetup submits a build job naming a secret and starts it on a worker.
func secretEvictSetup(t *testing.T) (*fakeNomad, *service, string) {
	t.Helper()
	nc := &fakeNomad{nodesByID: map[string]nomad.Node{
		"n1": {ID: "n1", Meta: map[string]string{"pool": "macos", "host": "mac.local", "vm": "macos-g-0"}},
	}}
	svc := newTestServiceWithOpts(t, nc, Options{Pools: []PoolConfig{{Name: "macos"}}, Secrets: fakeSecrets{"API_KEY": nastySecret}}).(*service)
	job, _, err := svc.Submit(context.Background(), JobRequest{
		Kind: KindBuild, Pool: "macos", Repo: "https://github.com/x/y", Script: "make\n",
		Env: map[string]string{"A": "b"}, Secrets: []string{"API_KEY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	nc.allocationsByJob = map[string][]nomad.Allocation{"grove-build-macos/dispatch-1": {{ID: "a1", NodeID: "n1", NodeName: "vm", ClientStatus: "running", CreatedAt: time.Now()}}}
	if got, err := svc.Get(context.Background(), job.ID); err != nil || got.Status != StatusRunning {
		t.Fatalf("not running: %+v %v", got, err)
	}
	nc.dispatchResult = &nomad.DispatchResult{DispatchedJobID: "grove-build-macos/dispatch-2", EvalID: "e2"}
	return nc, svc, job.ID
}

// A redispatch (eviction, fallback) reuses the payload it reads back, secrets included, and its
// meta stays free of them.
func TestRedispatch_KeepsSecretsInThePayload(t *testing.T) {
	nc, svc, id := secretEvictSetup(t)
	requeued, _, err := svc.Evict(context.Background(), "mac.local", "worker snoozed")
	if err != nil || len(requeued) != 1 || requeued[0] != id || len(nc.dispatchCalls) != 2 {
		t.Fatalf("requeued=%v err=%v calls=%d", requeued, err, len(nc.dispatchCalls))
	}
	again := nc.dispatchCalls[1]
	if string(again.payload) != string(nc.dispatchCalls[0].payload) {
		t.Error("the redispatch changed the payload")
	}
	noSecretInMeta(t, again, nastySecret)
	if p, err := DecodePayload("build", again.payload); err != nil || p.Secrets["API_KEY"] != nastySecret {
		t.Errorf("redispatched payload = %+v, %v", p, err)
	}
}

// A job an older grove dispatched has its secrets in env_json. Redispatching it moves them into
// the payload: this control plane never dispatches a secret in meta.
func TestRedispatch_MovesAnOlderDispatchsSecretsOutOfMeta(t *testing.T) {
	nc, svc, _ := secretEvictSetup(t)
	legacyEnv, _ := json.Marshal(map[string]string{"A": "b", "API_KEY": nastySecret})
	legacy := nc.dispatched["grove-build-macos/dispatch-1"]
	legacy.meta["env_json"] = string(legacyEnv)
	legacy.payload = []byte("grove-payload/1\nclone-tok\nmake\n")
	nc.dispatched["grove-build-macos/dispatch-1"] = legacy

	if requeued, _, err := svc.Evict(context.Background(), "mac.local", "worker snoozed"); err != nil || len(requeued) != 1 {
		t.Fatalf("requeued=%v err=%v", requeued, err)
	}
	again := nc.dispatchCalls[len(nc.dispatchCalls)-1]
	noSecretInMeta(t, again, nastySecret)
	var env map[string]string
	if err := json.Unmarshal([]byte(again.meta["env_json"]), &env); err != nil || len(env) != 1 || env["A"] != "b" {
		t.Errorf("env_json = %q, want the caller's env only", again.meta["env_json"])
	}
	p, err := DecodePayload("build", again.payload)
	if err != nil || p.CloneToken != "clone-tok" || p.Script != "make\n" || len(p.Secrets) != 1 || p.Secrets["API_KEY"] != nastySecret {
		t.Errorf("redispatched payload = %+v, %v", p, err)
	}
	if legacy.meta["env_json"] != string(legacyEnv) {
		t.Error("the redispatch modified the meta it read back")
	}
}

func TestSecretsOutOfMeta(t *testing.T) {
	// Nothing to move: the very same map and payload come back.
	meta := map[string]string{"env_json": `{"A":"b"}`, "requester": "r"}
	payload := []byte("echo hi\n")
	gotMeta, gotPayload, err := secretsOutOfMeta(KindShell, []string{"TOKEN"}, meta, payload)
	if err != nil || len(gotMeta) != 2 || gotMeta["env_json"] != `{"A":"b"}` || string(gotPayload) != "echo hi\n" {
		t.Fatalf("no secret in meta: %v %q %v", gotMeta, gotPayload, err)
	}
	// A shell job whose env was only the secret loses env_json altogether and gains a frame.
	meta = map[string]string{"env_json": `{"TOKEN":"v"}`, "requester": "r"}
	gotMeta, gotPayload, err = secretsOutOfMeta(KindShell, []string{"TOKEN"}, meta, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gotMeta["env_json"]; ok || gotMeta["requester"] != "r" {
		t.Errorf("meta = %v", gotMeta)
	}
	if p, err := DecodePayload("shell", gotPayload); err != nil || p.Script != "echo hi\n" || p.Secrets["TOKEN"] != "v" {
		t.Errorf("payload = %+v, %v", p, err)
	}
	if meta["env_json"] != `{"TOKEN":"v"}` {
		t.Error("the input meta was modified")
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
