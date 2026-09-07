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
