package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// dockerBackend runs a job in a fresh container in a Linux VM: `docker create`, `docker cp -`,
// `docker start`, `docker logs`, `docker inspect`, `docker stop`, `docker rm`.
type dockerBackend struct{}

// containerPaths are the job's paths inside its container: the bundle is extracted at its root.
var containerPaths = jobPaths{workspace: "/workspace", grove: "/grove"}

func (dockerBackend) paths() jobPaths { return containerPaths }

func (dockerBackend) place() string { return "its container" }

// create makes the job's container without starting it, pulling the image if the VM lacks it.
func (dockerBackend) create(ctx context.Context, j *job) int {
	args := []string{"docker", "create", "--init", "--name", j.name,
		"--label", "grove.alloc=" + j.req.AllocID,
		"--label", "grove.pool=" + j.req.Pool,
		"--label", "grove.kind=" + j.req.Kind,
		"--workdir", "/workspace", "--entrypoint", "/bin/bash"}
	if j.req.CPU > 0 {
		// Job CPU is in MHz at 2000 MHz per core.
		args = append(args, "--cpus", strconv.FormatFloat(float64(j.req.CPU)/2000, 'f', 2, 64))
	}
	if j.req.Memory > 0 {
		args = append(args, "--memory", strconv.Itoa(j.req.Memory)+"m")
	}
	args = append(args, j.req.Image, "/grove/run.sh")

	// A VM whose shutdown script has started (fleet's sealed shutdown script) takes no new job.
	cmd := fmt.Sprintf("if [ -e %s ]; then exit %d; fi; %s", drainingMarker, drainingExit, shellJoin(args))
	var errBuf bytes.Buffer
	code, err := j.exec(ctx, createTimeout, cmd, nil, nil, io.MultiWriter(j.r.stderr(), &errBuf))
	if err != nil {
		j.r.say("could not create the job's container in %s: %v", j.vm, err)
		return ExitLost
	}
	if code == drainingExit {
		return exitDraining
	}
	if code != 0 {
		if strings.Contains(errBuf.String(), "Cannot connect to the Docker daemon") {
			j.r.say("docker isn't running in %s", j.vm)
			return ExitLost
		}
		j.r.say("could not create the job's container (docker create exited %d)", code)
		return ExitSetup
	}
	return 0
}

func (dockerBackend) push(j *job) step {
	return step{cmd: shellJoin([]string{"docker", "cp", "-", j.name + ":/"}), tool: "docker cp"}
}

func (dockerBackend) start(j *job) step {
	return step{cmd: shellJoin([]string{"docker", "start", j.name}), tool: "docker start"}
}

func (dockerBackend) newLogs(j *job) jobLogs {
	return &dockerLogs{name: j.name, stdout: newLogStream(j.r.Stdout), stderr: newLogStream(j.r.stderr())}
}

func (dockerBackend) inspect(j *job) step {
	return step{
		cmd:  shellJoin([]string{"docker", "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", j.name}),
		tool: "docker inspect",
	}
}

func (dockerBackend) stop(j *job, grace time.Duration) step {
	secs := strconv.Itoa(max(int(grace/time.Second), 1))
	return step{cmd: shellJoin([]string{"docker", "stop", "-t", secs, j.name}), tool: "docker stop"}
}

func (dockerBackend) artifacts(j *job) step {
	return step{cmd: shellJoin([]string{"docker", "cp", j.name + ":/workspace/artifacts", "-"}), tool: "docker cp"}
}

func (dockerBackend) noArtifacts(stderr string) bool {
	return strings.Contains(stderr, "Could not find the file")
}

// remove deletes the container and everything in it.
func (dockerBackend) remove(j *job) {
	if _, err := j.exec(context.Background(), commandTimeout, shellJoin([]string{"docker", "rm", "--force", j.name}), nil, nil, nil); err != nil {
		j.r.say("could not remove the job's container from %s: %v", j.vm, err)
	}
}

// dockerLogs follows `docker logs --timestamps`, resuming with --since (see logStream).
type dockerLogs struct {
	name           string
	stdout, stderr *logStream
}

func (l *dockerLogs) streams() (io.Writer, io.Writer) { return l.stdout, l.stderr }

func (l *dockerLogs) command(follow bool) string {
	args := []string{"docker", "logs"}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, "--timestamps")
	if s := since(l.stdout, l.stderr); s != "" {
		args = append(args, "--since", s)
	}
	return shellJoin(append(args, l.name))
}

func (l *dockerLogs) reset() {
	l.stdout.reset()
	l.stderr.reset()
}

func (l *dockerLogs) flush() {
	l.stdout.flush()
	l.stderr.flush()
}
