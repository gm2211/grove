package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidateEnvironmentImage(t *testing.T) {
	allow := []string{"ghcr.io/acme/", "registry.example.com:5000/team/builder", "localhost/dev/tool"}
	cases := []struct {
		name string
		ref  string
		ok   bool
	}{
		{"prefix match", "ghcr.io/acme/builder@" + digest, true},
		{"prefix nested", "ghcr.io/acme/sub/x@" + digest, true},
		{"exact match with port", "registry.example.com:5000/team/builder@" + digest, true},
		{"localhost registry", "localhost/dev/tool@" + digest, true},
		{"no digest", "ghcr.io/acme/builder", false},
		{"tag only", "ghcr.io/acme/builder:latest", false},
		{"tag and digest", "ghcr.io/acme/builder:1@" + digest, false},
		{"no registry", "acme/builder@" + digest, false},
		{"bare name", "ubuntu@" + digest, false},
		{"hub-style first segment", "library/ubuntu/x@" + digest, false},
		{"prefix boundary", "ghcr.io/acme-evil/builder@" + digest, false},
		{"prefix is not the repo itself", "ghcr.io/acme@" + digest, false},
		{"exact entry is not a prefix", "registry.example.com:5000/team/builder2@" + digest, false},
		{"exact entry subpath", "registry.example.com:5000/team/builder/x@" + digest, false},
		{"other registry", "docker.io/acme/builder@" + digest, false},
		{"uppercase path", "ghcr.io/acme/Builder@" + digest, false},
		{"uppercase host", "GHCR.io/acme/builder@" + digest, false},
		{"short digest", "ghcr.io/acme/builder@sha256:abc", false},
		{"uppercase digest", "ghcr.io/acme/builder@sha256:" + strings.Repeat("A", 64), false},
		{"sha512", "ghcr.io/acme/builder@sha512:" + strings.Repeat("a", 64), false},
		{"dotdot", "ghcr.io/acme/../x@" + digest, false},
		{"empty component", "ghcr.io/acme//x@" + digest, false},
		{"newline", "ghcr.io/acme/x\n@" + digest, false},
		{"hcl interpolation", "ghcr.io/acme/${x}@" + digest, false},
		{"quote", "ghcr.io/acme/\"x@" + digest, false},
		{"two digests", "ghcr.io/acme/x@" + digest + "@" + digest, false},
		{"too long", "ghcr.io/acme/" + strings.Repeat("a", 600) + "@" + digest, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEnvironmentImage(tc.ref, allow)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateEnvironmentImage(%q) = %v, want ok=%v", tc.ref, err, tc.ok)
			}
		})
	}
	if err := ValidateEnvironmentImage("ghcr.io/acme/builder@"+digest, nil); err == nil {
		t.Error("an empty allowlist must allow nothing")
	}
}

func TestValidateAllowEntry(t *testing.T) {
	for entry, ok := range map[string]bool{
		"ghcr.io/acme/":            true,
		"ghcr.io/acme/builder":     true,
		"ghcr.io:443/acme/":        true,
		"acme/builder":             false,
		"ghcr.io/acme/b:tag":       false,
		"ghcr.io/acme/b@" + digest: false,
		"ghcr.io/":                 false,
		"ghcr.io":                  false,
		"":                         false,
		"ghcr.io/Acme/":            false,
	} {
		if err := ValidateAllowEntry(entry); (err == nil) != ok {
			t.Errorf("ValidateAllowEntry(%q) = %v, want ok=%v", entry, err, ok)
		}
	}
}

// FuzzValidateEnvironmentImage: whatever is accepted is a plain, single-line reference that is
// safe to drop into HCL, shell text and a docker pull, and sits under an allowed prefix.
func FuzzValidateEnvironmentImage(f *testing.F) {
	f.Add("ghcr.io/acme/builder@" + digest)
	f.Add("ghcr.io/acme/builder:latest")
	f.Add("a/b@sha256:")
	f.Fuzz(func(t *testing.T, ref string) {
		if err := ValidateEnvironmentImage(ref, []string{"ghcr.io/acme/"}); err != nil {
			return
		}
		if !strings.HasPrefix(ref, "ghcr.io/acme/") || strings.Count(ref, "@") != 1 || strings.Count(ref, ":") != 1 {
			t.Fatalf("accepted %q", ref)
		}
		for _, r := range ref {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("./_-@:", r)) {
				t.Fatalf("accepted %q with %q", ref, r)
			}
		}
		if _, err := RunnerImageOrDefault(ref); err != nil {
			t.Fatalf("accepted %q that RunnerImageOrDefault refuses: %v", ref, err)
		}
	})
}

func envService(t *testing.T, nc *fakeNomad, pools ...PoolConfig) *service {
	t.Helper()
	if pools == nil {
		pools = []PoolConfig{
			{Name: "linux", Fallback: "macos", RunnerImage: "ghcr.io/acme/runner:1", AllowedImages: []string{"ghcr.io/acme/"}},
			{Name: "macos", OS: "darwin", AllowedImages: []string{"ghcr.io/acme/"}},
			{Name: "sealed", Isolated: true, AllowedImages: []string{"ghcr.io/acme/"}},
			{Name: "plain"},
		}
	}
	return newTestServiceWithOpts(t, nc, Options{
		Pools: pools, FallbackNoNodesAfter: time.Nanosecond, FallbackAfter: time.Hour,
	}).(*service)
}

func buildReq(pool string, env *Environment) JobRequest {
	return JobRequest{Kind: KindBuild, Pool: pool, Repo: "https://github.com/gm2211/x", Script: "make", Environment: env}
}

func TestSubmit_EnvironmentSetsTaskImage(t *testing.T) {
	nc := fakeWithReadyPools()
	svc := envService(t, nc)
	image := "ghcr.io/acme/builder@" + digest
	job, _, err := svc.Submit(context.Background(), buildReq("linux", &Environment{Image: image}))
	if err != nil {
		t.Fatal(err)
	}
	if got := nc.dispatchCalls[0].meta["task_image"]; got != image {
		t.Errorf("task_image = %q, want %q", got, image)
	}
	if job.Request.Environment == nil || job.Request.Environment.Image != image {
		t.Errorf("the job record lost its environment: %+v", job.Request.Environment)
	}
}

func TestSubmit_RejectsEnvironmentThePoolCannotHonour(t *testing.T) {
	svc := envService(t, &fakeNomad{})
	good := "ghcr.io/acme/builder@" + digest
	cases := []struct {
		name string
		req  JobRequest
	}{
		{"tag", buildReq("linux", &Environment{Image: "ghcr.io/acme/builder:latest"})},
		{"no registry", buildReq("linux", &Environment{Image: "acme/builder@" + digest})},
		{"not allowed", buildReq("linux", &Environment{Image: "ghcr.io/other/builder@" + digest})},
		{"macos pool", buildReq("macos", &Environment{Image: good})},
		{"sealed pool", buildReq("sealed", &Environment{Image: good})},
		{"pool without environments", buildReq("plain", &Environment{Image: good})},
		{"unknown pool", buildReq("nope", &Environment{Image: good})},
		{"shell kind", JobRequest{Kind: KindShell, Pool: "linux", Script: "true", Environment: &Environment{Image: good}}},
		{"agent kind", JobRequest{Kind: KindAgent, Pool: "linux", Repo: "https://github.com/x/y", Script: "true", Environment: &Environment{Image: good}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := fakeWithReadyPools()
			svc.nomad = nc
			if _, _, err := svc.Submit(context.Background(), tc.req); err == nil {
				t.Fatal("expected a refusal")
			}
			if len(nc.dispatchCalls) != 0 {
				t.Errorf("a refused job reached Nomad: %+v", nc.dispatchCalls)
			}
		})
	}
}

func TestSubmit_CallerMetaImageIsNeverForwarded(t *testing.T) {
	nc := fakeWithReadyPools()
	svc := envService(t, nc)
	req := buildReq("linux", nil)
	req.Meta = map[string]string{"image": "evil.example/x:latest", "task_image": "evil.example/y:latest"}
	if _, _, err := svc.Submit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	meta := nc.dispatchCalls[0].meta
	if _, ok := meta["image"]; ok {
		t.Errorf("meta.image forwarded: %q", meta["image"])
	}
	if meta["task_image"] != "ghcr.io/acme/runner:1" {
		t.Errorf("task_image = %q, want the pool's runner image", meta["task_image"])
	}
	if strings.Contains(meta["grove_meta_json"], "evil") == false {
		t.Errorf("caller meta should still round-trip in grove_meta_json")
	}
}

func TestSubmit_BuildAlwaysGetsTaskImage(t *testing.T) {
	for _, pools := range [][]PoolConfig{
		nil,
		{{Name: "linux"}},
		{{Name: "linux", RunnerImage: "ghcr.io/acme/runner:2"}},
		{{Name: "linux", Isolated: true}},
	} {
		nc := fakeWithReadyPools()
		svc := envService(t, nc, pools...)
		if _, _, err := svc.Submit(context.Background(), buildReq("linux", nil)); err != nil {
			t.Fatal(err)
		}
		if nc.dispatchCalls[0].meta["task_image"] == "" {
			t.Errorf("pools %+v: a build dispatch carries no task_image", pools)
		}
	}
	// The other kinds' templates don't take one.
	nc := fakeWithReadyPools()
	svc := envService(t, nc)
	if _, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := nc.dispatchCalls[0].meta["task_image"]; ok {
		t.Error("a shell dispatch carries task_image")
	}
}

func TestFallback_EnvironmentNeverFallsBack(t *testing.T) {
	nc := fakeWithReadyPools()
	svc := envService(t, nc)
	job, _, err := svc.Submit(context.Background(), buildReq("linux", &Environment{Image: "ghcr.io/acme/builder@" + digest}))
	if err != nil {
		t.Fatal(err)
	}
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-build-linux/dispatch-1": noLinuxNodes()}
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fallback != nil || len(nc.dispatchCalls) != 1 {
		t.Fatalf("a job with an environment moved to another pool: %+v", got.Fallback)
	}

	// The same job without an environment does fall back, so the test above isolates the cause.
	nc2 := fakeWithReadyPools()
	svc2 := envService(t, nc2)
	job2, _, _ := svc2.Submit(context.Background(), buildReq("linux", nil))
	nc2.evaluationsByJob = map[string][]nomad.Evaluation{"grove-build-linux/dispatch-1": noLinuxNodes()}
	if got2, _ := svc2.Get(context.Background(), job2.ID); got2.Fallback == nil {
		t.Fatal("control job did not fall back")
	}
}

func TestUnplaceableReason_EnvironmentIsPinned(t *testing.T) {
	svc := envService(t, &fakeNomad{})
	svc.opts.PoolHealth = fakePoolHealth{"linux": "linux is broken"}
	svc.opts.UnplaceableGrace = time.Millisecond
	old := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		env  *Environment
		want string
	}{
		{nil, ""}, // may still fall back to macOS, so not yet unplaceable
		{&Environment{Image: "ghcr.io/acme/b@" + digest}, "linux is broken"},
	} {
		rec := &record{Job: Job{Request: buildReq("linux", tc.env), SubmittedAt: old}}
		if got := svc.unplaceableReason(rec, noLinuxNodes(), time.Now()); got != tc.want {
			t.Errorf("env=%v: unplaceableReason = %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestRedispatch_KeepsTaskImage(t *testing.T) {
	nc := fakeWithReadyPools()
	svc := envService(t, nc)
	image := "ghcr.io/acme/builder@" + digest
	job, _, err := svc.Submit(context.Background(), buildReq("linux", &Environment{Image: image}))
	if err != nil {
		t.Fatal(err)
	}
	nc.dispatchResult = &nomad.DispatchResult{DispatchedJobID: "grove-build-linux/dispatch-2", EvalID: "e2"}
	rec, ok := svc.store.get(job.ID)
	if !ok {
		t.Fatal("no record")
	}
	if err := svc.redispatch(context.Background(), rec, "linux"); err != nil {
		t.Fatal(err)
	}
	if got := nc.dispatchCalls[1].meta["task_image"]; got != image {
		t.Errorf("redispatched task_image = %q, want %q", got, image)
	}

	// A build dispatched before task_image existed is given its pool's runner image, but a job that
	// asked for an environment and lost it is not guessed at.
	legacy := nc.dispatchCalls[0]
	delete(legacy.meta, "task_image")
	nc.dispatched["grove-build-linux/dispatch-2"] = legacy
	if err := svc.redispatch(context.Background(), rec, "linux"); err == nil {
		t.Error("redispatch of an environment job without task_image must fail closed")
	}
	rec.Request.Environment = nil
	nc.dispatchResult = &nomad.DispatchResult{DispatchedJobID: "grove-build-linux/dispatch-3", EvalID: "e3"}
	if err := svc.redispatch(context.Background(), rec, "linux"); err != nil {
		t.Fatal(err)
	}
	if got := nc.dispatchCalls[len(nc.dispatchCalls)-1].meta["task_image"]; got != "ghcr.io/acme/runner:1" {
		t.Errorf("legacy redispatch task_image = %q", got)
	}
}
