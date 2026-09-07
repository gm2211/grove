# nomad/jobs

Go `text/template`'d Nomad job specs for the three job kinds grove dispatches (see
`internal/dispatch`): `build.nomad.hcl`, `agent.nomad.hcl`, `shell.nomad.hcl`. They're embedded at
build time (`embed.go`, package `nomadjobs`) and rendered + registered by the grove server, once
per (kind, pool) combination present in `fleet.yaml`, as `grove-<kind>-<pool>` — e.g.
`grove-build-linux`, `grove-shell-macos`.

## Rendering

Each template expects a data value with at least:

```go
type Data struct {
    Kind   string // "build" | "agent" | "shell" — must match the file being rendered
    Pool   string // "linux" | "macos" (or any future pool name)
    CPU    int    // MHz; 0 uses the template default (2000)
    Memory int    // MiB; 0 uses the template default (4096)
}
```

In practice `grove serve` never renders with `CPU`/`Memory` left at 0: `internal/dispatch.EnsureJobs`
is called with a `[]PoolConfig` built from `fleet.yaml`'s `pools[].jobCPU`/`jobMemory`
(`internal/cli/serve.go`'s `poolConfigs`, via `fleet.Pool.JobCPUOrDefault()`/`JobMemoryOrDefault()`),
which defaults to 500 MHz / 1024 MiB rather than 0 — so this template's own 2000/4096 fallback only
fires for a caller that builds a bare `PoolConfig{Name: ...}` directly (some tests, or a future
caller that hasn't been taught about pool job sizing). See `docs/JOBS.md`'s "Per-job resource
sizing" for the full picture, including why a `JobRequest.Resources` hint is validated but not
actually applied per-dispatch.

```go
tmpl, _ := template.New("build.nomad.hcl").ParseFS(nomadjobs.FS, "build.nomad.hcl")
var buf bytes.Buffer
tmpl.ExecuteTemplate(&buf, "build.nomad.hcl", Data{Kind: "build", Pool: "linux"})
nomadClient.RegisterJobFile(ctx, buf.String())
```

See `nomadjobs_test.go` for a full render-and-assert example across every (kind, pool) pair.

## The dispatch contract (fixed — shared with `internal/dispatch`)

Every rendered job:

- `type = "batch"`.
- `parameterized { payload = "required" meta_required = ["requester"] meta_optional = ["repo",
  "ref", "env_json", "timeout_seconds", "grove_meta_json", "artifact_prefix", "image"] }` — a
  `JobRequest` (see `internal/dispatch/dispatch.go`) maps onto these dispatch meta keys 1:1, plus
  the request's `Script` becomes the dispatch payload.
- `constraint { attribute = "${meta.pool}" value = "<Pool>" }` so the job only ever lands on a
  node whose Nomad client meta (`client.hcl`/`grove-meta.hcl` in `images/*-worker`) says
  `pool = "<Pool>"`.
- One task group, one task, named `main`.
- `restart { attempts = 0 }` and `reschedule { attempts = 0 }` on every job — grove owns retries
  at the `JobRequest` level (a caller that wants a retry submits a new request); Nomad must not
  also retry underneath it or the two retry policies fight each other.
- The dispatched payload (the caller's script) is written to `${NOMAD_TASK_DIR}/script.sh` via
  `dispatch_payload { file = "script.sh" }`.
- A `template` stanza writes an entrypoint wrapper to `local/run.sh` (`${NOMAD_TASK_DIR}/run.sh`
  at runtime). It is plain bash — no consul-template `{{ }}` directives — because Nomad already
  injects `NOMAD_META_*` and `NOMAD_TASK_DIR` as real environment variables for every task
  regardless of driver, so there's nothing left for consul-template to fill in. run.sh:
  1. Expands `NOMAD_META_env_json` (a JSON object) into `export`s via `jq`.
  2. (build/agent only) clones `NOMAD_META_repo` at depth 50 into `./work`, checks out
     `NOMAD_META_ref`, and runs `gh auth setup-git` first when `GH_TOKEN`/`GIT_TOKEN` is present
     (private HTTPS clones).
  3. Runs `script.sh` under `timeout ${NOMAD_META_timeout_seconds:-3600}`.
  4. (build only) uploads `./artifacts/**` to the artifact store with `mc`, prefixed by
     `NOMAD_META_artifact_prefix` (falls back to the Nomad alloc ID).
  5. Exits with `script.sh`'s exit code — the task's (and therefore the Nomad allocation's, and
     therefore the `Job.ExitCode` grove reports) exit status.
- Driver: `raw_exec` for the `macos` pool (no container runtime on macOS guests), `docker` for
  every other pool, defaulting the image to `ghcr.io/gm2211/grove-runner:latest`.

### Why the image isn't just `image = "${NOMAD_META_image}"`

Nomad's docker driver does not support interpolating its `image` config field from dispatch-time
meta ([hashicorp/nomad#6247](https://github.com/hashicorp/nomad/issues/6247) — confirmed still
open); `${NOMAD_META_x}` only resolves reliably in fields like `args`/`env`, not `image`. So the
outer container is always `grove-runner`, mounted with the host's docker socket
(`/var/run/docker.sock`, requires `docker.volumes.enabled = true` on the client — set in
`images/linux-worker/files/client.hcl`), and `run.sh` nests a `docker run "$NOMAD_META_image" ...`
only when a dispatch actually requested a different image than the default. `images/runner`
ships the docker CLI (no daemon) specifically for this. The `macos`/`raw_exec` pool ignores the
`image` meta entirely — there's no docker binary there to nest with.

### kind differences

| Kind    | Clone repo@ref | Artifact upload | `kill_timeout` |
|---------|:---:|:---:|---|
| `build` | yes | yes | default (5s) |
| `agent` | yes | no  | `10m` — give a long-running coding-agent session time to shut down cleanly on cancel |
| `shell` | no  | no  | default (5s) |

## Testing without a Nomad server

`nomad job validate` needs a running server to resolve datacenter/region defaults against, which
isn't available in CI or a plain checkout. Instead, `nomadjobs_test.go` renders every
(kind × pool) combination through `text/template` directly and asserts the output contains the
expected job name, the `${meta.pool}` constraint, the right driver, and the shared contract
fields. Run it with:

```console
$ go test ./nomad/...
```

To manually exercise a rendered job against a real cluster once one exists:

```console
$ nomad job run <(go run ./cmd/render-job build linux)   # however grove ends up registering jobs
$ nomad job dispatch -meta requester=cli -payload script.sh grove-shell-linux
```

(`-payload` takes a path to a local file whose contents become `${NOMAD_TASK_DIR}/script.sh` in
the dispatched allocation.)
