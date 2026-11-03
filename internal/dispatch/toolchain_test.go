package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

func tc(rust string, comps []string, node, python string) *Toolchain {
	t := &Toolchain{}
	if rust != "" {
		t.Rust = &RustToolchain{Version: rust, Components: comps}
	}
	if node != "" {
		t.Node = &ToolVersion{Version: node}
	}
	if python != "" {
		t.Python = &ToolVersion{Version: python}
	}
	return t
}

func TestValidateToolchain(t *testing.T) {
	cases := []struct {
		name string
		tc   *Toolchain
		ok   bool
	}{
		{"nil is no toolchain", nil, true},
		{"all three", tc("1.99.1", []string{"clippy", "rustfmt"}, "22.11.0", "3.12.7"), true},
		{"rust only", tc("1.80.0", nil, "", ""), true},
		{"all components", tc("1.99.1", []string{"clippy", "rustfmt", "rust-src", "llvm-tools"}, "", ""), true},
		{"duplicate components", tc("1.99.1", []string{"clippy", "clippy"}, "", ""), true},
		{"empty toolchain", &Toolchain{}, false},
		{"rust channel", tc("nightly", nil, "", ""), false},
		{"rust stable", tc("stable", nil, "", ""), false},
		{"rust partial", tc("1.99", nil, "", ""), false},
		{"rust 2.x", tc("2.0.0", nil, "", ""), false},
		{"rust prerelease", tc("1.99.1-beta", nil, "", ""), false},
		{"rust empty", &Toolchain{Rust: &RustToolchain{}}, false},
		{"rust range", tc("^1.99.1", nil, "", ""), false},
		{"rust unknown component", tc("1.99.1", []string{"miri"}, "", ""), false},
		{"rust empty component", tc("1.99.1", []string{""}, "", ""), false},
		{"node major only", tc("", nil, "22", ""), false},
		{"node latest", tc("", nil, "latest", ""), false},
		{"node v prefix", tc("", nil, "v22.11.0", ""), false},
		{"node lts", tc("", nil, "lts/*", ""), false},
		{"python minor only", tc("", nil, "", "3.12"), false},
		{"python 2", tc("", nil, "", "2.7.18"), false},
		{"python 4", tc("", nil, "", "4.0.0"), false},
		{"python suffix", tc("", nil, "", "3.13.0rc1"), false},
		{"newline", tc("", nil, "22.11.0\n", ""), false},
		{"trailing space", tc("", nil, "22.11.0 ", ""), false},
		{"shell", tc("", nil, "22.1.0;rm", ""), false},
		{"unicode digit", tc("", nil, "٢٢.1.0", ""), false},
		{"too big", tc("1.99.1", manyComponents(300), "", ""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateToolchain(c.tc); (err == nil) != c.ok {
				t.Fatalf("ValidateToolchain = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

func manyComponents(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "clippy"
	}
	return out
}

func TestToolchain_UnknownToolOrFieldIsRefusedWhenDecoded(t *testing.T) {
	for _, body := range []string{
		`{"go":{"version":"1.22.0"}}`,
		`{"rust":{"version":"1.99.1","channel":"nightly"}}`,
		`{"node":{"version":"22.11.0","extra":1}}`,
	} {
		var e Environment
		if err := json.Unmarshal([]byte(`{"toolchain":`+body+`}`), &e); err == nil {
			t.Errorf("%s decoded", body)
		}
	}
	var e Environment
	if err := json.Unmarshal([]byte(`{"toolchain":{"node":{"version":"22.11.0"}}}`), &e); err != nil || e.Toolchain.Node.Version != "22.11.0" {
		t.Fatalf("decode: %v %+v", err, e)
	}
}

func TestToolchain_CanonicalAndHashAreStable(t *testing.T) {
	a := tc("1.99.1", []string{"rustfmt", "clippy", "clippy"}, "22.11.0", "3.12.7")
	b := tc("1.99.1", []string{"clippy", "rustfmt"}, "22.11.0", "3.12.7")
	const want = `{"node":{"version":"22.11.0"},"python":{"version":"3.12.7"},"rust":{"version":"1.99.1","components":["clippy","rustfmt"]}}`
	if a.canonical() != want || b.canonical() != want {
		t.Fatalf("canonical = %s / %s, want %s", a.canonical(), b.canonical(), want)
	}
	if a.hash() != b.hash() {
		t.Error("equal toolchains hash differently")
	}
	// Frozen: a change here invalidates every worker's cache, so it must be deliberate.
	if got := a.hash(); got != "d799ded764531b9ff55247295d82485a9ce770e96f4b350245c0c778abe94ea4" {
		t.Errorf("hash = %q", got)
	}
	if c := tc("1.99.1", nil, "", "").canonical(); c != `{"rust":{"version":"1.99.1"}}` {
		t.Errorf("canonical without components = %s", c)
	}
	if tc("1.99.1", nil, "", "").hash() == tc("1.99.2", nil, "", "").hash() {
		t.Error("different versions share a hash")
	}
	// Order of input never matters.
	var reordered Toolchain
	if err := json.Unmarshal([]byte(`{"python":{"version":"3.12.7"},"rust":{"components":["rustfmt","clippy"],"version":"1.99.1"},"node":{"version":"22.11.0"}}`), &reordered); err != nil {
		t.Fatal(err)
	}
	if reordered.canonical() != want || reordered.hash() != b.hash() {
		t.Errorf("reordered input canonicalises to %s", reordered.canonical())
	}
}

func FuzzToolchain(f *testing.F) {
	f.Add(`{"rust":{"version":"1.99.1","components":["clippy"]}}`)
	f.Add(`{"node":{"version":"22.11.0"},"python":{"version":"3.12.7"}}`)
	f.Add(`{"node":{"version":"22"}}`)
	f.Fuzz(func(t *testing.T, body string) {
		var tcn Toolchain
		if err := json.Unmarshal([]byte(body), &tcn); err != nil {
			return
		}
		if ValidateToolchain(&tcn) != nil {
			return
		}
		c := tcn.canonical()
		if len(c) > maxToolchainBytes {
			t.Fatalf("canonical is %d bytes", len(c))
		}
		for _, r := range c {
			if r >= 0x7f || r < 0x20 {
				t.Fatalf("canonical %q has non-printable-ASCII %q", c, r)
			}
		}
		// Canonical form is a fixed point: decoding it and canonicalising again changes nothing.
		var again Toolchain
		if err := json.Unmarshal([]byte(c), &again); err != nil {
			t.Fatalf("canonical %q does not decode: %v", c, err)
		}
		if ValidateToolchain(&again) != nil || again.canonical() != c || again.hash() != tcn.hash() {
			t.Fatalf("canonical %q is not stable", c)
		}
	})
}

func toolchainService(t *testing.T, nc *fakeNomad) *service {
	t.Helper()
	return envService(t, nc,
		PoolConfig{Name: "linux", Fallback: "macos", RunnerImage: "ghcr.io/acme/runner:1", AllowedImages: []string{"ghcr.io/acme/"}, CPU: 4000, Memory: 8192},
		PoolConfig{Name: "macos", OS: "darwin", Toolchains: true, CPU: 4000, Memory: 8192},
		PoolConfig{Name: "macos-plain", OS: "darwin"},
		PoolConfig{Name: "sealed-mac", OS: "darwin", Isolated: true, Toolchains: true},
		PoolConfig{Name: "linux-tc", Fallback: "macos-plain", AllowedImages: []string{"ghcr.io/acme/"}},
		PoolConfig{Name: "linux-sealed-fb", Fallback: "sealed-mac", AllowedImages: []string{"ghcr.io/acme/"}},
	)
}

var goodToolchain = func() *Toolchain { return tc("1.99.1", []string{"rustfmt", "clippy"}, "22.11.0", "") }

func TestSubmit_ToolchainSetsMetaFromCanonicalForm(t *testing.T) {
	nc := &fakeNomad{}
	svc := toolchainService(t, nc)
	image := "ghcr.io/acme/builder@" + digest
	req := buildReq("linux", &Environment{Image: image, Toolchain: goodToolchain()})
	req.Meta = map[string]string{"toolchain_json": "evil", "toolchain_hash": "evil"}
	if _, _, err := svc.Submit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	meta := nc.dispatchCalls[0].meta
	want := goodToolchain()
	if meta["toolchain_json"] != want.canonical() || meta["toolchain_hash"] != want.hash() {
		t.Errorf("meta = %q / %q", meta["toolchain_json"], meta["toolchain_hash"])
	}
	if meta["task_image"] != image {
		t.Errorf("task_image = %q", meta["task_image"])
	}
	// No toolchain, no keys.
	nc2 := &fakeNomad{}
	svc2 := toolchainService(t, nc2)
	r2 := buildReq("linux", &Environment{Image: image})
	r2.Meta = map[string]string{"toolchain_json": "evil"}
	if _, _, err := svc2.Submit(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	if _, ok := nc2.dispatchCalls[0].meta["toolchain_json"]; ok {
		t.Error("caller Meta reached toolchain_json")
	}
}

func TestSubmit_ToolchainOnMacOS(t *testing.T) {
	nc := &fakeNomad{}
	svc := toolchainService(t, nc)
	// Image is optional and ignored; the pool's runner image is task_image.
	for _, img := range []string{"", "ghcr.io/anything:latest"} {
		if _, _, err := svc.Submit(context.Background(), buildReq("macos", &Environment{Image: img, Toolchain: goodToolchain()})); err != nil {
			t.Fatalf("image %q: %v", img, err)
		}
	}
	for _, c := range nc.dispatchCalls {
		if c.meta["task_image"] != DefaultRunnerImage || c.meta["toolchain_hash"] == "" {
			t.Errorf("meta = %v", c.meta)
		}
	}
	for name, req := range map[string]JobRequest{
		"no toolchain":            buildReq("macos", &Environment{Image: "ghcr.io/acme/b@" + digest}),
		"pool without toolchains": buildReq("macos-plain", &Environment{Toolchain: goodToolchain()}),
		"sealed macos":            buildReq("sealed-mac", &Environment{Toolchain: goodToolchain()}),
		"bad version":             buildReq("macos", &Environment{Toolchain: tc("", nil, "22", "")}),
		"empty toolchain":         buildReq("macos", &Environment{Toolchain: &Toolchain{}}),
		"linux needs image":       buildReq("linux", &Environment{Toolchain: goodToolchain()}),
		"linux bad toolchain":     buildReq("linux", &Environment{Image: "ghcr.io/acme/b@" + digest, Toolchain: tc("nightly", nil, "", "")}),
		"shell kind":              {Kind: KindShell, Pool: "macos", Script: "true", Environment: &Environment{Toolchain: goodToolchain()}},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(nc.dispatchCalls)
			if _, _, err := svc.Submit(context.Background(), req); err == nil {
				t.Fatal("expected a refusal")
			}
			if len(nc.dispatchCalls) != before {
				t.Error("a refused job reached Nomad")
			}
		})
	}
}

func TestEffectivePin_Toolchain(t *testing.T) {
	img := &Environment{Image: "ghcr.io/acme/b@" + digest}
	withTC := &Environment{Image: img.Image, Toolchain: goodToolchain()}
	for _, c := range []struct {
		req  JobRequest
		want bool
	}{
		{JobRequest{}, false},
		{JobRequest{PinPool: true}, true},
		{JobRequest{Environment: img}, true},
		{JobRequest{Environment: withTC}, false},
		{JobRequest{Environment: withTC, PinPool: true}, true},
	} {
		if got := effectivePin(c.req); got != c.want {
			t.Errorf("effectivePin(%+v) = %v, want %v", c.req, got, c.want)
		}
	}
}

// fallsBack submits req to its pool, reports no nodes there, and says whether the job moved.
func fallsBack(t *testing.T, req JobRequest) (*fakeNomad, bool) {
	t.Helper()
	nc := &fakeNomad{}
	svc := toolchainService(t, nc)
	job, _, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	nc.evaluationsByJob = map[string][]nomad.Evaluation{"grove-build-" + req.Pool + "/dispatch-1": {{Status: "blocked", FailedTGAllocs: map[string]nomad.AllocationMetric{
		"main": {ConstraintFiltered: map[string]int{"x": 1}, NodesEvaluated: 1},
	}}}}
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return nc, got.Fallback != nil
}

func TestFallback_ToolchainJobMovesToMacOSWithToolchains(t *testing.T) {
	env := &Environment{Image: "ghcr.io/acme/builder@" + digest, Toolchain: goodToolchain()}
	nc, moved := fallsBack(t, buildReq("linux", env))
	if !moved || len(nc.dispatchCalls) != 2 || nc.dispatchCalls[1].jobName != "grove-build-macos" {
		t.Fatalf("a toolchain job did not fall back to macos: %+v", nc.dispatchCalls)
	}
	// Byte for byte, both keys.
	a, b := nc.dispatchCalls[0].meta, nc.dispatchCalls[1].meta
	for _, k := range []string{"toolchain_json", "toolchain_hash"} {
		if a[k] == "" || a[k] != b[k] {
			t.Errorf("%s: %q -> %q", k, a[k], b[k])
		}
	}
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("meta changed in the move: %v -> %v", a, b)
	}
}

func TestFallback_ToolchainJobNeverMovesToTheWrongPool(t *testing.T) {
	env := &Environment{Image: "ghcr.io/acme/builder@" + digest, Toolchain: goodToolchain()}
	for name, pool := range map[string]string{
		"macos pool without toolchains": "linux-tc",
		"sealed macos pool":             "linux-sealed-fb",
	} {
		t.Run(name, func(t *testing.T) {
			if nc, moved := fallsBack(t, buildReq(pool, env)); moved || len(nc.dispatchCalls) != 1 {
				t.Fatalf("moved to %s: %+v", pool, nc.dispatchCalls)
			}
		})
	}
	t.Run("macos job size smaller than requested", func(t *testing.T) {
		nc := &fakeNomad{}
		svc := envService(t, nc,
			PoolConfig{Name: "linux", Fallback: "macos", AllowedImages: []string{"ghcr.io/acme/"}, CPU: 8000, Memory: 16384},
			PoolConfig{Name: "macos", OS: "darwin", Toolchains: true, CPU: 2000, Memory: 4096},
		)
		for name, res := range map[string]ResourceHint{"cpu": {CPU: 4000, Memory: 1024}, "memory": {CPU: 1000, Memory: 8192}} {
			req := buildReq("linux", env)
			req.Resources = &res
			job, _, err := svc.Submit(context.Background(), req)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			rec, _ := svc.store.get(job.ID)
			if target, _ := svc.fallbackTarget(rec, noLinuxNodes(), time.Now().Add(time.Hour)); target != "" {
				t.Errorf("%s: fell back to %q whose jobs are too small", name, target)
			}
		}
		req := buildReq("linux", env)
		req.Resources = &ResourceHint{CPU: 2000, Memory: 4096}
		job, _, _ := svc.Submit(context.Background(), req)
		rec, _ := svc.store.get(job.ID)
		if target, _ := svc.fallbackTarget(rec, noLinuxNodes(), time.Now().Add(time.Hour)); target != "macos" {
			t.Errorf("a job that fits stayed: target %q", target)
		}
	})
}

func TestFallback_ImageOnlyEnvironmentStillPinned(t *testing.T) {
	if nc, moved := fallsBack(t, buildReq("linux", &Environment{Image: "ghcr.io/acme/builder@" + digest})); moved || len(nc.dispatchCalls) != 1 {
		t.Fatal("an environment without a toolchain moved to another pool")
	}
}

func TestRedispatch_ToolchainCarriedAndFailsClosed(t *testing.T) {
	nc := &fakeNomad{}
	svc := toolchainService(t, nc)
	job, _, err := svc.Submit(context.Background(), buildReq("linux", &Environment{Image: "ghcr.io/acme/b@" + digest, Toolchain: goodToolchain()}))
	if err != nil {
		t.Fatal(err)
	}
	nc.dispatchResult = &nomad.DispatchResult{DispatchedJobID: "grove-build-macos/dispatch-2", EvalID: "e2"}
	rec, _ := svc.store.get(job.ID)
	if err := svc.redispatch(context.Background(), rec, "macos"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"toolchain_json", "toolchain_hash"} {
		if nc.dispatchCalls[1].meta[k] != nc.dispatchCalls[0].meta[k] || nc.dispatchCalls[1].meta[k] == "" {
			t.Errorf("%s not carried: %q", k, nc.dispatchCalls[1].meta[k])
		}
	}
	// A dispatch that lost the toolchain is never re-sent without it.
	lost := nc.dispatchCalls[0]
	delete(lost.meta, "toolchain_json")
	nc.dispatched["grove-build-macos/dispatch-2"] = lost
	n := len(nc.dispatchCalls)
	if err := svc.redispatch(context.Background(), rec, "macos"); err == nil || len(nc.dispatchCalls) != n {
		t.Errorf("redispatch without toolchain_json: err=%v", err)
	}
}
