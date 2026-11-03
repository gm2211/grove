package nomadjobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTaskImageInterpolationRealNomad proves, against a real Nomad dev agent and a real docker
// daemon, that a docker task's `image = "${NOMAD_META_task_image}"` resolves from dispatch meta,
// with the same entrypoint/args/user/image_pull_timeout shape build.nomad.hcl uses for its main
// task. It needs a nomad binary (GROVE_NOMAD_BIN, set in CI), a running docker daemon, the
// default Nomad ports free on 127.0.0.1 and network access to pull GROVE_TEST_IMAGE (default
// alpine:3.20); it is skipped otherwise.
func TestTaskImageInterpolationRealNomad(t *testing.T) {
	bin := os.Getenv("GROVE_NOMAD_BIN")
	if bin == "" {
		t.Skip("GROVE_NOMAD_BIN not set")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("no docker daemon: %v", err)
	}
	image := os.Getenv("GROVE_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// Pull by tag, then dispatch the immutable digest: that is what grove dispatches in practice.
	if out, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s: %v\n%s", image, err, out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", image).Output(); err == nil && strings.Contains(string(out), "@sha256:") {
		image = strings.TrimSpace(string(out))
	}

	dir := t.TempDir()
	// Cloud fingerprinters only slow a test agent down (or fail behind a proxy).
	extras := filepath.Join(dir, "extras.hcl")
	if err := os.WriteFile(extras, []byte(`client {
  options {
    "fingerprint.denylist" = "env_aws,env_gce,env_azure,env_digitalocean"
  }
}
advertise {
  http = "127.0.0.1"
  rpc  = "127.0.0.1"
  serf = "127.0.0.1"
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var agentLog lockedBuffer
	agent := exec.Command(bin, "agent", "-dev", "-bind=127.0.0.1", "-config="+extras, "-data-dir="+filepath.Join(dir, "data"))
	agent.Stdout, agent.Stderr = &agentLog, &agentLog
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = agent.Process.Kill()
		_ = agent.Wait()
		if t.Failed() {
			t.Logf("--- nomad agent output ---\n%s", agentLog.String())
		}
	})

	nomadCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(os.Environ(), "NOMAD_ADDR=http://127.0.0.1:4646")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("nomad %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	for deadline := time.Now().Add(2*time.Minute); ; time.Sleep(time.Second) {
		cmd := exec.CommandContext(ctx, bin, "node", "status")
		cmd.Env = append(os.Environ(), "NOMAD_ADDR=http://127.0.0.1:4646")
		out, err := cmd.CombinedOutput()
		if err == nil && strings.Contains(string(out), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dev agent never became ready (last `nomad node status`: err=%v\n%s)", err, out)
		}
	}

	spec := filepath.Join(t.TempDir(), "job.hcl")
	hcl := `job "env-probe" {
  datacenters = ["dc1"]
  type        = "batch"
  parameterized {
    payload       = "forbidden"
    meta_required = ["task_image"]
  }
  group "g" {
    restart { attempts = 0 }
    task "main" {
      driver = "docker"
      user   = "root"
      config {
        image              = "${NOMAD_META_task_image}"
        entrypoint         = ["/bin/sh"]
        args               = ["-c", ". /etc/os-release; echo probe-ran-in-$ID"]
        image_pull_timeout = "15m"
      }
    }
  }
}
`
	if err := os.WriteFile(spec, []byte(hcl), 0o600); err != nil {
		t.Fatal(err)
	}
	nomadCmd("job", "run", "-detach", spec)
	out := nomadCmd("job", "dispatch", "-detach", "-meta", "task_image="+image, "env-probe")
	fields := strings.Fields(out)
	var child string
	for i, f := range fields {
		if f == "ID" && i+2 < len(fields) {
			child = fields[i+2]
		}
	}
	if child == "" {
		t.Fatalf("no dispatched job ID in:\n%s", out)
	}
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(2 * time.Second) {
		status := nomadCmd("job", "status", child)
		if strings.Contains(status, "complete") {
			break
		}
		if strings.Contains(status, "failed") || time.Now().After(deadline) {
			t.Fatalf("job did not complete:\n%s", status)
		}
	}
	logs := nomadCmd("alloc", "logs", "-job", child, "main")
	// An unresolved ${NOMAD_META_task_image} could not have started a container at all.
	if !strings.Contains(logs, "probe-ran-in-") {
		t.Fatalf("task did not run in the image from meta task_image=%s:\n%s", image, logs)
	}
}

// lockedBuffer is an io.Writer safe for a child process's stdout and stderr at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
