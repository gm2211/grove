package fleet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildStartupScript_HostXcodeSetupIsFailClosedAndVerified(t *testing.T) {
	const expectedBuild = "27A266a"
	pool := Pool{
		Name: "macos",
		HostXcode: &HostXcodeConfig{
			Path:         "/Applications/Xcode_27.app",
			BuildVersion: expectedBuild,
		},
	}
	full := BuildStartupScript(pool, "mac1", "macos-mac1-0")
	script := buildHostXcodeStartupScript(*pool.HostXcode)
	if at := strings.Index(full, script); at < 0 || at < strings.Index(full, "network: isolated (sealed, macOS)") {
		t.Fatalf("the host Xcode setup is missing from the sealed macOS startup script, or runs before sealing:\n%s", full)
	}
	if strings.Contains(BuildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0"), "xcode-select") {
		t.Fatal("a Linux pool got host Xcode setup")
	}
	for _, want := range []string{
		"/Volumes/My Shared Files/grove-xcode.app",
		"ProductBuildVersion",
		"LSMinimumSystemVersion",
		"xcode-select --switch",
		"xcodebuild -license accept",
		"xcodebuild -runFirstLaunch",
		"xcodebuild -version",
		"xcrun --sdk macosx --show-sdk-path",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("host Xcode startup script missing %q", want)
		}
	}
	if strings.Contains(full, pool.HostXcode.Path) {
		t.Errorf("host-only Xcode path leaked into the guest startup script")
	}
	ordered := []string{
		"ProductBuildVersion",
		"xcode-select --switch",
		"xcodebuild -license accept",
		"xcodebuild -runFirstLaunch",
		"xcodebuild -version",
		"xcrun --sdk macosx --show-sdk-path",
	}
	previous := -1
	for _, item := range ordered {
		at := strings.Index(script, item)
		if at <= previous {
			t.Errorf("%q is missing or out of order in host Xcode startup script", item)
		}
		previous = at
	}

	for _, tt := range []struct {
		name                string
		build               string
		guestOS             string
		minOS               string
		sdkExists           bool
		sudoFails           bool
		expectXcodeCommands bool
		wantOK              bool
	}{
		{name: "matching image", build: expectedBuild, guestOS: "26.6.2", minOS: "26.6", sdkExists: true, wantOK: true},
		{name: "host updated", build: "27A266b", guestOS: "26.6.2", minOS: "26.6", sdkExists: true},
		{name: "guest too old", build: expectedBuild, guestOS: "26.5", minOS: "26.6", sdkExists: true},
		{name: "SDK missing", build: expectedBuild, guestOS: "26.6.2", minOS: "26.6", expectXcodeCommands: true},
		{name: "sudo unavailable", build: expectedBuild, guestOS: "26.6.2", minOS: "26.6", sudoFails: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "commands.log")
			shareRoot := filepath.Join(t.TempDir(), "Shared Files")
			appPath := filepath.Join(shareRoot, "grove-xcode.app")
			for _, dir := range []string{
				filepath.Join(appPath, "Contents"),
				filepath.Join(appPath, "Contents", "Developer", "Platforms", "MacOSX.platform"),
			} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"version.plist", "Info.plist"} {
				if err := os.WriteFile(filepath.Join(appPath, "Contents", name), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sdkPath := filepath.Join(t.TempDir(), "MacOSX.sdk")
			if tt.sdkExists {
				if err := os.Mkdir(sdkPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			writeExecutable := func(name, body string) string {
				t.Helper()
				path := filepath.Join(fakeBin, name)
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatalf("write fake %s: %v", name, err)
				}
				return path
			}
			writeExecutable("uname", "#!/bin/sh\nprintf 'Darwin\\n'\n")
			writeExecutable("id", "#!/bin/sh\nprintf '501\\n'\n")
			writeExecutable("sudo", `#!/bin/sh
set -eu
[ "${1:-}" = "-n" ] && shift
printf 'sudo %s\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
if [ "${GROVE_TEST_SUDO_FAIL:-false}" = true ]; then
  echo 'fake sudo denied' >&2
  exit 1
fi
exec "$@"
`)
			plistBuddy := writeExecutable("PlistBuddy", `#!/bin/sh
set -eu
printf 'PlistBuddy %s\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
case "$2" in
  *ProductBuildVersion*) printf '%s\n' "$GROVE_TEST_XCODE_BUILD" ;;
  *LSMinimumSystemVersion*) printf '%s\n' "$GROVE_TEST_MIN_OS" ;;
  *) exit 64 ;;
esac
`)
			writeExecutable("sw_vers", "#!/bin/sh\nprintf '%s\\n' \"$GROVE_TEST_GUEST_OS\"\n")
			writeExecutable("xcode-select", `#!/bin/sh
set -eu
printf 'xcode-select %s\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
`)
			writeExecutable("xcodebuild", `#!/bin/sh
set -eu
printf 'xcodebuild %s\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
if [ "${1:-}" = "-version" ]; then
  printf 'Xcode 27.0\nBuild version %s\n' "$GROVE_TEST_XCODE_BUILD"
fi
`)
			writeExecutable("dscl", "#!/bin/sh\n[ \"$2\" = -read ] && exit 1\nexit 0\n")
			writeExecutable("chown", "#!/bin/sh\nexit 0\n")
			writeExecutable("chmod", "#!/bin/sh\nexit 0\n")
			writeExecutable("xcrun", `#!/bin/sh
set -eu
printf 'xcrun %s\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
printf '%s\n' "$GROVE_TEST_SDK_PATH"
`)
			writeExecutable("sysctl", "#!/bin/sh\nprintf '12\\n'\n")
			writeExecutable("mkdir", `#!/bin/sh
set -eu
printf 'mkdir %s\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
`)
			writeExecutable("tee", `#!/bin/sh
set -eu
cat >/dev/null
printf 'tee %s\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
`)
			guestScript := strings.ReplaceAll(script, shQuote(guestHostXcodeMountPath), shQuote(appPath))
			guestScript = strings.ReplaceAll(guestScript, "/usr/libexec/PlistBuddy", plistBuddy)
			cmd := exec.Command("/bin/sh", "-c", guestScript)
			cmd.Env = append(os.Environ(),
				"PATH="+fakeBin+":"+os.Getenv("PATH"),
				"GROVE_TEST_COMMAND_LOG="+logPath,
				"GROVE_TEST_SUDO_FAIL="+fmt.Sprint(tt.sudoFails),
				"GROVE_TEST_XCODE_BUILD="+tt.build,
				"GROVE_TEST_GUEST_OS="+tt.guestOS,
				"GROVE_TEST_MIN_OS="+tt.minOS,
				"GROVE_TEST_SDK_PATH="+sdkPath,
			)
			output, runErr := cmd.CombinedOutput()
			logBytes, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatalf("read fake command log: %v", readErr)
			}
			log := string(logBytes)
			if tt.wantOK {
				if runErr != nil {
					t.Fatalf("startup script failed: %v\n%s\n%s", runErr, output, log)
				}
				for _, want := range []string{
					"xcode-select --switch",
					"xcodebuild -license accept",
					"xcodebuild -runFirstLaunch",
					"xcodebuild -version",
					"xcrun --sdk macosx --show-sdk-path",
				} {
					if !strings.Contains(log, want) {
						t.Errorf("successful startup did not run %q; log:\n%s", want, log)
					}
				}
				return
			}
			if runErr == nil {
				t.Fatalf("startup script succeeded, want failure\n%s\n%s", output, log)
			}
			if tt.sudoFails {
				if strings.Contains(log, "xcode-select") {
					t.Fatalf("failed privilege check advanced into setup:\n%s", log)
				}
				return
			}
			if !tt.expectXcodeCommands && (strings.Contains(log, "xcode-select") || strings.Contains(log, "xcodebuild")) {
				t.Fatalf("failed Xcode setup advanced to Xcode selection:\n%s", log)
			}
			if strings.TrimSpace(string(output)) == "" {
				t.Fatal("failed Xcode setup did not report a reason")
			}
		})
	}
}

func TestBuildStartupScript_Header(t *testing.T) {
	script := BuildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0")
	for _, want := range []string{"grove_pool='linux'", "grove_host='mac1'", "grove_vm='linux-mac1-0'", "network: isolated (sealed)"} {
		if !strings.Contains(script, want) {
			t.Errorf("startup script missing %q:\n%s", want, script)
		}
	}
	for _, gone := range []string{"tailscale up", "grove-meta.hcl", "systemctl restart nomad", "launchctl kickstart"} {
		if strings.Contains(script, gone) {
			t.Errorf("startup script still has %q:\n%s", gone, script)
		}
	}
}

func TestBuildStartupScript_AppendsPoolScript(t *testing.T) {
	pool := Pool{Name: "linux", StartupScript: "echo custom-startup"}
	script := BuildStartupScript(pool, "mac1", "linux-mac1-0")

	want := "# --- pool.startupScript ---\necho custom-startup\n"
	if !strings.HasSuffix(script, want) {
		t.Errorf("startup script missing appended pool script %q at the end:\n%s", want, script)
	}
}

func TestBuildShutdownScript_DefaultTimeout(t *testing.T) {
	script, timeout := BuildShutdownScript(Pool{Name: "linux"})
	if timeout != defaultShutdownTimeout {
		t.Errorf("want default timeout %s, got %s", defaultShutdownTimeout, timeout)
	}
	if strings.Contains(script, "nomad") {
		t.Errorf("shutdown script still drains Nomad:\n%s", script)
	}
}

func TestBuildShutdownScript_CustomTimeout(t *testing.T) {
	pool := Pool{Name: "linux", ShutdownTimeout: Duration(90 * time.Minute)}

	_, timeout := BuildShutdownScript(pool)
	if timeout != 90*time.Minute {
		t.Errorf("want 90m, got %s", timeout)
	}
}

func TestBuildShutdownScript_AppendsPoolScript(t *testing.T) {
	pool := Pool{Name: "linux", ShutdownScript: "echo custom-shutdown"}
	script, _ := BuildShutdownScript(pool)

	want := "# --- pool.shutdownScript ---\necho custom-shutdown\n"
	if !strings.HasSuffix(script, want) {
		t.Errorf("shutdown script missing appended pool script %q at the end:\n%s", want, script)
	}
}

// A macOS VM's job runs as an unprivileged account (see internal/sandbox); the startup script
// must create it, with its home, and give it no password.
func TestBuildStartupScript_MacOSCreatesJobUser(t *testing.T) {
	script := BuildStartupScript(Pool{Name: "macos"}, "mac1", "macos-mac1-0")
	userAt := strings.Index(script, "grove_priv dscl . -create /Users/"+MacOSJobUser+" UniqueID")
	homeAt := strings.Index(script, "grove_priv chown "+MacOSJobUser+":"+MacOSJobUser+" "+MacOSJobHome)
	if userAt < 0 || homeAt < 0 {
		t.Fatalf("job user or home missing:\n%s", script)
	}
	if !strings.Contains(script, "Password '*'") || !strings.Contains(script, "IsHidden 1") {
		t.Fatalf("job user must have no password and stay hidden:\n%s", script)
	}
	if strings.Contains(BuildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0"), "dscl") {
		t.Fatal("Linux startup script must not touch macOS accounts")
	}
}
