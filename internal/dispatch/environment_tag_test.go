package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeImageResolver answers ResolveTag from a table and records what it was asked.
type fakeImageResolver struct {
	pinned map[string]string
	err    error
	asked  []string
}

func (f *fakeImageResolver) ResolveTag(_ context.Context, ref string) (string, error) {
	f.asked = append(f.asked, ref)
	if f.err != nil {
		return "", f.err
	}
	return f.pinned[ref], nil
}

func tagService(t *testing.T, nc *fakeNomad, resolver ImageResolver) *service {
	t.Helper()
	return newTestServiceWithOpts(t, nc, Options{
		Pools: []PoolConfig{
			{Name: "linux", RunnerImage: "ghcr.io/acme/runner:1", AllowedImages: []string{"ghcr.io/acme/"}},
			{Name: "macos", OS: "darwin", Toolchains: true},
			{Name: "plain"},
		},
		FallbackNoNodesAfter: time.Nanosecond, FallbackAfter: time.Hour, ImageResolver: resolver,
	}).(*service)
}

func TestSubmit_PinsAnEnvironmentTagToItsDigest(t *testing.T) {
	nc := fakeWithReadyPools()
	resolver := &fakeImageResolver{pinned: map[string]string{"ghcr.io/acme/builder:v1": "ghcr.io/acme/builder@" + digest}}
	svc := tagService(t, nc, resolver)
	req := buildReq("linux", &Environment{Image: "ghcr.io/acme/builder:v1"})

	job, _, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/acme/builder@" + digest
	if got := nc.dispatchCalls[0].meta["task_image"]; got != want {
		t.Errorf("task_image = %q, want the pinned %q", got, want)
	}
	if job.Request.Environment == nil || job.Request.Environment.Image != want {
		t.Errorf("the job record holds %+v, want the pinned image", job.Request.Environment)
	}
	if len(resolver.asked) != 1 || resolver.asked[0] != "ghcr.io/acme/builder:v1" {
		t.Errorf("resolver asked %v", resolver.asked)
	}
	if req.Environment.Image != "ghcr.io/acme/builder:v1" {
		t.Error("Submit rewrote the caller's request")
	}
	for key, value := range nc.dispatchCalls[0].meta {
		if strings.Contains(value, ":v1") {
			t.Errorf("meta %s still holds the tag: %q", key, value)
		}
	}
}

func TestSubmit_RejectsATagItCannotResolve(t *testing.T) {
	good := "ghcr.io/acme/builder@" + digest
	for name, tc := range map[string]struct {
		resolver ImageResolver
		image    string
		asked    int
	}{
		"registry error":       {&fakeImageResolver{err: errors.New("registry said 401")}, "ghcr.io/acme/builder:v1", 1},
		"resolver gives a tag": {&fakeImageResolver{pinned: map[string]string{"ghcr.io/acme/builder:v1": "ghcr.io/acme/builder:v1"}}, "ghcr.io/acme/builder:v1", 1},
		"other repository":     {&fakeImageResolver{pinned: map[string]string{"ghcr.io/acme/builder:v1": "ghcr.io/evil/builder@" + digest}}, "ghcr.io/acme/builder:v1", 1},
		"empty answer":         {&fakeImageResolver{}, "ghcr.io/acme/builder:v1", 1},
		"repository not allowed": {&fakeImageResolver{pinned: map[string]string{"ghcr.io/other/builder:v1": "ghcr.io/other/builder@" + digest}},
			"ghcr.io/other/builder:v1", 0},
		"no tag":       {&fakeImageResolver{}, "ghcr.io/acme/builder", 0},
		"no registry":  {&fakeImageResolver{}, "acme/builder:v1", 0},
		"no resolver":  {nil, "ghcr.io/acme/builder:v1", 0},
		"uppercase":    {&fakeImageResolver{}, "ghcr.io/Acme/builder:v1", 0},
		"digest alone": {&fakeImageResolver{}, "ghcr.io/acme/builder@sha256:abc", 0},
	} {
		t.Run(name, func(t *testing.T) {
			nc := fakeWithReadyPools()
			svc := tagService(t, nc, tc.resolver)
			_, _, err := svc.Submit(context.Background(), buildReq("linux", &Environment{Image: tc.image}))
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if len(nc.dispatchCalls) != 0 {
				t.Errorf("a refused job reached Nomad (falling back to the tag?): %+v", nc.dispatchCalls)
			}
			if r, ok := tc.resolver.(*fakeImageResolver); ok && len(r.asked) != tc.asked {
				t.Errorf("resolver asked %d times, want %d", len(r.asked), tc.asked)
			}
			if name == "registry error" && !strings.Contains(err.Error(), tc.image) {
				t.Errorf("error does not name the image: %v", err)
			}
		})
	}
	_ = good
}

func TestSubmit_DigestAndNativePathsNeverAskTheResolver(t *testing.T) {
	resolver := &fakeImageResolver{}
	nc := fakeWithReadyPools()
	svc := tagService(t, nc, resolver)
	if _, _, err := svc.Submit(context.Background(), buildReq("linux", &Environment{Image: "ghcr.io/acme/builder@" + digest})); err != nil {
		t.Fatal(err)
	}
	if len(resolver.asked) != 0 {
		t.Fatalf("a digest was sent to the resolver: %v", resolver.asked)
	}
	// A macOS toolchain job ignores the image: no registry request for it.
	nc = fakeWithReadyPools()
	svc = tagService(t, nc, resolver)
	req := buildReq("macos", &Environment{Image: "ghcr.io/acme/builder:v1", Toolchain: &Toolchain{Rust: &RustToolchain{Version: "1.90.0"}}})
	_, _, _ = svc.Submit(context.Background(), req)
	if len(resolver.asked) != 0 {
		t.Fatalf("a macOS toolchain job's ignored image was resolved: %v", resolver.asked)
	}
}

func TestParseTagRef(t *testing.T) {
	for ref, want := range map[string]string{
		"ghcr.io/acme/builder:v1":        "ghcr.io/acme/builder",
		"127.0.0.1:5000/acme/b:v1":       "127.0.0.1:5000/acme/b",
		"ghcr.io/acme/builder":           "",
		"127.0.0.1:5000/acme/b":          "",
		"ghcr.io/acme/builder@" + digest: "",
		"ghcr.io/acme/builder:":          "",
		"":                               "",
	} {
		repo, ok := ParseTagRef(ref)
		if repo != want || ok != (want != "") {
			t.Errorf("ParseTagRef(%q) = %q, %v; want %q", ref, repo, ok, want)
		}
	}
}
