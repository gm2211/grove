package fleet

import (
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
	} {
		if !strings.Contains(script, want) {
			t.Errorf("startup script missing %q:\n%s", want, script)
		}
	}

	if strings.Contains(script, "tailscale up") {
		t.Errorf("startup script should not join tailscale without an auth key:\n%s", script)
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
