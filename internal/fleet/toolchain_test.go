package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The macOS startup script creates the toolchain user, its cache directory and the root-owned
// installer, and fences the user like the job user.
func TestMacOSToolchainUserAndInstaller(t *testing.T) {
	mac := homeFenceScript(Pool{Name: "macos"})
	for _, want := range []string{
		"dscl . -create /Users/_grovetool UniqueID 7462",
		"dscl . -create /Users/_grovetool PrimaryGroupID 7462",
		"dscl . -create /Groups/_grovetool PrimaryGroupID 7462",
		"dscl . -create /Users/_grovetool UserShell /usr/bin/false",
		"dscl . -create /Users/_grovetool NFSHomeDirectory /var/empty",
		"grove_priv chown _grovetool:_grovetool /private/var/grove-toolchains /private/var/grove-toolchains/.use",
		"grove_priv chmod 0755 /private/var/grove-toolchains\n",
		"grove_priv chmod 1777 /private/var/grove-toolchains/.use",
		"grove_priv tee /usr/local/libexec/grove-toolchain >/dev/null <<'GROVE_TOOLCHAIN_INSTALLER'",
		"grove_priv chown root:wheel /usr/local/libexec/grove-toolchain",
		"grove_priv chmod 0755 /usr/local/libexec/grove-toolchain",
		// pf: the same fence as the job user, and no SSH.
		"user { _grovejob, _grovesrc, _grovetool }",
		"block return out quick proto tcp from any to any port 22 user { _grovejob, _grovesrc, _grovetool }",
	} {
		if !strings.Contains(mac, want) {
			t.Errorf("macOS fence script lacks %q", want)
		}
	}
	// The user and the installer exist before pf loads, and on shared macOS pools only.
	if i, j := strings.Index(mac, "GROVE_TOOLCHAIN_INSTALLER\ngrove_priv chown root:wheel"), strings.Index(mac, "pfctl"); i < 0 || j < 0 || i > j {
		t.Errorf("the installer must be in place before the pf anchor loads")
	}
	if strings.Contains(homeFenceScript(Pool{Name: "linux"}), "grove-toolchain") {
		t.Errorf("a Linux pool must not get the macOS toolchain installer")
	}
	// Only the jobs user may use the broker, not the toolchain user.
	if strings.Contains(macosFenceLoader(true), "user "+MacOSToolchainUser) {
		t.Errorf("the toolchain user must not get a broker exception")
	}
	if !strings.Contains(sealedMacOSFenceRules, MacOSJobUser) || strings.Contains(sealedMacOSFenceRules, MacOSToolchainUser) {
		t.Errorf("a sealed guest has no toolchain user")
	}
}

// The installer's heredoc must survive being embedded in the startup script: no template
// placeholder left behind, the terminator only at its end.
func TestMacOSToolchainInstallerEmbeds(t *testing.T) {
	if regexp.MustCompile(`@[A-Z_]+@`).MatchString(macosToolchainInstaller) {
		t.Errorf("an unreplaced placeholder (or stray @) in the installer:\n%s", macosToolchainInstaller)
	}
	if strings.Contains(macosToolchainInstaller, "GROVE_TOOLCHAIN_INSTALLER") {
		t.Errorf("the installer contains its own heredoc terminator")
	}
	if strings.Contains(macosToolchainInstaller, "'") && strings.Contains(toolchainJQValidation(), "'") {
		t.Errorf("the jq program is single-quoted in the installer and must not contain a quote")
	}
	if !strings.HasPrefix(macosToolchainInstaller, "#!/bin/bash\n") {
		t.Errorf("installer has no shebang")
	}
	if out, err := guestCmd(t, "", "-c", `exec bash -n -c "$1"`, "sh", macosToolchainInstaller).CombinedOutput(); err != nil {
		t.Errorf("installer does not parse: %v\n%s", err, out)
	}
}

// The installer re-validates with the same patterns the control plane uses; the text must carry
// them (jq-escaped), the closed component set, and the size cap.
func TestMacOSToolchainInstallerMatchesValidationPatterns(t *testing.T) {
	for name, pattern := range map[string]string{
		"rust": toolchainRustPattern, "node": toolchainNodePattern, "python": toolchainPythonPattern,
	} {
		if !strings.Contains(macosToolchainInstaller, strings.ReplaceAll(pattern, `\`, `\\`)) {
			t.Errorf("installer lacks the %s version pattern %q", name, pattern)
		}
	}
	// The specified patterns, with \d: ours differ only by spelling it [0-9].
	for pattern, spec := range map[string]string{
		toolchainRustPattern: `^1\.\d+\.\d+$`, toolchainNodePattern: `^\d+\.\d+\.\d+$`, toolchainPythonPattern: `^3\.\d+\.\d+$`,
	} {
		if strings.ReplaceAll(spec, `\d`, `[0-9]`) != pattern {
			t.Errorf("pattern %q drifted from %q", pattern, spec)
		}
	}
	for _, c := range []string{"clippy", "rustfmt", "rust-src", "llvm-tools"} {
		if !strings.Contains(macosToolchainInstaller, `. == "`+c+`"`) {
			t.Errorf("installer lacks component %s", c)
		}
	}
	if len(toolchainComponents) != 4 {
		t.Errorf("components = %v", toolchainComponents)
	}
	if !strings.Contains(macosToolchainInstaller, "-le 1024") || !strings.Contains(macosToolchainInstaller, "grove-toolchain/1\\n%s") {
		t.Errorf("installer lacks the size cap or the hash domain prefix")
	}
	if ToolchainHashPrefix != "grove-toolchain/1\n" {
		t.Errorf("hash prefix = %q", ToolchainHashPrefix)
	}
	// Spot-check the patterns themselves.
	for _, c := range []struct {
		re   string
		in   string
		want bool
	}{
		{toolchainRustPattern, "1.99.1", true}, {toolchainRustPattern, "2.0.0", false}, {toolchainRustPattern, "1.99", false},
		{toolchainNodePattern, "22.11.0", true}, {toolchainNodePattern, "22.x.0", false},
		{toolchainPythonPattern, "3.12.3", true}, {toolchainPythonPattern, "4.0.0", false},
	} {
		if regexp.MustCompile(c.re).MatchString(c.in) != c.want {
			t.Errorf("%s on %q != %v", c.re, c.in, c.want)
		}
	}
}

// toolchainHash is the control plane's hash of a canonical spec.
func toolchainHash(spec string) string {
	sum := sha256.Sum256([]byte(ToolchainHashPrefix + spec))
	return hex.EncodeToString(sum[:])
}

// installerHarness runs the real installer text under bash with its two fixed paths pointed at a
// temp directory and a fake mise that records what it was asked to do.
type installerHarness struct {
	t         *testing.T
	root      string
	installer string
	miseLog   string
}

func newInstallerHarness(t *testing.T) *installerHarness {
	t.Helper()
	for _, tool := range []string{"jq", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	base := t.TempDir()
	h := &installerHarness{t: t, root: filepath.Join(base, "toolchains"), miseLog: filepath.Join(base, "mise.log")}
	if err := os.MkdirAll(filepath.Join(h.root, ".use"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeMise := filepath.Join(base, "mise")
	// mise runs under env -i, so the fake finds its flags and log next to itself, not in its env.
	body := "#!/bin/bash\nFAKE_BASE=" + base + "\n" + `
echo "mise $* | cwd=$PWD HOME=$HOME DATA=$MISE_DATA_DIR CACHE=$MISE_CACHE_DIR GLOBAL=$MISE_GLOBAL_CONFIG_FILE COMPILE=$MISE_PYTHON_COMPILE RUSTUP_HOME=$RUSTUP_HOME CARGO_HOME=$CARGO_HOME YES=$MISE_YES TOKEN=${GROVE_CLONE_TOKEN:-}" >> "$FAKE_BASE/mise.log"
case "$1" in
  --version) echo "2026.10.24 macos-arm64 (2026-10-30)" ;;
  install)
    cat mise.toml >> "$FAKE_BASE/mise.log"
    [ ! -f "$FAKE_BASE/sleep" ] || sleep 30
    [ ! -f "$FAKE_BASE/fail" ] || exit 1
    mkdir -p "$MISE_DATA_DIR/installs/node/bin" ;;
  exec) ;;
  bin-paths) if [ -f "$FAKE_BASE/badpath" ]; then echo /usr/bin; else echo "$MISE_DATA_DIR/installs/node/bin"; fi ;;
esac
`
	if err := os.WriteFile(fakeMise, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	script := macosToolchainInstaller
	for old, repl := range map[string]string{
		"grove_root=" + MacOSToolchainRoot + "\n": "grove_root=" + h.root + "\n",
		"mise_bin=" + MacOSMise + "\n":            "mise_bin=" + fakeMise + "\n",
	} {
		if !strings.Contains(script, old) {
			t.Fatalf("installer has no %q line", old)
		}
		script = strings.Replace(script, old, repl, 1)
	}
	h.installer = filepath.Join(base, "grove-toolchain")
	if err := os.WriteFile(h.installer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *installerHarness) run(spec, hash string, env ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	// bash, not /bin/sh: the installer's shebang. guestCmd keeps unfaked host-mutating binaries out.
	cmd := guestCmd(h.t, "", "-c", `exec bash "$@"`, "sh", h.installer, hash)
	cmd.Stdin = strings.NewReader(spec)
	cmd.Env = append(cmd.Env, "GROVE_CLONE_TOKEN=", "TMPDIR="+h.t.TempDir())
	cmd.Env = append(cmd.Env, env...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatalf("run installer: %v", err)
	}
	return out.String(), errb.String(), code
}

// flag turns on one of the fake mise's failure modes: fail, badpath, sleep.
func (h *installerHarness) flag(name string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.miseLog), name), nil, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *installerHarness) mise() string {
	b, _ := os.ReadFile(h.miseLog)
	return string(b)
}

const validToolchainSpec = `{"node":{"version":"22.11.0"},"python":{"version":"3.12.3"},"rust":{"components":["clippy","rustfmt"],"version":"1.99.1"}}`

func TestToolchainInstallerInstallsAndCaches(t *testing.T) {
	h := newInstallerHarness(t)
	hash := toolchainHash(validToolchainSpec)
	out, errOut, code := h.run(validToolchainSpec, hash)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errOut, h.mise())
	}
	dir := filepath.Join(h.root, "mise-2026.10.24-"+hash)
	if strings.TrimSpace(out) != dir {
		t.Fatalf("printed %q, want %q", out, dir)
	}
	toml, err := os.ReadFile(filepath.Join(dir, "mise.toml"))
	if err != nil {
		t.Fatal(err)
	}
	wantTOML := "[tools]\nrust = { version = \"1.99.1\", profile = \"minimal\", components = \"clippy,rustfmt\" }\nnode = \"22.11.0\"\npython = \"3.12.3\"\n"
	if string(toml) != wantTOML {
		t.Errorf("mise.toml =\n%s\nwant\n%s", toml, wantTOML)
	}
	for _, f := range []string{".complete", ".lastuse"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	paths, _ := os.ReadFile(filepath.Join(dir, "paths"))
	if !strings.HasPrefix(string(paths), dir+"/") {
		t.Errorf("paths = %q", paths)
	}
	log := h.mise()
	for _, want := range []string{
		"mise install | cwd=" + dir, "HOME=/var/empty", "DATA=" + dir + "/data", "CACHE=" + dir + "/cache",
		"GLOBAL=/dev/null", "COMPILE=0", "RUSTUP_HOME=" + dir + "/rustup", "CARGO_HOME=" + dir + "/cargo", "YES=1",
		"mise exec -- rustup default 1.99.1", "TOKEN=\n",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("mise log lacks %q:\n%s", want, log)
		}
	}
	// Second call: a hit, no new install.
	before := strings.Count(h.mise(), "mise install")
	out2, _, code := h.run(validToolchainSpec, hash)
	if code != 0 || out2 != out || strings.Count(h.mise(), "mise install") != before {
		t.Errorf("cache hit reinstalled or printed %q (code %d)", out2, code)
	}
}

func TestToolchainInstallerNoRustSkipsRustup(t *testing.T) {
	h := newInstallerHarness(t)
	spec := `{"node":{"version":"22.11.0"}}`
	if _, errOut, code := h.run(spec, toolchainHash(spec)); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(h.mise(), "rustup default") || strings.Contains(h.mise(), "rust =") {
		t.Errorf("a node-only spec touched rust:\n%s", h.mise())
	}
}

func TestToolchainInstallerRejects(t *testing.T) {
	long := `{"node":{"version":"22.11.0"},"python":{"version":"3.12.3"},"rust":{"version":"1.99.1","components":[` +
		strings.TrimSuffix(strings.Repeat(`"clippy",`, 150), ",") + `]}}`
	cases := map[string]string{
		"unknown tool":        `{"go":{"version":"1.23.0"}}`,
		"empty object":        `{}`,
		"not an object":       `["rust"]`,
		"extra key":           `{"node":{"version":"22.11.0","registry":"http://evil"}}`,
		"extra top key":       `{"node":{"version":"22.11.0"},"env":{}}`,
		"range":               `{"node":{"version":"22"}}`,
		"x version":           `{"node":{"version":"22.x.y"}}`,
		"rust 2":              `{"rust":{"version":"2.0.0"}}`,
		"rust channel":        `{"rust":{"version":"nightly"}}`,
		"python 2":            `{"python":{"version":"2.7.18"}}`,
		"python minor only":   `{"python":{"version":"3.12"}}`,
		"trailing newline":    "{\"node\":{\"version\":\"22.11.0\\n\"}}",
		"version not string":  `{"node":{"version":22}}`,
		"missing version":     `{"node":{}}`,
		"bad component":       `{"rust":{"version":"1.99.1","components":["miri"]}}`,
		"component injection": `{"rust":{"version":"1.99.1","components":["clippy\"\nx=\"1"]}}`,
		"components not list": `{"rust":{"version":"1.99.1","components":"clippy"}}`,
		"over 1 KiB":          long,
		"unicode digits":      `{"node":{"version":"２２.11.0"}}`,
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			h := newInstallerHarness(t)
			if name == "over 1 KiB" && len(spec) <= ToolchainMaxBytes {
				t.Fatalf("test spec is only %d bytes", len(spec))
			}
			_, errOut, code := h.run(spec, toolchainHash(spec))
			if code == 0 {
				t.Fatalf("accepted %s", spec)
			}
			if strings.Contains(h.mise(), "mise install") {
				t.Errorf("mise ran for a rejected spec")
			}
			if entries, _ := os.ReadDir(h.root); len(entries) != 1 { // .use only
				t.Errorf("a rejected spec left %v (stderr %q)", entries, errOut)
			}
		})
	}
	t.Run("hash mismatch", func(t *testing.T) {
		h := newInstallerHarness(t)
		for _, hash := range []string{toolchainHash(`{"node":{"version":"22.11.1"}}`), "", "XYZ", strings.Repeat("0", 63)} {
			if _, _, code := h.run(validToolchainSpec, hash); code == 0 {
				t.Errorf("accepted hash %q", hash)
			}
		}
		// Whitespace changes the bytes, so the hash.
		if _, _, code := h.run(strings.Replace(validToolchainSpec, `":`, `": `, 1), toolchainHash(validToolchainSpec)); code == 0 {
			t.Errorf("accepted a respaced spec under the canonical hash")
		}
		if strings.Contains(h.mise(), "mise install") {
			t.Errorf("mise ran")
		}
	})
}

func TestToolchainInstallerFailureLeavesNoEntry(t *testing.T) {
	spec := `{"node":{"version":"22.11.0"}}`
	for name, flag := range map[string]string{"install fails": "fail", "path outside it": "badpath"} {
		t.Run(name, func(t *testing.T) {
			h := newInstallerHarness(t)
			h.flag(flag)
			if _, _, code := h.run(spec, toolchainHash(spec)); code == 0 {
				t.Fatalf("succeeded")
			}
			_ = os.Remove(filepath.Join(filepath.Dir(h.miseLog), flag))
			entries, _ := os.ReadDir(h.root)
			for _, e := range entries {
				if e.Name() != ".use" {
					t.Errorf("left %s behind", e.Name())
				}
			}
			// A retry works once the cause is gone.
			if _, errOut, code := h.run(spec, toolchainHash(spec)); code != 0 {
				t.Errorf("retry: %d %s", code, errOut)
			}
		})
	}
}

func TestToolchainInstallerLocks(t *testing.T) {
	spec := `{"node":{"version":"22.11.0"}}`
	hash := toolchainHash(spec)
	t.Run("stale lock is cleared", func(t *testing.T) {
		h := newInstallerHarness(t)
		lock := filepath.Join(h.root, ".lock-mise-2026.10.24-"+hash)
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
		if _, errOut, code := h.run(spec, hash); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if _, err := os.Stat(lock); err == nil {
			t.Errorf("lock left behind")
		}
	})
	t.Run("a live lock makes it wait for the other install", func(t *testing.T) {
		h := newInstallerHarness(t)
		dir := filepath.Join(h.root, "mise-2026.10.24-"+hash)
		lock := filepath.Join(h.root, ".lock-mise-2026.10.24-"+hash)
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(1 * time.Second)
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(filepath.Join(dir, ".complete"), nil, 0o644)
		}()
		out, errOut, code := h.run(spec, hash)
		if code != 0 || strings.TrimSpace(out) != dir {
			t.Fatalf("exit %d out %q: %s", code, out, errOut)
		}
		if strings.Contains(h.mise(), "mise install") {
			t.Errorf("installed although another installer held the lock")
		}
	})
}

func TestToolchainInstallerEvictsIdleEntries(t *testing.T) {
	h := newInstallerHarness(t)
	old := time.Now().Add(-48 * time.Hour)
	mk := func(name string, age time.Time, marker bool) string {
		dir := filepath.Join(h.root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{".complete", ".lastuse"} {
			p := filepath.Join(dir, f)
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			_ = os.Chtimes(p, age, age)
		}
		if marker {
			m := filepath.Join(h.root, ".use", name+".alloc1")
			_ = os.WriteFile(m, nil, 0o644)
		}
		return dir
	}
	h64 := func(c string) string { return strings.Repeat(c, 64) }
	idle1 := mk("mise-1.0.0-"+h64("1"), old.Add(-3*time.Hour), false)
	idle2 := mk("mise-1.0.0-"+h64("2"), old.Add(-2*time.Hour), false)
	busy := mk("mise-1.0.0-"+h64("3"), old.Add(-4*time.Hour), true) // old stamp, but a job is using it
	recent := mk("mise-1.0.0-"+h64("4"), time.Now().Add(-time.Hour), false)
	spec := `{"node":{"version":"22.11.0"}}`
	if _, errOut, code := h.run(spec, toolchainHash(spec)); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// Five entries with the new one; at most three kept; the oldest idle go first.
	for dir, wantGone := range map[string]bool{idle1: true, idle2: true, busy: false, recent: false} {
		_, err := os.Stat(dir)
		if gone := err != nil; gone != wantGone {
			t.Errorf("%s: gone = %v, want %v", filepath.Base(dir), gone, wantGone)
		}
	}
	if _, err := os.Stat(filepath.Join(h.root, ".use", filepath.Base(idle1)+".alloc1")); err == nil {
		t.Errorf("an evicted entry's marker was kept")
	}
}

func TestToolchainInstallerTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	script := strings.Replace(macosToolchainInstaller, "sleep 1200", "sleep 1", 1)
	if script == macosToolchainInstaller {
		t.Fatalf("watchdog sleep not found")
	}
	h := newInstallerHarness(t)
	script = strings.Replace(script, "grove_root="+MacOSToolchainRoot+"\n", "grove_root="+h.root+"\n", 1)
	script = strings.Replace(script, "mise_bin="+MacOSMise+"\n", "mise_bin="+filepath.Join(filepath.Dir(h.installer), "mise")+"\n", 1)
	if err := os.WriteFile(h.installer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := `{"node":{"version":"22.11.0"}}`
	start := time.Now()
	h.flag("sleep")
	_, errOut, code := h.run(spec, toolchainHash(spec))
	if code == 0 || !strings.Contains(errOut, "took longer") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("the watchdog did not stop the install in time: %v", time.Since(start))
	}
}
