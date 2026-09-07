# grove — the job dispatch contract

This is the contract between a `JobRequest` (`internal/dispatch/dispatch.go`) and the parameterized
Nomad jobs it gets mapped onto (`nomad/jobs/*.nomad.hcl`). Read this if you're implementing
`internal/dispatch`'s `Service.Submit`, building Argos's `grove` executor, or writing a script
meant to run as a grove job (CI builds, agent sessions, `grove exec`).

## The three kinds

| `JobRequest.Kind` | Nomad job | Clones `repo@ref`? | Uploads artifacts? | Notes |
|---|---|:---:|:---:|---|
| `build` | `grove-build-<pool>` | yes | yes, `./artifacts/**` | CI-style: run a script, keep what it produced. |
| `agent` | `grove-agent-<pool>` | yes | no | Long-running Claude Code / Codex session; `kill_timeout = "10m"` so a cancel lets it shut down cleanly instead of SIGKILL. |
| `shell` | `grove-shell-<pool>` | no | no | Arbitrary command against whatever's already on disk — what `grove exec` and the MCP `grove_run` tool use. |

`<pool>` is whatever `Pool` values exist in `fleet.yaml` (`linux`, `macos`, and any future pool);
the server registers one job per (kind, pool) pair at startup by rendering
`nomad/jobs/*.nomad.hcl` (see `nomad/jobs/README.md`).

## `JobRequest` → dispatch meta

`Service.Submit` should map a `JobRequest` onto `nomad job dispatch` roughly as:

```go
meta := map[string]string{
    "requester": req.Requester, // required
}
if req.Repo != "" { meta["repo"] = req.Repo }
if req.Ref != ""  { meta["ref"] = req.Ref }
if req.Timeout > 0 { meta["timeout_seconds"] = strconv.Itoa(int(req.Timeout.Seconds())) }
if len(req.Env) > 0 {
    b, _ := json.Marshal(req.Env)
    meta["env_json"] = string(b)
}
if len(req.Meta) > 0 {
    b, _ := json.Marshal(req.Meta)
    meta["grove_meta_json"] = string(b) // round-tripped, not consumed by run.sh — Argos bead ids etc.
}
// artifact_prefix: server-assigned, e.g. the Job.ID, so build artifacts land at a predictable
// path grove can later present as Artifact.URL.
meta["artifact_prefix"] = jobID
payload := []byte(req.Script)
client.Dispatch(ctx, "grove-"+string(req.Kind)+"-"+req.Pool, meta, payload)
```

`req.Secrets` (names to resolve server-side) are **not** part of this mapping — resolve them to
values and fold them into `env_json` before dispatch, so `run.sh` never has to know secrets exist
as a separate concept from ordinary env vars. Nothing about secret handling lives in the job
template; keeping that logic in `internal/dispatch` means changing how secrets are resolved never
requires re-registering jobs.

## What runs inside the allocation

1. Nomad places the allocation on a node whose `meta.pool` (set in the image's `grove-meta.hcl`,
   see docs/IMAGES.md) matches the job's constraint.
2. The dispatch payload (`req.Script`) is written to `${NOMAD_TASK_DIR}/script.sh`.
3. A `template`-delivered `run.sh` at `${NOMAD_TASK_DIR}/run.sh` is what the task actually execs:
   - expands `env_json` into `export`s (`jq`),
   - (build/agent) `git clone --depth 50 $repo work && cd work && git checkout $ref`, running
     `gh auth setup-git` first if `GH_TOKEN`/`GIT_TOKEN` is set (so a private HTTPS clone works),
   - runs `script.sh` under `timeout ${timeout_seconds:-3600}`,
   - (build) uploads `./artifacts/**` via `mc` to `$ARTIFACT_ENDPOINT/$ARTIFACT_BUCKET/<artifact_prefix>/`
     using `ARTIFACT_ACCESS_KEY`/`ARTIFACT_SECRET_KEY` from the environment,
   - exits with `script.sh`'s exit code, which becomes the Nomad task's (and therefore the
     allocation's, and therefore `Job.ExitCode`'s) exit status.
4. `internal/dispatch` polls (or streams via `Logs`) the allocation the same way regardless of
   pool — the pool-specific driver difference (`raw_exec` on macOS, `docker` on linux) is fully
   contained inside the job template and invisible to callers.

## Why the image isn't just `image = "${NOMAD_META_image}"`

`meta_optional` includes an `image` field so a `build`/`agent`/`shell` job can request a different
container than the default `ghcr.io/gm2211/grove-runner:latest` — e.g. a job that needs a specific
language toolchain version. The obvious implementation would be
`config { image = "${NOMAD_META_image}" }` in the docker driver's task config. That does not work:
Nomad's docker driver does not support interpolating the `image` field from dispatch-time/task
meta ([hashicorp/nomad#6247](https://github.com/hashicorp/nomad/issues/6247) — filed 2019, still
open as of writing); `${NOMAD_META_x}`-style interpolation is only reliable in fields Nomad
documents as "interpretable" for that driver, such as `args` and `env`
(see [Runtime Variable Interpolation](https://developer.hashicorp.com/nomad/docs/reference/runtime-variable-interpolation) —
constraints only see node attributes/meta, since runtime env vars don't exist until after
placement, which is a separate but related interpolation limitation).

grove's chosen fix: the docker task's own `image` is *always* the fixed `grove-runner` image, with
the host's docker socket bind-mounted in (`volumes = ["/var/run/docker.sock:/var/run/docker.sock"]`,
which needs `docker.volumes.enabled = true` in the client's plugin config — see
`images/linux-worker/files/client.hcl`). `images/runner/Dockerfile` installs the Docker **CLI**
(not a daemon) for exactly this reason: when `NOMAD_META_image` is set and differs from the
default, `run.sh` nests a `docker run --rm -v "$PWD":/workspace -w /workspace ... "$NOMAD_META_image"
timeout ... bash -eo pipefail <script>` instead of running the script directly. This is real,
functioning per-dispatch image selection — it just happens one level down from where a first
glance at the Nomad job spec would suggest. The `macos`/`raw_exec` pool has no docker daemon at
all, so it ignores `image` entirely and always runs the script as a native process — there's
nothing to select an image for a `raw_exec` task in the first place.

**Not independently verified**: the docker-image-interpolation limitation is confirmed via the
linked upstream issue and Nomad's own interpolation docs, but the nested-`docker run` fallback
itself has not been exercised against a live Nomad cluster (no server available in this
environment) — see `nomad/jobs/README.md`'s testing section for what *was* verified
(template rendering via `go test ./nomad/...`).

### The socket mount is opt-in per pool, off by default

The docker-socket-mount-plus-nested-`docker run` mechanism described above is **not** unconditional
— it's gated behind `fleet.yaml`'s `pools[].allowDockerSocket` (default `false`, see
`internal/fleet.Pool.AllowDockerSocket`), threaded through `dispatch.PoolConfig` into each job
template's `{{.AllowDockerSocket}}`.

- When `allowDockerSocket` is unset or `false` (the default): the docker task's `config` block
  never mounts `/var/run/docker.sock`, and `run.sh` never attempts a nested `docker run`.
  `NOMAD_META_image` is still accepted (it stays in every template's `meta_optional` list for
  dispatch-contract symmetry across pools) but it is silently ignored — every job on the pool runs
  in the fixed `grove-runner` image.
- When `allowDockerSocket` is `true`: behavior is exactly as described above — the socket is
  mounted and `run.sh` nests a `docker run` whenever `NOMAD_META_image` differs from the default.

**Why default to off**: the docker socket gives a container root-equivalent control of the VM
host it's running on, and it lets any job on the pool reach every other job's containers through
the same daemon. On a shared multi-tenant pool that's a real isolation hole — one job's
`docker run` can inspect, exec into, or kill any other job's container, or escape to the host
entirely. That's a meaningful security/blast-radius trade-off in exchange for per-dispatch image
selection, so grove requires an operator to opt a pool into it explicitly rather than enabling it
silently for everyone.

## Testing the contract

```console
$ go test ./nomad/...                    # renders every (kind × pool) job and asserts the shape

# against a real cluster:
$ echo 'echo hello from grove' > /tmp/script.sh
$ nomad job dispatch -meta requester=cli -payload /tmp/script.sh grove-shell-linux
$ nomad job dispatch -meta requester=cli -meta repo=https://github.com/gm2211/grove -meta ref=main \
    -payload build-script.sh grove-build-linux
```
