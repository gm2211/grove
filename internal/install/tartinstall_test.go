package install

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTartBundle lays down the files of a tart.app at app (so symlinks into it resolve) and
// returns its resolved path, which is how the code names it.
func makeTartBundle(t *testing.T, app string) string {
	t.Helper()
	exe := filepath.Join(app, "Contents", "MacOS", "tart")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return resolvedPath(app)
}

func linkTo(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// tartOnPath makes `tart` resolve to path in opts, as exec.LookPath would.
func tartOnPath(opts Options, path string) Options {
	opts.LookPath = func(name string) (string, error) {
		if name == "tart" {
			return path, nil
		}
		return "", errors.New("not found (fake)")
	}
	return opts
}

func TestTartHeadlessStep_CopiesAGroveManagedTartWithoutHomebrew(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	managed := makeTartBundle(t, groveManagedTartApp(opts))
	r := NewFakeRunner()
	r.Script("brew --prefix tart", FakeResult{Err: errors.New("no formula")})
	scriptTartBundle(r, managed, "2.41.1")
	if err := applyHeadlessTart(t, r, opts); err != nil {
		t.Fatal(err)
	}
	if !r.CalledWith("/usr/bin/ditto " + managed + " " + groveTartAppDir(opts) + ".new") {
		t.Errorf("did not copy the managed bundle; calls: %v", r.Calls)
	}
	prov, _ := os.ReadFile(groveTartProvenancePath(opts))
	if !strings.Contains(string(prov), "source="+managed+"\n") || !strings.Contains(string(prov), "version=2.41.1") {
		t.Errorf("provenance %q", prov)
	}
}

func TestTartHeadlessStep_PrefersHomebrewsTart(t *testing.T) {
	home := t.TempDir()
	opts := testWorkerOptions(home)
	managed := makeTartBundle(t, groveManagedTartApp(opts))
	brewApp := makeTartBundle(t, filepath.Join(home, "brew", "opt", "tart", "libexec", "tart.app"))
	r := NewFakeRunner()
	r.Script("brew --prefix tart", FakeResult{Stdout: filepath.Join(home, "brew", "opt", "tart") + "\n"})
	scriptTartBundle(r, brewApp, "2.41.1")
	scriptTartBundle(r, managed, "2.40.0")
	if err := applyHeadlessTart(t, r, opts); err != nil {
		t.Fatal(err)
	}
	if !r.CalledWith("/usr/bin/ditto " + brewApp + " " + groveTartAppDir(opts) + ".new") {
		t.Errorf("did not copy Homebrew's tart; calls: %v", r.Calls)
	}
}

func TestTartHeadlessStep_RefusesANonGenuineNonHomebrewSource(t *testing.T) {
	for name, mutate := range map[string]func(*FakeRunner, string){
		"bundle id": func(r *FakeRunner, app string) {
			r.Script("/usr/bin/plutil -extract CFBundleIdentifier raw -o - "+app+"/Contents/Info.plist", FakeResult{Stdout: "com.evil.tart\n"})
		},
		"team": func(r *FakeRunner, app string) {
			r.Script("/usr/bin/codesign -dv "+app, FakeResult{Stderr: "CDHash=aa\nTeamIdentifier=EVIL000000\n"})
		},
		"signature": func(r *FakeRunner, app string) {
			r.Script("/usr/bin/codesign --verify --strict "+app, FakeResult{Err: errors.New("invalid")})
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testWorkerOptions(t.TempDir())
			managed := makeTartBundle(t, groveManagedTartApp(opts))
			r := NewFakeRunner()
			r.Script("brew --prefix tart", FakeResult{Err: errors.New("no formula")})
			scriptTartBundle(r, managed, "2.41.1")
			mutate(r, managed)
			err := applyHeadlessTart(t, r, opts)
			if err == nil || !strings.Contains(err.Error(), "refusing "+managed) {
				t.Fatalf("err = %v, want a refusal of %s", err, managed)
			}
			for _, c := range r.Calls {
				if c.Name == "/usr/bin/ditto" {
					t.Error("copied a bundle it refused")
				}
			}
		})
	}
}

func TestTartHeadlessStep_CheckFollowsTheNonHomebrewSource(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	managed := makeTartBundle(t, groveManagedTartApp(opts))
	laysDown := func(version string) *FakeRunner {
		r := NewFakeRunner()
		laysDownCopy(t, r, opts, version) // writes the copy's provenance and scripts brew's tart
		r.Script("brew --prefix tart", FakeResult{Err: errors.New("no formula")})
		scriptTartBundle(r, managed, version)
		src := tartSource{App: managed, Version: "2.41.1", CDHash: "fedc1ee5"}
		if err := os.WriteFile(groveTartProvenancePath(opts), []byte(src.provenance()), 0o644); err != nil {
			t.Fatal(err)
		}
		return r
	}
	step := tartHeadlessStep(laysDown("2.41.1"), opts)
	if ok, err := step.Check(context.Background()); err != nil || !ok {
		t.Fatalf("Check = %v, %v; want satisfied", ok, err)
	}
	step = tartHeadlessStep(laysDown("2.42.0"), opts)
	if ok, _ := step.Check(context.Background()); ok {
		t.Error("a copy of an older Tart still satisfies the step")
	}
	if c := checkHeadlessTart(context.Background(), laysDown("2.42.0"), opts); c.OK || !strings.Contains(c.Detail, "2.42.0") {
		t.Errorf("doctor: %+v", c)
	}
}

func TestWorkerPlan_BrewTrustDoesNotBlockWhenTartIsNotHomebrews(t *testing.T) {
	home := t.TempDir()
	app := makeTartBundle(t, filepath.Join(home, ".local", "lib", "grove", "tart.app"))
	link := filepath.Join(home, ".local", "bin", "tart")
	linkTo(t, link, filepath.Join(app, "Contents", "MacOS", "tart"))
	opts := tartOnPath(testWorkerOptions(home), link)

	r := NewFakeRunner()
	scriptTailscaleUp(r)
	// The failure on the Studio: Homebrew is another user's and openai/tools is not tapped.
	scriptTapInfo(r, "openai/tools", false, false)
	r.Script("brew --prefix", FakeResult{Stdout: "/fake-brew\n"})
	r.Script("stat -f %Su /fake-brew", FakeResult{Stdout: "someone-else-zzz\n"})
	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"brew-trust:openai/tools", "tart"} {
		if ok, err := stepNamed(t, steps, name).Check(context.Background()); err != nil || !ok {
			t.Errorf("%s Check = %v, %v; want satisfied", name, ok, err)
		}
	}
	for _, c := range r.Calls {
		if c.Name == "brew" && len(c.Args) > 0 && (c.Args[0] == "tap" || c.Args[0] == "trust" || c.Args[0] == "install") {
			t.Errorf("ran %s", c)
		}
	}
}

func TestWorkerPlan_BrewTrustStillRunsWhenHomebrewWillInstallTart(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	r := NewFakeRunner()
	scriptTapInfo(r, "openai/tools", false, false)
	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := stepNamed(t, steps, "brew-trust:openai/tools").Check(context.Background()); ok {
		t.Error("an untrusted tap satisfied the trust step although Homebrew will install Tart")
	}
}

// tarFakeRunner extracts a fake tart.app when asked to untar, since FakeRunner has no side effects.
type tarFakeRunner struct {
	*FakeRunner
	t *testing.T
}

func (f tarFakeRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	if name == "/usr/bin/tar" {
		makeTartBundle(f.t, filepath.Join(args[3], "tart.app"))
	}
	return f.FakeRunner.Run(ctx, name, args...)
}

func scriptForeignBrew(r *FakeRunner) {
	r.Script("brew --prefix", FakeResult{Stdout: "/fake-brew\n"})
	r.Script("stat -f %Su /fake-brew", FakeResult{Stdout: "someone-else-zzz\n"})
}

func scriptTartTarball(r *FakeRunner, opts Options) pinnedDownload {
	d, err := tartDownload(opts)
	if err != nil {
		panic(err)
	}
	name, args := sha256Command(opts, filepath.Join(downloadDir(opts), d.File))
	r.Script(Call{Name: name, Args: args}.String(), FakeResult{Stdout: d.SHA256 + "  " + d.File + "\n"})
	return d
}

func TestTartStep_InstallsThePinnedTarballWhenHomebrewIsForeign(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	fr := NewFakeRunner()
	scriptForeignBrew(fr)
	d := scriptTartTarball(fr, opts)
	dest := groveManagedTartApp(opts)
	scriptTartBundle(fr, dest+".extract/tart.app", TartVersion)

	step := tartStep(tarFakeRunner{fr, t}, opts, io.Discard)
	if ok, _ := step.Check(context.Background()); ok {
		t.Fatal("Tart reported installed before the install")
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(downloadDir(opts), d.File)
	for _, want := range []string{
		"curl -fsSL --proto =https --tlsv1.2 -o " + staged + " " + d.URL,
		"/usr/bin/tar -xzf " + staged + " -C " + dest + ".extract tart.app",
		"/usr/bin/codesign --verify --strict " + dest + ".extract/tart.app",
	} {
		if !fr.CalledWith(want) {
			t.Errorf("missing call %q", want)
		}
	}
	for _, c := range fr.Calls {
		if c.Name == "brew" && len(c.Args) > 0 && c.Args[0] == "install" {
			t.Errorf("ran %s", c)
		}
	}
	if !isRegularFile(groveManagedTartExecutable(opts)) {
		t.Error("bundle not installed")
	}
	if got, err := os.Readlink(groveManagedTartLink(opts)); err != nil || got != groveManagedTartExecutable(opts) {
		t.Errorf("link = %q, %v", got, err)
	}
	if _, err := os.Stat(dest + ".extract"); err == nil {
		t.Error("extraction directory left behind")
	}
	// Idempotent: a second run is satisfied, and re-running Apply replaces its own link.
	if ok, _ := step.Check(context.Background()); !ok {
		t.Error("Tart not reported installed afterwards")
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
}

func TestTartStep_RefusesATarballThatIsNotGenuine(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	fr := NewFakeRunner()
	scriptForeignBrew(fr)
	scriptTartTarball(fr, opts)
	dest := groveManagedTartApp(opts)
	fresh := dest + ".extract/tart.app"
	scriptTartBundle(fr, fresh, TartVersion)
	fr.Script("/usr/bin/codesign -dv "+fresh, FakeResult{Stderr: "CDHash=aa\nTeamIdentifier=EVIL000000\n"})

	err := tartStep(tarFakeRunner{fr, t}, opts, io.Discard).Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if _, err := os.Lstat(dest); err == nil {
		t.Error("installed a bundle it refused")
	}
	if _, err := os.Lstat(groveManagedTartLink(opts)); err == nil {
		t.Error("linked a bundle it refused")
	}
}

func TestTartStep_FailsClosedOnADigestMismatch(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	fr := NewFakeRunner()
	scriptForeignBrew(fr)
	d, _ := tartDownload(opts)
	name, args := sha256Command(opts, filepath.Join(downloadDir(opts), d.File))
	fr.Script(Call{Name: name, Args: args}.String(), FakeResult{Stdout: strings.Repeat("0", 64) + "  x\n"})
	if err := tartStep(tarFakeRunner{fr, t}, opts, io.Discard).Apply(context.Background()); err == nil {
		t.Fatal("expected a checksum failure")
	}
	for _, c := range fr.Calls {
		if c.Name == "/usr/bin/tar" {
			t.Error("extracted an unverified download")
		}
	}
}

func TestTartStep_DoesNotClobberAForeignTartLink(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	if err := os.MkdirAll(orchardBinDir(opts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(groveManagedTartLink(opts), []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	fr := NewFakeRunner()
	scriptForeignBrew(fr)
	scriptTartTarball(fr, opts)
	scriptTartBundle(fr, groveManagedTartApp(opts)+".extract/tart.app", TartVersion)
	if err := tartStep(tarFakeRunner{fr, t}, opts, io.Discard).Apply(context.Background()); err == nil {
		t.Fatal("expected a refusal to replace a file grove did not make")
	}
	if b, _ := os.ReadFile(groveManagedTartLink(opts)); string(b) != "mine" {
		t.Errorf("the existing file was replaced: %q", b)
	}
}

func TestTartStep_UsesHomebrewWhenItIsTheUsersOwn(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	fr := NewFakeRunner()
	if err := tartStep(fr, opts, io.Discard).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fr.CalledWith("brew install openai/tools/tart") {
		t.Error("expected the Homebrew install")
	}
	for _, c := range fr.Calls {
		if c.Name == "curl" {
			t.Errorf("downloaded %s although Homebrew is usable", c)
		}
	}
}

func TestWorkerPlan_BrewTrustIsSatisfiedWhenTartWillComeFromTheTarball(t *testing.T) {
	opts := testWorkerOptions(t.TempDir())
	r := NewFakeRunner()
	scriptTailscaleUp(r)
	scriptTapInfo(r, "openai/tools", false, false)
	scriptForeignBrew(r)
	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := stepNamed(t, steps, "brew-trust:openai/tools").Check(context.Background()); err != nil || !ok {
		t.Errorf("trust Check = %v, %v; want satisfied", ok, err)
	}
	if ok, _ := stepNamed(t, steps, "tart").Check(context.Background()); ok {
		t.Error("tart step satisfied with no Tart")
	}
}
