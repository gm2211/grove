package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		candidate, current string
		want               bool
	}{
		{"v0.1.44", "0.1.43", true},
		{"0.2.0", "v0.1.99", true},
		{"v0.1.43", "0.1.43", false},
		{"v0.1.42", "0.1.43", false},
		{"v0.1.44", "dev", false},
		{"v0.1.44", "0.1.43-next", false},
		{"latest", "0.1.43", false},
	} {
		if got := NewerVersion(c.candidate, c.current); got != c.want {
			t.Errorf("NewerVersion(%q, %q) = %t, want %t", c.candidate, c.current, got, c.want)
		}
	}
}

func TestSelfUpdatable_RefusesHomebrew(t *testing.T) {
	if err := SelfUpdatable("/opt/homebrew/Cellar/grove/0.1.43/bin/grove"); err == nil || !strings.Contains(err.Error(), "brew upgrade") {
		t.Fatalf("err = %v", err)
	}
}

// updateRunner plays curl, shasum, tar and the grove binaries of a self-update.
type updateRunner struct {
	Runner   // the methods a self-update never calls
	t        *testing.T
	sum      string // what the hash tool prints for the archive
	newVer   string // what the downloaded grove reports
	calls    []string
	exe      string
	replaced bool
}

func (u *updateRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	u.calls = append(u.calls, name+" "+strings.Join(args, " "))
	switch {
	case name == "curl" && strings.HasSuffix(args[len(args)-1], "/checksums.txt"):
		return strings.Repeat("a", 64) + "  grove_darwin_arm64.tar.gz\n" + strings.Repeat("b", 64) + "  orchard_darwin_arm64.tar.gz\n", "", nil
	case name == "curl":
		return "", "", nil
	case name == "shasum" || name == "sha256sum":
		return u.sum + "  x\n", "", nil
	case name == "tar":
		dir := args[3]
		return "", "", os.WriteFile(filepath.Join(dir, "grove"), []byte("new"), 0o755)
	case name == "mv":
		u.replaced = true
		return "", "", os.Rename(args[1], args[2])
	case name == u.exe:
		return "grove version 0.1.43\n", "", nil
	case strings.HasSuffix(name, "/grove"):
		return "grove version " + u.newVer + "\n", "", nil
	}
	return "", "", nil
}

func newUpdateRunner(t *testing.T) (*updateRunner, Options) {
	home := t.TempDir()
	exe := filepath.Join(home, "bin", "grove")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &updateRunner{t: t, sum: strings.Repeat("a", 64), newVer: "0.1.44", exe: exe},
		Options{Home: home, GOOS: "darwin", GOARCH: "arm64", GroveVersion: "0.1.43"}
}

func TestSelfUpdate_InstallsAVerifiedRelease(t *testing.T) {
	r, opts := newUpdateRunner(t)
	if err := SelfUpdate(context.Background(), r, opts, r.exe, "v0.1.44", io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(r.exe); string(got) != "new" {
		t.Fatalf("binary not replaced: %q", got)
	}
	joined := strings.Join(r.calls, "\n")
	for _, want := range []string{
		"https://github.com/gm2211/grove/releases/download/v0.1.44/checksums.txt",
		"https://github.com/gm2211/grove/releases/download/v0.1.44/grove_darwin_arm64.tar.gz",
		"cp -pf " + r.exe + " " + r.exe + ".0.1.43.bak",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
}

func TestSelfUpdate_RefusesABadChecksum(t *testing.T) {
	r, opts := newUpdateRunner(t)
	r.sum = strings.Repeat("c", 64)
	err := SelfUpdate(context.Background(), r, opts, r.exe, "v0.1.44", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || r.replaced {
		t.Fatalf("err = %v, replaced = %t", err, r.replaced)
	}
	if got, _ := os.ReadFile(r.exe); string(got) != "old" {
		t.Fatalf("binary touched: %q", got)
	}
}

func TestSelfUpdate_RefusesABinaryOfAnotherVersion(t *testing.T) {
	r, opts := newUpdateRunner(t)
	r.newVer = "0.1.40"
	err := SelfUpdate(context.Background(), r, opts, r.exe, "v0.1.44", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "reports version") || r.replaced {
		t.Fatalf("err = %v, replaced = %t", err, r.replaced)
	}
}

func TestUnattendedPath_PutsHomebrewFirstAndKeepsTheRest(t *testing.T) {
	got := UnattendedPath("/usr/bin:/bin:/opt/homebrew/bin", "/Users/argos")
	want := "/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/Users/argos/.local/bin:/usr/bin:/bin"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
