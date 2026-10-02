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

func TestBuildStartupScript_Basic(t *testing.T) {
	pool := Pool{Name: "linux"}
	script := BuildStartupScript(pool, "mac1", "linux-mac1-0", "")

	for _, want := range []string{
		"grove_pool='linux'",
		"grove_host='mac1'",
		"grove_vm='linux-mac1-0'",
		`grove_meta_file="/etc/nomad.d/grove-meta.hcl"`,
		`grove_meta_file="/usr/local/etc/nomad.d/grove-meta.hcl"`,
		`pool = "$grove_pool"`,
		`host = "$grove_host"`,
		`vm = "$grove_vm"`,
		"systemctl restart nomad",
		"launchctl kickstart -k system/com.grove.nomad",
		"sudo -n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("startup script missing %q:\n%s", want, script)
		}
	}

	if strings.Contains(script, "tailscale up") {
		t.Errorf("startup script should not join tailscale without an auth key:\n%s", script)
	}
}

func TestBuildStartupScript_UnprivilegedGuestUsesNonInteractiveSudo(t *testing.T) {
	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "sudo.log")
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	writeExecutable("id", "#!/bin/sh\necho 502\n")
	writeExecutable("uname", "#!/bin/sh\necho Linux\n")
	writeExecutable("sudo", `#!/bin/sh
set -eu
[ "${1:-}" = "-n" ] && shift
case "${1:-}" in
  mkdir) exit 0 ;;
  tee)
    tee_args="$*"
    [ "${2:-}" = "-a" ] && shift
    cat >/dev/null
    printf '%s\n' "$tee_args" >> "$GROVE_TEST_SUDO_LOG"
    ;;
  systemctl)
    printf '%s\n' "$*" >> "$GROVE_TEST_SUDO_LOG"
    ;;
  *) exit 64 ;;
esac
`)

	cmd := exec.Command("/bin/sh", "-c", BuildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0", ""))
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "GROVE_TEST_SUDO_LOG="+logPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("startup script failed through fake sudo: %v\n%s", err, output)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read sudo log: %v", err)
	}
	got := string(log)
	for _, want := range []string{
		"tee /etc/nomad.d/grove-meta.hcl",
		"tee -a /etc/nomad.d/grove-meta.hcl",
		"systemctl restart nomad",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sudo log missing %q: %s", want, got)
		}
	}
}

func TestBuildStartupScript_Tailscale(t *testing.T) {
	pool := Pool{Name: "linux"}
	script := BuildStartupScript(pool, "mac1", "linux-mac1-0", "tskey-abc")

	want := `tailscale up --authkey='tskey-abc' --hostname="$grove_vm" --ssh --accept-routes`
	if !strings.Contains(script, want) {
		t.Errorf("startup script missing tailscale up call %q:\n%s", want, script)
	}
}

// TestBuildStartupScript_MacOSCPUTotalCompute is the regression test for the live-run defect:
// Nomad on Apple Silicon fingerprints cpu.totalcompute in the single digits of MHz, which fails
// placement for any job requesting a realistic CPU value. The startup script must compute and
// write a sane cpu_total_compute override into the dynamic grove-meta.hcl on macOS — see
// docs/OPERATIONS.md "jobs pending with DimensionExhausted cpu on macOS".
func TestBuildStartupScript_MacOSCPUTotalCompute(t *testing.T) {
	pool := Pool{Name: "macos"}
	script := BuildStartupScript(pool, "mac1", "macos-mac1-0", "")

	for _, want := range []string{
		"grove_cpu_total_compute=$(( $(sysctl -n hw.ncpu) * 2000 ))",
		"cpu_total_compute = $grove_cpu_total_compute",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("startup script missing %q:\n%s", want, script)
		}
	}

	// The cpu_total_compute line must land inside the same `client { ... }` block as the meta
	// stanza, not as a sibling top-level block (Nomad's client.hcl schema only allows one `client`
	// block per merged config directory).
	clientOpen := strings.Index(script, "client {")
	metaIdx := strings.Index(script, "pool = \"$grove_pool\"")
	cpuIdx := strings.Index(script, "cpu_total_compute = $grove_cpu_total_compute")
	closeIdx := strings.Index(script, "\n}\nGROVE_META_END")
	if clientOpen < 0 || metaIdx < 0 || cpuIdx < 0 || closeIdx < 0 {
		t.Fatalf("could not locate client{}/meta/cpu_total_compute/close markers in:\n%s", script)
	}
	if !(clientOpen < metaIdx && metaIdx < cpuIdx && cpuIdx < closeIdx) {
		t.Errorf("cpu_total_compute is not nested inside the client{} block as expected:\n%s", script)
	}
}

func TestBuildStartupScript_HostXcodeSetupIsFailClosedAndVerified(t *testing.T) {
	const expectedBuild = "27A266a"
	pool := Pool{
		Name: "macos",
		HostXcode: &HostXcodeConfig{
			Path:         "/Applications/Xcode_27.app",
			BuildVersion: expectedBuild,
		},
	}
	script := BuildStartupScript(pool, "mac1", "macos-mac1-0", "")
	for _, want := range []string{
		"launchctl bootout system/com.grove.nomad",
		"/Volumes/My Shared Files/grove-xcode.app",
		"ProductBuildVersion",
		"LSMinimumSystemVersion",
		"xcode-select --switch",
		"xcodebuild -license accept",
		"xcodebuild -runFirstLaunch",
		"xcodebuild -version",
		"xcrun --sdk macosx --show-sdk-path",
		"launchctl bootstrap system /Library/LaunchDaemons/com.grove.nomad.plist",
		"launchctl kickstart -k system/com.grove.nomad",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("host Xcode startup script missing %q", want)
		}
	}
	if strings.Contains(script, pool.HostXcode.Path) {
		t.Errorf("host-only Xcode path leaked into the guest startup script")
	}
	ordered := []string{
		"launchctl bootout system/com.grove.nomad",
		"ProductBuildVersion",
		"xcode-select --switch",
		"xcodebuild -license accept",
		"xcodebuild -runFirstLaunch",
		"xcodebuild -version",
		"xcrun --sdk macosx --show-sdk-path",
		"launchctl bootstrap system /Library/LaunchDaemons/com.grove.nomad.plist",
		"launchctl kickstart -k system/com.grove.nomad",
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
		name                 string
		build                string
		guestOS              string
		minOS                string
		sdkExists            bool
		launchctlPrintStatus string
		sudoFails            bool
		expectXcodeCommands  bool
		wantOK               bool
	}{
		{name: "matching image", build: expectedBuild, guestOS: "26.6.2", minOS: "26.6", sdkExists: true, launchctlPrintStatus: "0", wantOK: true},
		{name: "host updated", build: "27A266b", guestOS: "26.6.2", minOS: "26.6", sdkExists: true, launchctlPrintStatus: "0"},
		{name: "guest too old", build: expectedBuild, guestOS: "26.5", minOS: "26.6", sdkExists: true, launchctlPrintStatus: "0"},
		{name: "SDK missing", build: expectedBuild, guestOS: "26.6.2", minOS: "26.6", launchctlPrintStatus: "0", expectXcodeCommands: true},
		{name: "Nomad inspection failed", build: expectedBuild, guestOS: "26.6.2", minOS: "26.6", launchctlPrintStatus: "5"},
		{name: "sudo unavailable", build: expectedBuild, guestOS: "26.6.2", minOS: "26.6", launchctlPrintStatus: "0", sudoFails: true},
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
			writeExecutable("launchctl", `#!/bin/sh
set -eu
printf 'launchctl %s\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
case "${1:-}" in
  print)
    if [ -f "$GROVE_TEST_NOMAD_BOOTED_OUT" ]; then
      echo 'Could not find service' >&2
      exit 113
    fi
    status=${GROVE_TEST_LAUNCHCTL_PRINT_STATUS:-0}
    if [ "$status" -ne 0 ]; then echo 'mock launchctl detail'; fi
    exit "$status"
    ;;
  bootout) touch "$GROVE_TEST_NOMAD_BOOTED_OUT" ;;
esac
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
			writeExecutable("curl", `#!/bin/sh
set -eu
printf 'curl %s\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
printf '%s\n' '{"client":{"ok":true},"server":{"ok":false}}'
`)
			writeExecutable("jq", `#!/bin/sh
set -eu
body=$(cat)
case "$body" in
  *'"client":{"ok":true}'*) exit 0 ;;
  *) exit 1 ;;
esac
`)

			guestScript := strings.ReplaceAll(script, shQuote(guestHostXcodeMountPath), shQuote(appPath))
			guestScript = strings.ReplaceAll(guestScript, "/usr/libexec/PlistBuddy", plistBuddy)
			cmd := exec.Command("/bin/sh", "-c", guestScript)
			cmd.Env = append(os.Environ(),
				"PATH="+fakeBin+":"+os.Getenv("PATH"),
				"GROVE_TEST_COMMAND_LOG="+logPath,
				"GROVE_TEST_NOMAD_BOOTED_OUT="+filepath.Join(t.TempDir(), "nomad-booted-out"),
				"GROVE_TEST_LAUNCHCTL_PRINT_STATUS="+tt.launchctlPrintStatus,
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
					"launchctl bootout system/com.grove.nomad",
					"xcode-select --switch",
					"xcodebuild -license accept",
					"xcodebuild -runFirstLaunch",
					"xcodebuild -version",
					"xcrun --sdk macosx --show-sdk-path",
					"launchctl bootstrap system /Library/LaunchDaemons/com.grove.nomad.plist",
					"launchctl kickstart -k system/com.grove.nomad",
				} {
					if !strings.Contains(log, want) {
						t.Errorf("successful startup did not run %q; log:\n%s", want, log)
					}
				}
				if strings.Index(log, "xcrun --sdk macosx --show-sdk-path") > strings.Index(log, "launchctl bootstrap") {
					t.Errorf("Nomad restarted before the SDK check completed:\n%s", log)
				}
				return
			}
			if runErr == nil {
				t.Fatalf("startup script succeeded, want failure\n%s\n%s", output, log)
			}
			if tt.sudoFails || tt.launchctlPrintStatus != "0" {
				if strings.Contains(log, "launchctl bootout") || strings.Contains(log, "xcode-select") || strings.Contains(log, "launchctl bootstrap") {
					t.Fatalf("failed privilege/launchctl inspection advanced into setup:\n%s", log)
				}
				return
			}
			if !strings.Contains(log, "launchctl bootout system/com.grove.nomad") {
				t.Fatalf("Nomad was not stopped before the failed Xcode setup:\n%s", log)
			}
			if !tt.expectXcodeCommands && (strings.Contains(log, "xcode-select") || strings.Contains(log, "xcodebuild")) {
				t.Fatalf("failed Xcode setup advanced to Xcode selection or Nomad restart:\n%s", log)
			}
			if strings.Contains(log, "launchctl bootstrap") || strings.Contains(log, "launchctl kickstart") {
				t.Fatalf("failed Xcode setup advanced to Nomad restart:\n%s", log)
			}
			if strings.TrimSpace(string(output)) == "" {
				t.Fatal("failed Xcode setup did not report a reason")
			}
		})
	}
}

func TestBuildStartupScript_MacOSNomadServiceAndReadiness(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		launchctlPrintStatus string
		clientHealthy        bool
		wantOK               bool
		wantBootstrap        bool
	}{
		{name: "bootstraps fresh image and ignores unhealthy server field", launchctlPrintStatus: "113", clientHealthy: true, wantOK: true, wantBootstrap: true},
		{name: "kickstarts loaded service without bootstrapping", launchctlPrintStatus: "0", clientHealthy: true, wantOK: true},
		{name: "fails closed when service inspection fails", launchctlPrintStatus: "5"},
		{name: "fails when client never becomes healthy", launchctlPrintStatus: "113", wantBootstrap: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "commands.log")
			writeExecutable := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
					t.Fatalf("write fake %s: %v", name, err)
				}
			}
			writeExecutable("uname", "#!/bin/sh\nprintf 'Darwin\\n'\n")
			writeExecutable("id", "#!/bin/sh\nprintf '501\\n'\n")
			writeExecutable("sudo", `#!/bin/sh
set -eu
[ "${1:-}" = "-n" ] && shift
printf 'sudo %s\\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
exec "$@"
`)
			writeExecutable("launchctl", `#!/bin/sh
set -eu
printf 'launchctl %s\\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
case "${1:-}" in
  print) exit "$GROVE_TEST_LAUNCHCTL_PRINT_STATUS" ;;
  bootstrap) exit 0 ;;
esac
`)
			writeExecutable("sysctl", "#!/bin/sh\nprintf '12\\n'\n")
			writeExecutable("mkdir", "#!/bin/sh\nexit 0\n")
			writeExecutable("tee", "#!/bin/sh\ncat >/dev/null\n")
			writeExecutable("curl", `#!/bin/sh
set -eu
printf 'curl %s\\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
if [ "$GROVE_TEST_CLIENT_HEALTHY" = true ]; then
  printf '%s\\n' '{"client":{"ok":true},"server":{"ok":false}}'
else
  printf '%s\\n' '{"client":{"ok":false},"server":{"ok":false}}'
fi
`)
			writeExecutable("jq", `#!/bin/sh
set -eu
body=$(cat)
case "$body" in
  *'"client":{"ok":true}'*) exit 0 ;;
  *) exit 1 ;;
esac
`)
			writeExecutable("sleep", "#!/bin/sh\nexit 0\n")

			cmd := exec.Command("/bin/sh", "-c", BuildStartupScript(Pool{Name: "macos"}, "mac1", "macos-mac1-0", ""))
			cmd.Env = append(os.Environ(),
				"PATH="+fakeBin+":"+os.Getenv("PATH"),
				"GROVE_TEST_COMMAND_LOG="+logPath,
				"GROVE_TEST_LAUNCHCTL_PRINT_STATUS="+tt.launchctlPrintStatus,
				"GROVE_TEST_CLIENT_HEALTHY="+fmt.Sprint(tt.clientHealthy),
			)
			output, runErr := cmd.CombinedOutput()
			logBytes, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read command log: %v", err)
			}
			log := string(logBytes)
			if tt.wantOK && runErr != nil {
				t.Fatalf("startup script failed: %v\\n%s\\n%s", runErr, output, log)
			}
			if !tt.wantOK && runErr == nil {
				t.Fatalf("startup script succeeded, want failure\\n%s\\n%s", output, log)
			}
			if tt.launchctlPrintStatus == "5" {
				if strings.Contains(log, "launchctl kickstart") || strings.Contains(log, "curl ") {
					t.Fatalf("service inspection failure advanced into restart/readiness:\\n%s", log)
				}
				return
			}
			if tt.wantBootstrap != strings.Contains(log, "launchctl bootstrap system /Library/LaunchDaemons/com.grove.nomad.plist") {
				t.Errorf("bootstrap presence mismatch (want %v):\\n%s", tt.wantBootstrap, log)
			}
			if !strings.Contains(log, "launchctl kickstart -k system/com.grove.nomad") {
				t.Errorf("Nomad was not kickstarted:\\n%s", log)
			}
			if tt.clientHealthy {
				if !strings.Contains(log, "curl --silent --max-time 2 http://127.0.0.1:4646/v1/agent/health") {
					t.Errorf("local agent health was not checked:\\n%s", log)
				}
			} else {
				if strings.Count(log, "curl --silent") != 40 {
					t.Errorf("unhealthy client did not exhaust bounded readiness attempts: got %d\\n%s", strings.Count(log, "curl --silent"), log)
				}
				if !strings.Contains(string(output), "Nomad client did not become healthy") {
					t.Errorf("readiness timeout did not explain failure: %s", output)
				}
			}
		})
	}
}

func TestBuildStartupScript_AppendsPoolScript(t *testing.T) {
	pool := Pool{Name: "linux", StartupScript: "echo custom-startup"}
	script := BuildStartupScript(pool, "mac1", "linux-mac1-0", "")

	want := "# --- pool.startupScript ---\necho custom-startup\n"
	if !strings.HasSuffix(script, want) {
		t.Errorf("startup script missing appended pool script %q at the end:\n%s", want, script)
	}
}

func TestBuildShutdownScript_DefaultTimeout(t *testing.T) {
	pool := Pool{Name: "linux"}
	script, timeout := BuildShutdownScript(pool)

	if timeout != defaultShutdownTimeout {
		t.Errorf("want default timeout %s, got %s", defaultShutdownTimeout, timeout)
	}

	for _, want := range []string{
		"nomad node drain -self -enable -deadline 2h -m 'grove recycle'",
		"nomad node status -self -json",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("shutdown script missing %q:\n%s", want, script)
		}
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
