package scan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runScan(t *testing.T, root string) (string, int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	out, err := exec.Command(bash, "scan-root.sh", root).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanRoot_CleanGuestPasses(t *testing.T) {
	root := t.TempDir()
	write(t, root, "private/var/run/resolv.conf", "nameserver 192.168.64.1\n")
	write(t, root, "private/etc/hosts", "127.0.0.1 localhost\n")
	// Tart's bridge and ordinary public addresses are not host-derived identifiers.
	write(t, root, "usr/local/etc/nomad.d/client.hcl", "bind_addr = \"127.0.0.1\"\n# 100.20.30.40 8.8.8.8\n")
	if out, code := runScan(t, root); code != 0 {
		t.Fatalf("clean root failed (%d): %s", code, out)
	}
}

// The leak that reached a pushed image: the build Mac's tailnet search domain.
func TestScanRoot_FindsWhatTheBuildMacLeaks(t *testing.T) {
	for name, tc := range map[string]struct{ rel, content, want string }{
		"tailnet search domain": {"private/var/run/resolv.conf", "search tail0000aa.ts.net\n", "tail0000aa.ts.net"},
		"tailnet address":       {"private/etc/hosts", "100.101.102.103 studio\n", "100.101.102.103"},
		"tailscale auth key":    {"Users/admin/.zsh_history", "tailscale up --authkey tskey-auth-abc123-def456\n", "tskey-…"},
		"github token":          {"Users/admin/.netrc", "password ghp_" + strings.Repeat("a", 36) + "\n", "ghp_…"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, tc.rel, tc.content)
			out, code := runScan(t, root)
			if code != 1 {
				t.Fatalf("exit %d, want 1: %s", code, out)
			}
			if !strings.Contains(out, tc.want) || !strings.Contains(out, tc.rel) {
				t.Fatalf("output doesn't name the hit: %s", out)
			}
			if strings.Contains(out, strings.Repeat("a", 36)) || strings.Contains(out, "abc123-def456") {
				t.Fatalf("output echoes a secret: %s", out)
			}
		})
	}
}

// Apple's own cached lists hold arbitrary names; they are not this build's state.
func TestScanRoot_IgnoresCaches(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Users/admin/Library/Caches/com.apple.x/list.txt", "example.ts.net\n")
	if out, code := runScan(t, root); code != 0 {
		t.Fatalf("cache hit failed the scan (%d): %s", code, out)
	}
}

func TestScanRoot_Usage(t *testing.T) {
	if _, code := runScan(t, filepath.Join(t.TempDir(), "missing")); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
