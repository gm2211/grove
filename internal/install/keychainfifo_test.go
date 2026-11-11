//go:build darwin || linux

package install

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDeliverKeychainUnlock_OnlyIntoAFIFOWithAReader(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, KeychainUnlockFIFOName)

	if err := DeliverKeychainUnlock(fifo, "pw"); !errors.Is(err, ErrNoUnlockReader) {
		t.Fatalf("missing FIFO: %v, want ErrNoUnlockReader", err)
	}
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DeliverKeychainUnlock(regular, "pw"); err == nil || errors.Is(err, ErrNoUnlockReader) {
		t.Fatalf("a regular file took the password: %v", err)
	}
	if b, _ := os.ReadFile(regular); len(b) != 0 {
		t.Fatalf("the password was written to a regular file: %q", b)
	}
	link := filepath.Join(dir, "link")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	if err := DeliverKeychainUnlock(link, "pw"); err == nil {
		t.Fatal("followed a symlink")
	}
	if err := DeliverKeychainUnlock(fifo, "two\nlines"); err == nil {
		t.Fatal("wrote a multi-line password")
	}
	if err := DeliverKeychainUnlock(fifo, "pw"); !errors.Is(err, ErrNoUnlockReader) {
		t.Fatalf("FIFO with no reader: %v, want ErrNoUnlockReader", err)
	}

	got := make(chan string, 1)
	go func() {
		f, err := os.Open(fifo)
		if err != nil {
			got <- "open: " + err.Error()
			return
		}
		defer f.Close()
		line, _ := bufio.NewReader(f).ReadString('\n')
		got <- line
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := DeliverKeychainUnlock(fifo, `p a"s\s`)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNoUnlockReader) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if line := <-got; line != "p a\"s\\s\n" {
		t.Fatalf("reader got %q", line)
	}
}

// macOS only: the wrapper's FIFO loop unlocks a real keychain with a password the broker hands it.
func TestWorkerWrapperFIFOUnlock_RealKeychain(t *testing.T) {
	for name, password := range keychainPasswords {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			newLockedKeychain(t, home, password)
			zsh, err := exec.LookPath("zsh")
			if err != nil {
				t.Skip("no zsh")
			}
			fifo := filepath.Join(home, KeychainUnlockFIFOName)
			script := strings.ReplaceAll(workerWrapperUnlockScript(filepath.Join(home, "legacy"), fifo), loginKeychainPath, testKeychainPath)
			out, err := os.Create(filepath.Join(home, "wrapper.log"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(zsh, "-c", "set -eu\n"+script+"sleep 30\n")
			cmd.Env = append(os.Environ(), "HOME="+home)
			cmd.Stdout, cmd.Stderr = out, out
			// Its own process group, so the FIFO loop (a background subshell) dies with it.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })

			deadline := time.Now().Add(10 * time.Second)
			for {
				err := DeliverKeychainUnlock(fifo, password)
				if err == nil {
					break
				}
				if !errors.Is(err, ErrNoUnlockReader) || time.Now().After(deadline) {
					t.Fatalf("deliver: %v", err)
				}
				time.Sleep(50 * time.Millisecond)
			}
			for {
				b, _ := os.ReadFile(out.Name())
				if strings.Contains(string(b), "unlocked the login keychain from the control panel") {
					if strings.Contains(string(b), password) && password != "" {
						t.Fatalf("the wrapper's output shows the password: %s", b)
					}
					return
				}
				if strings.Contains(string(b), "could not unlock") || time.Now().After(deadline) {
					t.Fatalf("not unlocked: %s", b)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}
