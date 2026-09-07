package nomadjobs

import (
	"strings"
	"testing"
	"text/template"
)

// renderData mirrors the fields the grove server is expected to supply when rendering a job
// template (see README.md). CPU/Memory are optional — each template falls back to a default when
// they're zero.
type renderData struct {
	Kind              string
	Pool              string
	CPU               int
	Memory            int
	AllowDockerSocket bool
}

func render(t *testing.T, file string, data renderData) string {
	t.Helper()
	tmpl, err := template.New(file).ParseFS(FS, file)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var sb strings.Builder
	if err := tmpl.ExecuteTemplate(&sb, file, data); err != nil {
		t.Fatalf("execute %s: %v", file, err)
	}
	return sb.String()
}

func TestRenderAllKindsAndPools(t *testing.T) {
	files := map[string]string{
		"build": "build.nomad.hcl",
		"agent": "agent.nomad.hcl",
		"shell": "shell.nomad.hcl",
	}
	pools := []string{"linux", "macos"}

	for kind, file := range files {
		for _, pool := range pools {
			t.Run(kind+"/"+pool, func(t *testing.T) {
				out := render(t, file, renderData{Kind: kind, Pool: pool})

				wantJob := `job "grove-` + kind + `-` + pool + `"`
				if !strings.Contains(out, wantJob) {
					t.Errorf("rendered %s/%s missing job name %q\n---\n%s", kind, pool, wantJob, out)
				}

				if !strings.Contains(out, `${meta.pool}`) {
					t.Errorf("rendered %s/%s missing ${meta.pool} constraint attribute\n---\n%s", kind, pool, out)
				}
				if !strings.Contains(out, `value     = "`+pool+`"`) {
					t.Errorf("rendered %s/%s constraint value missing pool %q\n---\n%s", kind, pool, pool, out)
				}

				// Driver must match the pool: macos -> raw_exec, everything else -> docker.
				if pool == "macos" {
					if !strings.Contains(out, `driver = "raw_exec"`) {
						t.Errorf("rendered %s/%s expected raw_exec driver\n---\n%s", kind, pool, out)
					}
					if strings.Contains(out, `driver = "docker"`) {
						t.Errorf("rendered %s/%s unexpectedly contains docker driver\n---\n%s", kind, pool, out)
					}
				} else {
					if !strings.Contains(out, `driver = "docker"`) {
						t.Errorf("rendered %s/%s expected docker driver\n---\n%s", kind, pool, out)
					}
					if strings.Contains(out, `driver = "raw_exec"`) {
						t.Errorf("rendered %s/%s unexpectedly contains raw_exec driver\n---\n%s", kind, pool, out)
					}
				}

				// Shared dispatch contract, identical across kinds/pools.
				for _, want := range []string{
					`payload       = "required"`,
					`meta_required = ["requester"]`,
					`meta_optional = ["repo", "ref", "env_json", "timeout_seconds", "grove_meta_json", "artifact_prefix", "image"]`,
					`attempts = 0`, // reschedule/restart
					`dispatch_payload`,
					`file = "script.sh"`,
					`destination = "local/run.sh"`,
				} {
					if !strings.Contains(out, want) {
						t.Errorf("rendered %s/%s missing %q\n---\n%s", kind, pool, want, out)
					}
				}

				// kind-specific shape.
				switch kind {
				case "agent":
					if !strings.Contains(out, `kill_timeout = "10m"`) {
						t.Errorf("rendered agent/%s missing kill_timeout = \"10m\"", pool)
					}
				case "build":
					if !strings.Contains(out, "mc alias set") {
						t.Errorf("rendered build/%s missing artifact upload via mc", pool)
					}
				case "shell":
					if strings.Contains(out, "git clone --depth") {
						t.Errorf("rendered shell/%s unexpectedly clones a repo", pool)
					}
				}
			})
		}
	}
}

func TestRenderRespectsCPUAndMemoryOverrides(t *testing.T) {
	out := render(t, "build.nomad.hcl", renderData{Kind: "build", Pool: "linux", CPU: 8000, Memory: 16384})
	if !strings.Contains(out, "cpu    = 8000") {
		t.Errorf("expected overridden cpu, got:\n%s", out)
	}
	if !strings.Contains(out, "memory = 16384") {
		t.Errorf("expected overridden memory, got:\n%s", out)
	}
}

func TestRenderDefaultsCPUAndMemoryWhenZero(t *testing.T) {
	out := render(t, "shell.nomad.hcl", renderData{Kind: "shell", Pool: "macos"})
	if !strings.Contains(out, "cpu    = 2000") {
		t.Errorf("expected default cpu 2000, got:\n%s", out)
	}
	if !strings.Contains(out, "memory = 4096") {
		t.Errorf("expected default memory 4096, got:\n%s", out)
	}
}

// dockerRunMarkers maps each kind to the nested-`docker run` invocation form it uses in run.sh
// ("build" uses plain `docker run --rm`, "agent"/"shell" use `exec docker run --rm` since their
// run.sh replaces the shell process instead of capturing an exit code).
var dockerRunMarkers = map[string]string{
	"build": "docker run --rm",
	"agent": "exec docker run --rm",
	"shell": "exec docker run --rm",
}

// TestRenderDockerSocketOptOut asserts that with AllowDockerSocket left at its zero value (the
// fleet.yaml default), the rendered HCL for every kind's docker-driver ("linux") task neither
// mounts the VM's docker socket nor ever attempts a nested `docker run` — see
// internal/fleet.Pool.AllowDockerSocket and docs/JOBS.md.
func TestRenderDockerSocketOptOut(t *testing.T) {
	files := map[string]string{
		"build": "build.nomad.hcl",
		"agent": "agent.nomad.hcl",
		"shell": "shell.nomad.hcl",
	}
	for kind, file := range files {
		t.Run(kind, func(t *testing.T) {
			out := render(t, file, renderData{Kind: kind, Pool: "linux", AllowDockerSocket: false})
			if strings.Contains(out, "/var/run/docker.sock") {
				t.Errorf("rendered %s/linux with AllowDockerSocket=false unexpectedly mounts the docker socket\n---\n%s", kind, out)
			}
			if strings.Contains(out, dockerRunMarkers[kind]) {
				t.Errorf("rendered %s/linux with AllowDockerSocket=false unexpectedly attempts a nested docker run\n---\n%s", kind, out)
			}
		})
	}
}

// TestRenderDockerSocketOptIn asserts that with AllowDockerSocket true, the rendered HCL restores
// exactly the previous (pre-opt-in) behavior: the docker socket is mounted and run.sh is able to
// nest a `docker run` for a per-dispatch NOMAD_META_image override.
func TestRenderDockerSocketOptIn(t *testing.T) {
	files := map[string]string{
		"build": "build.nomad.hcl",
		"agent": "agent.nomad.hcl",
		"shell": "shell.nomad.hcl",
	}
	for kind, file := range files {
		t.Run(kind, func(t *testing.T) {
			out := render(t, file, renderData{Kind: kind, Pool: "linux", AllowDockerSocket: true})
			if !strings.Contains(out, "/var/run/docker.sock:/var/run/docker.sock") {
				t.Errorf("rendered %s/linux with AllowDockerSocket=true missing docker socket mount\n---\n%s", kind, out)
			}
			if !strings.Contains(out, dockerRunMarkers[kind]) {
				t.Errorf("rendered %s/linux with AllowDockerSocket=true missing nested docker run invocation\n---\n%s", kind, out)
			}
		})
	}
}
