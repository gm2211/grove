package scan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// scanIdentifiers is what the tests pass as GROVE_SCAN_IDENTIFIERS.
const scanIdentifiers = "build-mac-m5, Build Mac,ab"

func runScan(t *testing.T, root string) (string, int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command(bash, "scan-root.sh", root)
	cmd.Env = append(os.Environ(), "GROVE_SCAN_IDENTIFIERS="+scanIdentifiers)
	out, err := cmd.CombinedOutput()
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
		"tailnet search domain":  {"private/var/run/resolv.conf", "search tail0000aa.ts.net\n", "tailnet name"},
		"tailnet name in a log":  {"private/var/db/diagnostics/Persist/0000000000000006.tracev3", "\x00\x01\xff tail0000aa.ts.net \x00\xfe", "tailnet name"},
		"build host in a log":    {"private/var/db/diagnostics/Special/0000000000000005.tracev3", "\x00\xff Build-Mac-M5._companion-link._tcp \x00", "operator identifier"},
		"computer name anywhere": {"Library/Logs/x.log", "from Build Mac\n", "operator identifier"},
		"tailnet address":        {"private/etc/hosts", "100.101.102.103 studio\n", "tailnet address"},
		"tailscale auth key":     {"Users/admin/.zsh_history", "tailscale up --authkey tskey-auth-abc123-def45678\n", "Tailscale auth key"},
		"github token":           {"Users/admin/.netrc", "password ghp_" + strings.Repeat("a", 36) + "\n", "GitHub token"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, tc.rel, tc.content)
			out, code := runScan(t, root)
			if code != 1 {
				t.Fatalf("exit %d, want 1: %s", code, out)
			}
			if !strings.Contains(out, tc.want+": ") || !strings.Contains(out, tc.rel) {
				t.Fatalf("output doesn't name the hit: %s", out)
			}
			// Never the matched text: not a token, not the name.
			for _, secret := range []string{strings.Repeat("a", 36), "abc123-def45678", "tail0000aa", "Build-Mac-M5", "100.101.102.103"} {
				if strings.Contains(out, secret) {
					t.Fatalf("output echoes the match %q: %s", secret, out)
				}
			}
		})
	}
}

// A too-short identifier ("ab") is dropped, and program binaries may hold token-shaped strings.
func TestScanRoot_NoFalseAlarms(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/local/bin/gh", "\x00gho_brewPATHNoneOfTheAbove\x00 fabulous cab\n")
	write(t, root, "opt/homebrew/share/doc/x.txt", "example 100.100.100.100\n")
	if out, code := runScan(t, root); code != 0 {
		t.Fatalf("false alarm (%d): %s", code, out)
	}
}

// The Data volume of a booted guest is a separate mount under /System/Volumes/Data.
func TestScanRoot_ScansTheDataVolumeOfABootedGuest(t *testing.T) {
	root := t.TempDir()
	write(t, root, "System/Volumes/Data/private/var/run/resolv.conf", "search tail0000aa.ts.net\n")
	if out, code := runScan(t, root); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
}

// A FIFO must not hang the scan.
func TestScanRoot_SkipsFIFOs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "private/var/run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "private/var/run/some.fifo"), 0o600); err != nil {
		t.Skip("no mkfifo here")
	}
	done := make(chan int, 1)
	go func() { _, code := runScan(t, root); done <- code }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the scan hung on a FIFO")
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

// scrub-tart-vm.sh writes the VM's disk, so it must refuse a VM Tart doesn't report as stopped.
func TestScrubTartVM_RefusesUnlessStopped(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for name, state := range map[string]string{"running": "running", "suspended": "suspended", "unknown to tart": ""} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			write(t, home, "vms/vm1/disk.img", "not a disk")
			listed := `[]`
			if state != "" {
				listed = `[{"Name":"vm1","State":"` + state + `"}]`
			}
			fake := filepath.Join(t.TempDir(), "tart")
			if err := os.WriteFile(fake, []byte("#!/bin/sh\necho '"+listed+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bash, "scrub-tart-vm.sh", "vm1")
			cmd.Env = append(os.Environ(), "TART_HOME="+home, "TART="+fake)
			out, err := cmd.CombinedOutput()
			ee, ok := err.(*exec.ExitError)
			if !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "refusing") {
				t.Fatalf("want a refusal with exit 2, got %v: %s", err, out)
			}
		})
	}
}

func TestScrubTartVM_Usage(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for _, args := range [][]string{{}, {"no-such-vm"}} {
		cmd := exec.Command(bash, append([]string{"scrub-tart-vm.sh"}, args...)...)
		cmd.Env = append(os.Environ(), "TART_HOME="+t.TempDir())
		if err := cmd.Run(); err == nil || err.(*exec.ExitError).ExitCode() != 2 {
			t.Fatalf("args %v: want exit 2, got %v", args, err)
		}
	}
}
