package install

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func applyHeadlessTart(t *testing.T, r *FakeRunner, opts Options) error {
	t.Helper()
	step := tartHeadlessStep(r, opts)
	return step.Apply(context.Background())
}

func TestTartHeadlessStep_CopiesVerifiesAndResigns(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	r := NewFakeRunner()
	scriptTartSource(r, "2.41.1")
	if err := applyHeadlessTart(t, r, opts); err != nil {
		t.Fatal(err)
	}
	tmp := groveTartAppDir(opts) + ".new"
	plist := tmp + "/Contents/Info.plist"
	for _, want := range []string{
		"brew --prefix tart",
		"/usr/bin/codesign --verify --strict " + fakeTartApp,
		"/usr/bin/codesign -dv " + fakeTartApp,
		"/usr/bin/ditto " + fakeTartApp + " " + tmp,
		"/usr/bin/plutil -replace LSUIElement -bool YES " + plist,
		"/usr/bin/plutil -replace CFBundleIdentifier -string com.gm2211.grove.tart " + plist,
		"/usr/bin/plutil -replace CFBundleName -string Grove Tart " + plist,
		"/usr/bin/plutil -replace CFBundleDisplayName -string Grove Tart " + plist,
		"/usr/bin/codesign --force --sign - --entitlements " + groveTartEntitlementsPath(opts) +
			" --identifier com.gm2211.grove.tart --timestamp=none " + tmp,
	} {
		if !r.CalledWith(want) {
			t.Errorf("missing call %q", want)
		}
	}
	for _, c := range r.Calls {
		if strings.Contains(c.String(), "vm.networking") || strings.Contains(c.String(), "runtime") {
			t.Errorf("unexpected %s", c)
		}
	}
	ents, _ := os.ReadFile(groveTartEntitlementsPath(opts))
	if !strings.Contains(string(ents), entVirtualization) || strings.Contains(string(ents), entVMNetworking) {
		t.Errorf("entitlements: %s", ents)
	}
	prov, err := os.ReadFile(groveTartProvenancePath(opts))
	if err != nil || !strings.Contains(string(prov), "version=2.41.1") || !strings.Contains(string(prov), "cdhash=fedc1ee5") {
		t.Errorf("provenance %q, %v", prov, err)
	}
	shim, _ := os.ReadFile(groveTartShimPath(opts))
	if !strings.HasPrefix(string(shim), "#!/bin/sh\nexec \"") || !strings.Contains(string(shim), groveTartExecutable(opts)+`" "$@"`) {
		t.Errorf("shim: %s", shim)
	}
	if fi, _ := os.Stat(groveTartShimPath(opts)); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("shim mode %v", fi)
	}
}

func TestTartHeadlessStep_RefusesAForeignBundle(t *testing.T) {
	for name, mutate := range map[string]func(*FakeRunner){
		"bundle id": func(r *FakeRunner) {
			r.Script("/usr/bin/plutil -extract CFBundleIdentifier raw -o - "+fakeTartApp+"/Contents/Info.plist", FakeResult{Stdout: "com.evil.tart\n"})
		},
		"team": func(r *FakeRunner) {
			r.Script("/usr/bin/codesign -dv "+fakeTartApp, FakeResult{Stderr: "CDHash=aa\nTeamIdentifier=EVIL000000\n"})
		},
		"signature": func(r *FakeRunner) {
			r.Script("/usr/bin/codesign --verify --strict "+fakeTartApp, FakeResult{Err: errors.New("invalid")})
		},
		"no brew tart": func(r *FakeRunner) { r.Script("brew --prefix tart", FakeResult{Err: errors.New("no formula")}) },
	} {
		t.Run(name, func(t *testing.T) {
			opts := testWorkerOptions(t.TempDir())
			r := NewFakeRunner()
			scriptTartSource(r, "2.41.1")
			mutate(r)
			if err := applyHeadlessTart(t, r, opts); err == nil {
				t.Fatal("expected a refusal")
			}
			for _, c := range r.Calls {
				if c.Name == "/usr/bin/ditto" {
					t.Error("copied a bundle it refused")
				}
			}
			if _, err := os.Stat(groveTartAppDir(opts)); err == nil {
				t.Error("installed a refused bundle")
			}
		})
	}
}

// laysDownCopy writes what a finished install leaves on disk and scripts the runner to match.
func laysDownCopy(t *testing.T, r *FakeRunner, opts Options, version string) {
	t.Helper()
	scriptTartSource(r, version)
	src := tartSource{App: fakeTartApp, Version: "2.41.1", CDHash: "fedc1ee5"}
	for p, c := range map[string]string{
		groveTartExecutable(opts):     "bin",
		groveTartProvenancePath(opts): src.provenance(),
		groveTartShimPath(opts):       groveTartShim(opts),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	app := groveTartAppDir(opts)
	r.Script("/usr/bin/plutil -extract LSUIElement raw -o - "+groveTartInfoPlist(opts), FakeResult{Stdout: "true\n"})
	r.Script("/usr/bin/codesign -d --entitlements :- "+app, FakeResult{Stdout: "<key>" + entVirtualization + "</key><true/>"})
}

func TestTartHeadlessStep_SkippedWhenProvenanceMatches(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	r := NewFakeRunner()
	laysDownCopy(t, r, opts, "2.41.1")
	step := tartHeadlessStep(r, opts)
	if ok, err := step.Check(context.Background()); err != nil || !ok {
		t.Fatalf("Check = %v, %v; want satisfied", ok, err)
	}
	// brew upgrade tart: the copy is stale.
	r2 := NewFakeRunner()
	laysDownCopy(t, r2, opts, "2.42.0")
	step = tartHeadlessStep(r2, opts)
	if ok, _ := step.Check(context.Background()); ok {
		t.Error("a copy of an older Tart still satisfies the step")
	}
	// Not background-only any more.
	r3 := NewFakeRunner()
	laysDownCopy(t, r3, opts, "2.41.1")
	r3.Script("/usr/bin/plutil -extract LSUIElement raw -o - "+groveTartInfoPlist(opts), FakeResult{Err: errors.New("no value")})
	step = tartHeadlessStep(r3, opts)
	if ok, _ := step.Check(context.Background()); ok {
		t.Error("a Dock-visible copy satisfies the step")
	}
	// vm.networking crept back in.
	r4 := NewFakeRunner()
	laysDownCopy(t, r4, opts, "2.41.1")
	r4.Script("/usr/bin/codesign -d --entitlements :- "+groveTartAppDir(opts), FakeResult{Stdout: entVirtualization + entVMNetworking})
	step = tartHeadlessStep(r4, opts)
	if ok, _ := step.Check(context.Background()); ok {
		t.Error("a copy with vm.networking satisfies the step")
	}
}

func TestTartHeadlessStep_DoctorCheck(t *testing.T) {
	ctx := context.Background()
	opts := testWorkerOptions(t.TempDir())
	good := func(r *FakeRunner) {
		laysDownCopy(t, r, opts, "2.41.1")
		if err := os.WriteFile(workerWrapperPath(opts), []byte(workerWrapper(opts, "/x/orchard")), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r := NewFakeRunner()
	good(r)
	if c := checkHeadlessTart(ctx, r, opts); !c.OK {
		t.Fatalf("healthy: %+v", c)
	}
	// Missing copy.
	if c := checkHeadlessTart(ctx, NewFakeRunner(), testWorkerOptions(t.TempDir())); c.OK || !strings.Contains(c.Remediation, "grove install --role worker") {
		t.Errorf("missing: %+v", c)
	}
	// Brew upgraded.
	r = NewFakeRunner()
	good(r)
	scriptTartSource(r, "2.42.0")
	if c := checkHeadlessTart(ctx, r, opts); c.OK || !strings.Contains(c.Detail, "2.42.0") {
		t.Errorf("stale: %+v", c)
	}
	// Unsigned.
	r = NewFakeRunner()
	good(r)
	r.Script("/usr/bin/codesign --verify --strict "+groveTartAppDir(opts), FakeResult{Err: errors.New("bad")})
	if c := checkHeadlessTart(ctx, r, opts); c.OK || !strings.Contains(c.Detail, "signature") {
		t.Errorf("unsigned: %+v", c)
	}
	// Old wrapper.
	r = NewFakeRunner()
	good(r)
	_ = os.WriteFile(workerWrapperPath(opts), []byte("export PATH=\"$HOME/.local/bin:/opt/homebrew/bin\"\n"), 0o700)
	if c := checkHeadlessTart(ctx, r, opts); c.OK || !strings.Contains(c.Detail, "PATH") {
		t.Errorf("old wrapper: %+v", c)
	}
}

func TestUninstallPlan_RemovesGroveTart(t *testing.T) {
	opts := uninstallTestOptions(t)
	installedWorkerFixture(t, opts)
	for _, p := range []string{groveTartAppDir(opts), groveTartBinDir(opts), groveTartEntitlementsPath(opts)} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := BuildUninstallPlan(NewFakeRunner(), opts, UninstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Name == "remove:grove-tart-app" {
			if err := s.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{groveTartAppDir(opts), groveTartBinDir(opts), groveTartEntitlementsPath(opts)} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Errorf("%s remains", p)
				}
			}
			return
		}
	}
	t.Fatal("no remove:grove-tart-app step")
}

func TestHeal_OldWrapperGainsTheHeadlessTartPrefix(t *testing.T) {
	e := newHealEnv(t)
	opts := e.opts()
	opts.Controller, opts.Token, opts.Hostname = "https://cp.tailnet.ts.net:6120", "tok-123456", "studio"
	e.write(workerWrapperPath(opts), "#!/bin/zsh\nexport PATH=\"$HOME/.local/bin:/opt/homebrew/bin:/usr/bin\"\nexec orchard worker run --name studio --guest-agent-transport\n", 0o700)
	results, out, err := e.install(opts)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if n := needed(results, "orchard-worker-wrapper", "tart-headless-app"); len(n) != 2 {
		t.Errorf("heal skipped %v", n)
	}
	w := e.read(workerWrapperPath(opts))
	if !strings.Contains(w, workerWrapperTartPathPrefix) || !strings.Contains(w, "exit 78") {
		t.Errorf("wrapper not healed:\n%s", w)
	}
}

func TestEnsureGroveTart_RealCodeSign(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS codesign/plutil/ditto")
	}
	home := t.TempDir()
	opts := testWorkerOptions(home)
	// A fake upstream bundle: /usr/bin/true as the executable, and a plist.
	src := filepath.Join(home, "upstream", "tart.app")
	if err := os.MkdirAll(filepath.Join(src, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin, _ := os.ReadFile("/usr/bin/true")
	if err := os.WriteFile(filepath.Join(src, "Contents", "MacOS", "tart"), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Contents", "embedded.provisionprofile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Contents", "Info.plist"), []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>tart</string>
<key>CFBundleIdentifier</key><string>com.github.cirruslabs.tart</string>
<key>CFBundleName</key><string>Tart</string>
<key>CFBundleShortVersionString</key><string>2.41.1</string>
</dict></plist>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", src).CombinedOutput(); err != nil {
		t.Skipf("cannot sign the fake bundle here: %v\n%s", err, out)
	}
	err := buildGroveTart(context.Background(), ExecRunner{}, opts, tartSource{App: src, Version: "2.41.1", CDHash: "abc"})
	if err != nil {
		t.Fatalf("buildGroveTart: %v", err)
	}
	app := groveTartAppDir(opts)
	if out, err := exec.Command("/usr/bin/codesign", "--verify", "--strict", app).CombinedOutput(); err != nil {
		t.Fatalf("copy does not verify: %v\n%s", err, out)
	}
	ents, _ := exec.Command("/usr/bin/codesign", "-d", "--entitlements", ":-", app).Output()
	if !strings.Contains(string(ents), entVirtualization) || strings.Contains(string(ents), entVMNetworking) {
		t.Errorf("entitlements:\n%s", ents)
	}
	if v, err := plutilRaw(context.Background(), ExecRunner{}, groveTartInfoPlist(opts), "LSUIElement"); err != nil || v != "true" {
		t.Errorf("LSUIElement = %q, %v", v, err)
	}
	if id, _ := plutilRaw(context.Background(), ExecRunner{}, groveTartInfoPlist(opts), "CFBundleIdentifier"); id != groveTartBundleID {
		t.Errorf("bundle id %q", id)
	}
	if _, err := os.Stat(filepath.Join(app, "Contents", "embedded.provisionprofile")); err == nil {
		t.Error("provisioning profile kept")
	}
	if _, err := os.Stat(app + ".new"); err == nil {
		t.Error("temp copy left behind")
	}
	// The shim runs the bundle's executable.
	if out, err := exec.Command(groveTartShimPath(opts)).CombinedOutput(); err != nil {
		t.Errorf("shim: %v\n%s", err, out)
	}
	_ = io.Discard
}
