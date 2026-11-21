package dispatch

import (
	"context"
	"strings"
	"testing"
)

const placementImage = "ghcr.io/acme/builder@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func placementService(pools ...PoolConfig) *service {
	m := make(map[string]PoolConfig, len(pools))
	for _, pc := range pools {
		m[pc.Name] = pc
	}
	return &service{pools: m}
}

// A job describes what it needs and names no pool; the control plane decides where it runs.
func TestPlacePoolChoosesFromTheJobsDescription(t *testing.T) {
	macos := PoolConfig{Name: "macos", OS: "darwin", Toolchains: true, CPU: 8000, Memory: 10240}
	linux := PoolConfig{Name: "linux", OS: "linux", AllowedImages: []string{"ghcr.io/acme/"}, CPU: 4000, Memory: 4096}
	sealed := PoolConfig{Name: "a-sealed", OS: "darwin", Isolated: true}
	toolchain := &Toolchain{}
	cases := []struct {
		name  string
		pools []PoolConfig
		req   JobRequest
		want  string
		err   string
	}{
		{name: "a plain job goes to macOS", pools: []PoolConfig{linux, macos, sealed}, req: JobRequest{}, want: "macos"},
		{name: "a toolchain job goes to macOS", pools: []PoolConfig{linux, macos}, req: JobRequest{Environment: &Environment{Toolchain: toolchain}}, want: "macos"},
		{name: "an image with a toolchain still prefers macOS", pools: []PoolConfig{linux, macos}, req: JobRequest{Environment: &Environment{Image: placementImage, Toolchain: toolchain}}, want: "macos"},
		{name: "an image alone needs Linux", pools: []PoolConfig{linux, macos}, req: JobRequest{Environment: &Environment{Image: placementImage}}, want: "linux"},
		{name: "a tagged image is matched by repository", pools: []PoolConfig{linux, macos}, req: JobRequest{Environment: &Environment{Image: "ghcr.io/acme/builder:v1"}}, want: "linux"},
		{name: "no macOS pool: Linux takes it", pools: []PoolConfig{linux, sealed}, req: JobRequest{}, want: "linux"},
		{name: "a job too big for macOS goes on", pools: []PoolConfig{linux, {Name: "macos", OS: "darwin", CPU: 1000, Memory: 1024}}, req: JobRequest{Resources: &ResourceHint{CPU: 2000, Memory: 2048}}, want: "linux"},
		{name: "ties break by name", pools: []PoolConfig{{Name: "mac-b", OS: "darwin"}, {Name: "mac-a", OS: "darwin"}}, req: JobRequest{}, want: "mac-a"},
		{name: "a sealed pool is never chosen", pools: []PoolConfig{sealed}, req: JobRequest{}, err: "no shared pool"},
		{name: "an image nobody allows says why", pools: []PoolConfig{linux, macos}, req: JobRequest{Environment: &Environment{Image: "ghcr.io/other/x@sha256:" + strings.Repeat("0", 64)}}, err: "environments.allow does not permit"},
		{name: "a toolchain with toolchains off says why", pools: []PoolConfig{{Name: "macos", OS: "darwin"}}, req: JobRequest{Environment: &Environment{Toolchain: toolchain}}, err: "toolchains are not enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := placementService(tc.pools...).placePool(tc.req)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("placePool = %q, %v; want an error containing %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("placePool = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// Whatever placePool picks, Submit's own checks for that pool must accept: the two never disagree.
func TestPlacedPoolPassesSubmitsOwnChecks(t *testing.T) {
	macos := PoolConfig{Name: "macos", OS: "darwin", Toolchains: true}
	linux := PoolConfig{Name: "linux", OS: "linux", AllowedImages: []string{"ghcr.io/acme/"}}
	s := placementService(macos, linux)
	for _, env := range []*Environment{
		nil,
		{Toolchain: &Toolchain{}},
		{Image: placementImage},
		{Image: placementImage, Toolchain: &Toolchain{}},
	} {
		req := JobRequest{Kind: KindBuild, Environment: env}
		pool, err := s.placePool(req)
		if err != nil {
			t.Fatalf("placePool(%+v): %v", env, err)
		}
		req.Pool = pool
		if err := s.validateResources(req); err != nil {
			t.Errorf("pool %s: %v", pool, err)
		}
		if _, err := s.taskImage(req); err != nil {
			t.Errorf("pool %s refuses the job placed in it: %v", pool, err)
		}
	}
}

// A request with no pool is placed, and its record carries the pool chosen.
func TestSubmitPlacesAJobThatNamesNoPool(t *testing.T) {
	nc := &fakeNomad{}
	svc := newTestServiceWithOpts(t, nc, Options{Pools: []PoolConfig{
		{Name: "linux", OS: "linux"},
		{Name: "macos", OS: "darwin", Toolchains: true},
	}})
	job, _, err := svc.Submit(context.Background(), JobRequest{Kind: KindShell, Script: "true", Requester: "t"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job.Request.Pool != "macos" {
		t.Errorf("the job's record names pool %q, want macos", job.Request.Pool)
	}
	if len(nc.dispatchCalls) != 1 || nc.dispatchCalls[0].jobName != jobName(KindShell, "macos") {
		t.Errorf("dispatched %+v, want one dispatch of %s", nc.dispatchCalls, jobName(KindShell, "macos"))
	}
	// A caller that names a pool still gets it.
	job, _, err = svc.Submit(context.Background(), JobRequest{Kind: KindShell, Pool: "linux", Script: "true", Requester: "t"})
	if err != nil || job.Request.Pool != "linux" {
		t.Errorf("a named pool: %v, pool %q", err, job.Request.Pool)
	}
}
