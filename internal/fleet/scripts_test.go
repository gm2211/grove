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

func TestBuildStartupScript_MacOSNomadRPCServer(t *testing.T) {
	for _, tt := range []struct {
		name    string
		address string
		want    string
	}{
		{name: "IPv4", address: "100.101.229.60:4647", want: `servers = ["100.101.229.60:4647"]`},
		{name: "IPv6", address: "[2001:db8::60]:4647", want: `servers = ["[2001:db8::60]:4647"]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			script := buildStartupScript(Pool{Name: "macos"}, "mac1", "macos-mac1-0", Options{NomadRPCAddress: tt.address})
			configAt := strings.Index(script, tt.want)
			restartAt := strings.Index(script, "launchctl kickstart -k system/com.grove.nomad")
			if configAt < 0 || restartAt < 0 || configAt > restartAt {
				t.Fatalf("Nomad RPC server config missing or written after restart (%q):\n%s", tt.want, script)
			}
			if strings.Contains(script, "grove_priv tee /etc/nomad.d/grove-rpc.hcl") {
				t.Fatal("macOS startup should not write RPC config under /etc/nomad.d")
			}
		})
	}
}

func TestBuildStartupScript_MacOSExplicitLoopbackAdvertisement(t *testing.T) {
	for _, pool := range []Pool{{Name: "macos"}, {Name: "macos", HostXcode: &HostXcodeConfig{Path: "/Applications/Xcode.app", BuildVersion: "27A266a"}}} {
		script := buildStartupScript(pool, "mac1", "macos-mac1-0", Options{})
		restartAt := strings.Index(script, "launchctl kickstart -k system/com.grove.nomad")
		for _, line := range []string{`http = "127.0.0.1:4646"`, `rpc = "127.0.0.1:4647"`, `serf = "127.0.0.1:4648"`} {
			at := strings.Index(script, line)
			if at < 0 || restartAt < 0 || at > restartAt {
				t.Fatalf("explicit advertisement missing before Nomad restart: %s", line)
			}
		}
	}
}

func TestBuildStartupScript_LinuxMatchesV010(t *testing.T) {
	want, err := os.ReadFile("testdata/startup-linux-v0.1.10.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Turning Tailscale off, the home fence and the broker block are the intended changes since
	// v0.1.10: every Linux VM is recreated once to get them. Everything else must stay
	// byte-identical.
	got := BuildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0", "")
	got = strings.Replace(got, tailscaleOffScript+"\n", "", 1)
	got = strings.Replace(got, homeFenceScript(Pool{Name: "linux"})+"\n"+brokerScript()+"\n", "", 1)
	if got != string(want) {
		t.Fatalf("Linux startup script drifted from v0.1.10; this would recreate healthy Linux workers\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestNomadRPCAddressFromHTTPURL(t *testing.T) {
	for _, tt := range []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{name: "hostname", url: "http://nomad.cp.example:4646", want: "nomad.cp.example:4647"},
		{name: "IPv4", url: "http://100.101.229.60:4646", want: "100.101.229.60:4647"},
		{name: "IPv6", url: "http://[2001:db8::60]:4646", want: "[2001:db8::60]:4647"},
		{name: "unset", want: ""},
		{name: "missing host", url: "http:///nomad", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NomadRPCAddressFromHTTPURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NomadRPCAddressFromHTTPURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("NomadRPCAddressFromHTTPURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
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
	// No default route: the broker block looks for no broker (see broker_test.go).
	writeExecutable("ip", "#!/bin/sh\nexit 0\n")
	writeExecutable("sudo", `#!/bin/sh
set -eu
[ "${1:-}" = "-n" ] && shift
case "${1:-}" in
  mkdir|rm) exit 0 ;;
  tee)
    tee_args="$*"
    [ "${2:-}" = "-a" ] && shift
    cat >/dev/null
    printf '%s\n' "$tee_args" >> "$GROVE_TEST_SUDO_LOG"
    ;;
  systemctl)
    printf '%s\n' "$*" >> "$GROVE_TEST_SUDO_LOG"
    ;;
  chmod) exit 0 ;;
  /bin/sh)
    cat >/dev/null
    printf 'fence %s\n' "$*" >> "$GROVE_TEST_SUDO_LOG"
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

// No VM joins the tailnet any more, whatever key it is handed: a shared VM turns the image's
// Tailscale off and reaches Nomad through its host Mac's NAT, at the address it is given.
func TestBuildStartupScript_StaysOffTheTailnet(t *testing.T) {
	for _, pool := range []Pool{{Name: "linux"}, {Name: "macos"}} {
		script := buildStartupScript(pool, "mac1", pool.Name+"-mac1-0", Options{TailscaleAuthKey: "tskey-abc", NomadRPCAddress: "100.101.229.60:4647"})
		if strings.Contains(script, "tailscale up") || strings.Contains(script, "tskey-abc") {
			t.Errorf("%s: startup script joins the tailnet or carries the key:\n%s", pool.Name, script)
		}
		offAt := strings.Index(script, "systemctl disable --now tailscaled.service")
		restartAt := strings.LastIndex(script, "systemctl restart nomad")
		dir := "/etc/nomad.d"
		if pool.MacOS() {
			offAt = strings.Index(script, "launchctl bootout system/homebrew.mxcl.tailscale")
			restartAt = strings.LastIndex(script, "launchctl kickstart -k system/com.grove.nomad")
			dir = "/usr/local/etc/nomad.d"
		}
		rpcAt := strings.Index(script, "grove_priv tee "+dir+"/grove-rpc.hcl")
		if offAt < 0 || rpcAt < 0 || restartAt < offAt || restartAt < rpcAt || !strings.Contains(script, `servers = ["100.101.229.60:4647"]`) {
			t.Errorf("%s: Tailscale not turned off, or no RPC server, before Nomad restarts:\n%s", pool.Name, script)
		}
	}
	if strings.Contains(BuildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0", "tskey-abc"), "tskey-abc") {
		t.Error("BuildStartupScript must ignore the tailnet key it is passed")
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
			writeExecutable("pfctl", "#!/bin/sh\ncase \"$*\" in *'-s rules'*) echo 'block user = 7460' ;; *'-s info'*) echo 'Status: Enabled' ;; esac\n")
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
			writeExecutable("dscl", "#!/bin/sh\n[ \"$2\" = -read ] && exit 1\n[ \"$2\" = -authonly ] && exit 1\nexit 0\n")
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
			writeExecutable("dscl", "#!/bin/sh\n[ \"$2\" = -read ] && exit 1\n[ \"$2\" = -authonly ] && exit 1\nexit 0\n")
			writeExecutable("chown", "#!/bin/sh\nexit 0\n")
			writeExecutable("chmod", "#!/bin/sh\nexit 0\n")
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
			// No default route: no broker exception, no broker (see broker_test.go).
			writeExecutable("route", "#!/bin/sh\nexit 1\n")
			writeExecutable("pfctl", `#!/bin/sh
printf 'pfctl %s\\n' "$*" >> "$GROVE_TEST_COMMAND_LOG"
case "$*" in *"-s rules"*) echo 'block return out quick proto tcp from any to 10.0.0.0/8 user = 7460' ;; *"-s info"*) echo 'Status: Enabled' ;; esac
`)

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

// macOS jobs run as an unprivileged account (nomad/jobs/*.nomad.hcl's `user`); the startup script
// must create it, with its home, before Nomad is restarted into service.
func TestBuildStartupScript_MacOSCreatesJobUserBeforeNomad(t *testing.T) {
	script := buildStartupScript(Pool{Name: "macos"}, "mac1", "macos-mac1-0", Options{})
	userAt := strings.Index(script, "grove_priv dscl . -create /Users/"+MacOSJobUser+" UniqueID")
	homeAt := strings.Index(script, "grove_priv chown "+MacOSJobUser+":"+MacOSJobUser+" "+MacOSJobHome)
	restartAt := strings.Index(script, "launchctl kickstart -k system/com.grove.nomad")
	if userAt < 0 || homeAt < 0 || restartAt < 0 || userAt > restartAt || homeAt > restartAt {
		t.Fatalf("job user or home missing, or created after Nomad restarts:\n%s", script)
	}
	if !strings.Contains(script, "Password '*'") || !strings.Contains(script, "IsHidden 1") {
		t.Fatalf("job user must have no password and stay hidden:\n%s", script)
	}
	if strings.Contains(BuildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0", ""), "dscl") {
		t.Fatal("Linux startup script must not touch macOS accounts")
	}
}

// TestBuildStartupScript_TailscaleFailureStillRestartsNomad is the regression test for the Studio's
// Linux VM, restarted 40,426 times: under `set -eu` a failing `tailscale up` (expired auth key)
// ended the startup script before Nomad was restarted with the VM's meta.
func TestBuildStartupScript_LinuxTurnsTailscaleOffThenRestartsNomad(t *testing.T) {
	fakeBin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("id", "#!/bin/sh\necho 0\n")
	write("uname", "#!/bin/sh\necho Linux\n")
	write("tee", "#!/bin/sh\ncat >/dev/null\n")
	write("mkdir", "#!/bin/sh\nexit 0\n")
	write("tailscale", "#!/bin/sh\necho \"tailscale $*\" >> \"$GROVE_TEST_CALLS\"\nexit 1\n")
	write("systemctl", "#!/bin/sh\necho \"systemctl $*\" >> \"$GROVE_TEST_CALLS\"\n")
	write("chmod", "#!/bin/sh\nexit 0\n")
	write("iptables", "#!/bin/sh\nexit 0\n")
	write("ip", "#!/bin/sh\nexit 0\n")

	script := BuildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0", "tskey-expired")
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "GROVE_TEST_CALLS="+calls)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("startup script failed: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(calls)
	if string(got) != "systemctl disable --now tailscaled.service\nsystemctl daemon-reload\nsystemctl restart nomad\n" {
		t.Fatalf("calls = %q, want Tailscale off, then one Nomad restart", got)
	}
}

// Shared pools' jobs reach the public internet only: the fence must be in every shared startup
// script, before Nomad is restarted into service, and it must cover the tailnet and the LAN.
func TestBuildStartupScript_FencesJobsOffHomeNetworks(t *testing.T) {
	for _, pool := range []Pool{{Name: "linux"}, {Name: "macos"}, {Name: "gpu"}} {
		script := buildStartupScript(pool, "mac1", pool.Name+"-mac1-0", Options{})
		fenceAt := strings.Index(script, "jobs reach the internet, not home")
		restartAt := strings.LastIndex(script, "com.grove.nomad")
		if !pool.MacOS() {
			restartAt = strings.LastIndex(script, "systemctl restart nomad")
		}
		if fenceAt < 0 || restartAt < 0 || fenceAt > restartAt {
			t.Fatalf("%s: home fence missing or after the Nomad restart:\n%s", pool.Name, script)
		}
		for _, cidr := range []string{"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16"} {
			if !strings.Contains(script, cidr) {
				t.Errorf("%s: fence does not cover %s", pool.Name, cidr)
			}
		}
	}
	mac := homeFenceScript(Pool{Name: "macos"})
	if !strings.Contains(mac, "user { "+MacOSJobUser+", "+MacOSSourceUser+" }") || !strings.Contains(mac, "/Users/"+MacOSSourceUser+" UniqueID 7461") || !strings.Contains(mac, "fc00::/7") || !strings.Contains(mac, "RunAtLoad") {
		t.Errorf("macOS fence must match the job user, cover tailnet IPv6 and reload at boot:\n%s", mac)
	}
	linux := homeFenceScript(Pool{Name: "linux"})
	if !strings.Contains(linux, "DOCKER-USER") || !strings.Contains(linux, "ExecStartPre=/usr/local/sbin/grove-egress-fence") {
		t.Errorf("Linux fence must sit in DOCKER-USER and rerun before Docker starts:\n%s", linux)
	}
	// The guest's resolver is its host Mac: no job may reach it, DNS included.
	if strings.Contains(linux, "--dport 53") || strings.Contains(mac, "port 53") {
		t.Errorf("the fences must not let jobs reach the host's DNS:\n%s\n%s", linux, mac)
	}
	if !strings.Contains(mac, "networksetup -setdnsservers \"$grove_dns_service\" 1.1.1.1 8.8.8.8") {
		t.Errorf("a shared macOS guest must resolve through public resolvers:\n%s", mac)
	}
	if i, j := strings.Index(mac, "networksetup -setdnsservers"), strings.Index(mac, "pfctl"); i < 0 || j < 0 || i > j {
		t.Errorf("DNS must move off the host before the fence goes up:\n%s", mac)
	}
	if sealed := buildStartupScript(Pool{Name: "linux", Network: NetworkIsolated}, "mac1", "linux-mac1-0", Options{}); strings.Contains(sealed, "jobs reach the internet, not home") || strings.Contains(sealed, "--dport 53") {
		t.Error("sealed pools keep their own fence unchanged")
	}
}

// The Linux fence script itself must run: shell syntax, no DNS exception, and exactly one way to
// the host: TCP to the DHCP default gateway's broker port, ahead of the rejects, and none at all
// when the guest has no IPv4 default route (or ip prints something that isn't an address).
func TestSharedEgressFenceScript_Runs(t *testing.T) {
	for _, tc := range []struct {
		name, route, wantException string
	}{
		{"gateway", "default via 192.168.64.1 dev enp0s1 proto dhcp src 192.168.64.5 metric 100", "-w -A GROVE-EGRESS -d 192.168.64.1/32 -p tcp --dport 3128 -j RETURN"},
		{"no default route", "", ""},
		{"not an address", "default via fe80::1 dev enp0s1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			log := filepath.Join(t.TempDir(), "iptables.log")
			if err := os.WriteFile(filepath.Join(fakeBin, "iptables"), []byte("#!/bin/sh\necho \"$*\" >> \"$GROVE_TEST_LOG\"\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			ip := "#!/bin/sh\n[ \"$*\" = \"-4 route show default\" ] || exit 64\n"
			if tc.route != "" {
				ip += "echo '" + tc.route + "'\n"
			}
			if err := os.WriteFile(filepath.Join(fakeBin, "ip"), []byte(ip), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-s")
			cmd.Stdin = strings.NewReader(sharedEgressFenceScript())
			cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "GROVE_TEST_LOG="+log)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fence script failed: %v\n%s", err, out)
			}
			data, _ := os.ReadFile(log)
			got := string(data)
			rejectAt := strings.Index(got, "-d 10.0.0.0/8 -j REJECT")
			if rejectAt < 0 || !strings.Contains(got, "-d 100.64.0.0/10 -j REJECT") || !strings.Contains(got, "-d 192.168.0.0/16 -j REJECT") {
				t.Fatalf("home networks not rejected:\n%s", got)
			}
			if strings.Contains(got, "--dport 53") {
				t.Fatalf("no resolver may be let through:\n%s", got)
			}
			var exceptions []string
			for _, line := range strings.Split(got, "\n") {
				if strings.Contains(line, "GROVE-EGRESS -d ") && !strings.Contains(line, "REJECT") {
					exceptions = append(exceptions, line)
				}
			}
			switch {
			case tc.wantException == "" && len(exceptions) != 0:
				t.Fatalf("no gateway, yet an exception:\n%s", got)
			case tc.wantException != "" && (len(exceptions) != 1 || exceptions[0] != tc.wantException):
				t.Fatalf("exceptions = %q, want just %q", exceptions, tc.wantException)
			case tc.wantException != "" && strings.Index(got, tc.wantException) > rejectAt:
				t.Fatalf("the broker exception must come before the rejects:\n%s", got)
			}
		})
	}
	if strings.Contains(egressFenceScript, "3128") || strings.Contains(egressFenceScript, "@BROKER") {
		t.Errorf("a sealed guest's fence has no broker exception:\n%s", egressFenceScript)
	}
}

// The macOS fence loader puts the job user's one exception, TCP to the default gateway's broker
// port, ahead of the anchor's block rule, and loads no exception when there's no gateway.
func TestMacOSHomeFenceLoader_BrokerException(t *testing.T) {
	for _, tc := range []struct {
		name, route, want string
	}{
		{"gateway", "   route to: default\ndestination: default\n    gateway: 192.168.64.1\n  interface: en0", "pass out quick proto tcp from any to 192.168.64.1 port 3128 user " + MacOSJobUser + "\n"},
		{"no gateway", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			dir := t.TempDir()
			anchor := filepath.Join(dir, "anchor")
			for name, body := range map[string]string{
				"route": "#!/bin/sh\n[ \"$*\" = \"-n get default\" ] || exit 64\n" + func() string {
					if tc.route == "" {
						return "exit 1\n"
					}
					return "printf '%s\\n' '" + strings.ReplaceAll(tc.route, "\n", "' '") + "'\n"
				}(),
				"sleep": "#!/bin/sh\nexit 0\n",
				"pfctl": `#!/bin/sh
case "$*" in
  "-q -f /etc/pf.conf") ;;
  "-q -a com.apple/grove-jobs -f -") cat > "$GROVE_TEST_ANCHOR" ;;
  "-q -E") ;;
  "-q -a com.apple/grove-jobs -s rules") cat "$GROVE_TEST_ANCHOR" ;;
  "-s info") echo "Status: Enabled" ;;
  *) exit 64 ;;
esac
`,
				"cat": "#!/bin/sh\nif [ \"$1\" = /usr/local/etc/grove/pf-jobs.conf ]; then printf '%s' \"$GROVE_TEST_RULES\"; else exec /bin/cat \"$@\"; fi\n",
			} {
				if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("/bin/sh", "-s")
			cmd.Stdin = strings.NewReader(macosHomeFenceLoader)
			cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "GROVE_TEST_ANCHOR="+anchor, "GROVE_TEST_RULES="+macosHomeFenceRules)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("loader failed: %v\n%s", err, out)
			}
			got, err := os.ReadFile(anchor)
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.want + macosHomeFenceRules; string(got) != want {
				t.Errorf("anchor =\n%s\nwant:\n%s", got, want)
			}
		})
	}
}
