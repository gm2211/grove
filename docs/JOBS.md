# grove — the job dispatch contract

This is the contract between a `JobRequest` (`internal/dispatch/dispatch.go`) and the parameterized
Nomad jobs it gets mapped onto (`nomad/jobs/*.nomad.hcl`). Read this if you're implementing
`internal/dispatch`'s `Service.Submit`, building a `grove` executor for an agent orchestrator, or
writing a script
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

## `JobRequest.timeout` wire format

`timeout` is a `dispatch.Duration` (`internal/dispatch/duration.go`), not a raw `time.Duration` —
so it is never sent or received as nanoseconds. On the wire it is one of:

- a JSON **string** parsed by Go's `time.ParseDuration`: `"30m"`, `"2h"`, `"90s"`, `"1h30m"`; or
- a JSON **number** (integer or float), interpreted as **seconds** — `120` means 120 seconds, not
  120 nanoseconds, no matter how large or "nanosecond-looking" the number is.

Anything else (bool, object, array) is a 400. A completed `Job.request.timeout` read back from
`GET /jobs/{id}` is always the string form (e.g. `"2m0s"`). `Submit` rejects `0 < timeout < 1s`
with 400 `timeout must be at least 1s (send a duration string like "30m" or a number of seconds)`
— a timeout that small is almost certainly a caller sending the wrong unit, and `run.sh`'s
`timeout $T` would otherwise kill the job before it starts.

This exists because a caller once sent `"timeout": 120000` meaning milliseconds; decoded as raw
nanoseconds that's 120µs, which `FormatFloat`'d down to `"0"` seconds of `meta["timeout_seconds"]`
and killed the job instantly (exit 124). Nobody should have to know grove's wire format uses
nanoseconds — now it can't, because it doesn't.

## `JobRequest` → dispatch meta

`Service.Submit` should map a `JobRequest` onto `nomad job dispatch` roughly as:

```go
meta := map[string]string{
    "requester": req.Requester, // required
}
if req.Repo != "" { meta["repo"] = req.Repo }
if req.Ref != ""  { meta["ref"] = req.Ref }
if t := req.Timeout.Duration(); t > 0 { meta["timeout_seconds"] = strconv.Itoa(int(t.Seconds())) }
if len(req.Env) > 0 {
    b, _ := json.Marshal(req.Env)
    meta["env_json"] = string(b)
}
if len(req.Meta) > 0 {
    b, _ := json.Marshal(req.Meta)
    meta["grove_meta_json"] = string(b) // round-tripped, not consumed by run.sh — caller's task ids etc.
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

## Per-job resource sizing

Every `nomad/jobs/*.nomad.hcl` template's `resources { cpu = ...; memory = ... }` block is rendered
from `dispatch.PoolConfig.CPU`/`Memory` (MHz / MiB), which `internal/cli/serve.go`'s `poolConfigs`
populates from `fleet.yaml`'s `pools[].jobCPU`/`jobMemory` (`fleet.Pool.JobCPUOrDefault()`/
`JobMemoryOrDefault()`, falling back to 500 MHz / 1024 MiB when unset) — see `EnsureJobs`
(`internal/dispatch/nomadjobs.go`). This sizes every `build`/`agent`/`shell` job dispatched
against that pool identically; a pool built without a `PoolConfig` at all (some tests, or a
`grove serve` that couldn't load a fleet spec) leaves every job at the templates' own hardcoded
fallback (2000 MHz / 4096 MiB).

**These are pool-level, not per-dispatch, and that's a real limitation.** `JobRequest.Resources`
(`{cpu, memory}`) *looks* like it should let one dispatch request more or less than another, but
Nomad has no dispatch-time equivalent of "override this parameterized job's `resources` block for
just this run" — a job's `resources` is fixed at *registration* time (`nomad job dispatch` only
supplies meta + payload, never a resources override), and there's no per-dispatch job update API
either. So today, `JobRequest.Resources` is **validated only**: `Service.Submit`
(`internal/dispatch/service.go`) rejects a hint that exceeds the target pool's configured CPU/
Memory defaults with a 400 (`dispatch: requested cpu=...MHz exceeds pool "..."'s job default
...`), via `Options.Pools` (the same `[]PoolConfig` `internal/cli/serve.go` also hands to
`EnsureJobs`). A hint within budget is otherwise a no-op — the dispatched job runs at the pool's
one fixed size regardless of what `Resources` says. A pool the service has no config for at all
(`Options.Pools` didn't mention it) accepts any hint without validating it, since there's nothing
to check it against.

Real per-dispatch sizing would need either per-size parameterized jobs (`grove-shell-macos-small`/
`-large`, ...) or a Nomad feature that doesn't exist yet; either is future work, not something a
`Resources` field can paper over today.

## Logs and the "no allocation yet" race

`GET /api/v1/jobs/{id}/logs` (and its NDJSON mode) has to cope with the gap between "job
submitted" and "Nomad actually placed an allocation" — `dispatch.Service.Get`/`List` show the job
as `pending` the whole time, but there is nothing to stream logs from yet. `Service.Logs`/
`LogLines` treat that gap differently depending on `follow`:

- **`follow=false`** (a one-shot `grove logs <id>`) does a single check; if there's still no
  allocation it returns `dispatch.ErrNoAllocationYet` immediately, which the HTTP handler turns
  into **`200` with an empty body and an `X-Grove-Job-Status: pending` header** — not a `502`, since
  "no output yet" is a legitimate state for a pending job. `apiclient.Client.JobLogs` retries this
  a handful of times with a short backoff before giving up and handing the (still-empty) response
  back to the caller, so `grove logs <id>` run moments after `grove dispatch` usually just works
  rather than requiring the caller to poll by hand.
- **`follow=true`** (`grove dispatch --follow` / `grove logs --follow`) instead *waits*: it polls
  Nomad roughly once a second until an allocation appears, the job reaches a terminal status, or a
  deadline passes, then streams normally. The HTTP handler bounds that wait with a context
  deadline — `?wait=<duration>` on the request (`time.ParseDuration` syntax, e.g. `?wait=2m`),
  defaulting to 10 minutes — so a job that Nomad can't place at all (no capacity, no matching pool)
  doesn't hold the connection open forever; if the deadline elapses without an allocation, the
  handler falls back to the same `200`/empty/`pending` response as the non-follow case.

This is the fix for a real bug: `grove dispatch --follow` used to 502 immediately with `job <id>
has no allocation yet` the moment the job was still `pending`, which is the common case for the
first second or two after submission (and much longer on a macOS pool waiting on a Tart VM to
boot) — every `--follow` invocation would race the scheduler and usually lose.

## What runs inside the allocation

1. Nomad places the allocation on a node whose `meta.pool` (set in the image's `grove-meta.hcl`,
   see docs/IMAGES.md) matches the job's constraint.
2. The dispatch payload (`req.Script`) is written to `${NOMAD_TASK_DIR}/script.sh`.
3. A `template`-delivered `run.sh` at `${NOMAD_TASK_DIR}/run.sh` is what the task actually execs:
   - expands `env_json` into `export`s (`jq`),
   - (build/agent) `git clone --depth 50 $repo work && cd work && git checkout $ref`, running
     `gh auth setup-git` first if `GH_TOKEN`/`GIT_TOKEN` is set (so a private HTTPS clone works),
   - runs `script.sh` under `run_with_timeout $T` (see "Portable timeout" below), where
     `T="${timeout_seconds:-3600}"`,
   - (build) uploads `./artifacts/**` via `mc` to `$ARTIFACT_ENDPOINT/$ARTIFACT_BUCKET/<artifact_prefix>/`
     using `ARTIFACT_ACCESS_KEY`/`ARTIFACT_SECRET_KEY` from the environment,
   - exits with `script.sh`'s exit code, which becomes the Nomad task's (and therefore the
     allocation's, and therefore `Job.ExitCode`'s) exit status.

### Portable timeout: macOS raw_exec hosts have no GNU `timeout(1)`

GNU coreutils' `timeout(1)` does not exist on stock macOS (no coreutils installed by default), but
every `run.sh` used to `exec timeout $T bash -eo pipefail script.sh` unconditionally. On a `macos`
pool node (`raw_exec` driver, native process, no container) that failed every dispatch with exit
127 (`…/run.sh: line N: exec: timeout: not found`) before ever running the caller's script.

Each `run.sh` now defines a `run_with_timeout` shell function instead of calling `timeout`
directly:

- If `command -v timeout` finds a real `timeout` binary (true inside the `docker`-driver `linux`
  pool's `grove-runner` image, and inside the nested `docker run` in the `AllowDockerSocket`
  branch, both of which are Linux-based and carry GNU coreutils), it's used directly — same
  behavior as before.
- Otherwise (macOS raw_exec, bash 3.2 at `/bin/bash`), the command runs in the background (under
  `set -m` so it lands in its own process group — see below) under a watchdog subshell that sleeps
  `$T`, sends `kill -TERM "-$pid"`, then polls (`ps -o stat=`, once a second, up to 10s) until the
  target is gone or a zombie before sending `kill -KILL "-$pid"` as a fallback. A marker file
  records whether the watchdog actually fired, so `run_with_timeout` can `return 124` exactly when
  it did — preserving the same contract `internal/dispatch` already relies on (124 ->
  `Job.TimedOut`) regardless of which path ran.
- The grace-period loop polls with `ps` instead of a blind `sleep 10` for a second, load-bearing
  reason beyond responsiveness: on macOS's bash 3.2, sending `kill -TERM` from this watchdog
  subshell straight into a `sleep 10` (no intervening forked process) could leave the main flow's
  `wait "$pid"` below stuck indefinitely — the target process died right away, but run.sh's own
  blocking `wait` for it never woke up, an apparent SIGCHLD-notification race specific to old
  bash's job-control implementation without a controlling terminal (as is the case for both a
  Nomad raw_exec task and this fix's own test, which runs run.sh via Go's `os/exec` — no tty).
  Forking `ps` here — a real, unrelated child process, not merely another builtin — reliably
  un-sticks it: repeated empirical testing (`nomad/jobs/nomadjobs_test.go`'s
  `TestRunSHPortableTimeout` "times out and exits 124" case, which took the full 30s instead of
  ~1s without this) never reproduced the hang once a real `ps` fork was in the path between the
  `kill -TERM` and the eventual `wait`. If this ever needs revisiting, the reproduction is: drop
  the `ps` poll for a blind `sleep 10; kill -KILL ...` and rerun that test.
- The kill targets the negative pid (`-$pid`, the process **group**), not the pid itself. `"$@"`
  is `bash -eo pipefail script.sh`, and the dispatched `script.sh` typically ends in its own
  foreground command (e.g. a `sleep`, a long-running build step); signaling only the immediate
  `bash -eo pipefail` wrapper kills that wrapper but leaves any such grandchild orphaned and
  running for its full, unbounded duration — a real bug caught by this fix's own test
  (`TestRunSHPortableTimeout`'s "times out and exits 124" case took the full 30s instead of ~1s
  before `set -m` + `-$pid` was added). `set -m` (enabling job control) is what makes `&` place
  `"$@"` in a new process group in the first place — off by default in a non-interactive script,
  so it's toggled on immediately before backgrounding and back off immediately after.
- Either path installs a `trap ... TERM INT` that forwards a signal Nomad sends the task (e.g. on
  `kill_timeout` during a cancel) to the child process, since the fallback path no longer `exec`s
  into `timeout` and therefore keeps `run.sh`'s own bash process as the one Nomad signals.
- `build.nomad.hcl`'s `run.sh` (which already captures `code=$?` under `set +e` to run the artifact
  upload afterward, rather than `exec`ing into the last command) calls `run_with_timeout` the same
  way; `shell.nomad.hcl`/`agent.nomad.hcl` call it followed by an explicit `exit $?`, since neither
  had anything left to do after the script previously.

See `nomad/jobs/nomadjobs_test.go`'s `TestRunSHPortableTimeout` for the regression test — it
extracts the real, HCL-unescaped `run.sh` body via `jobspec2` and runs it directly with
`/bin/bash`, so it exercises whichever of the two `run_with_timeout` paths matches the machine
running `go test` (the fast path if that machine happens to have `timeout` on `PATH`, the fallback
otherwise).
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

## HCL escaping in `run.sh`

Each `nomad/jobs/*.nomad.hcl` template embeds its `run.sh` as a `template { data = <<-EOF ... EOF }`
heredoc. Nomad parses the **entire** job file — including that heredoc — as HCL2, and HCL2 treats
any `${...}` anywhere in a string (heredocs included) as *its own* template interpolation, not as
bash. `run.sh` is bash, and bash's default-value syntax (`${VAR:-default}`) is not valid HCL
expression syntax, so an unescaped bash expansion like `${NOMAD_META_timeout_seconds:-3600}` fails
Nomad's parser with an error like `Invalid character` / `Template interpolation doesn't expect a
colon at this location`. This is a real bug that broke every `grove serve` startup (`EnsureJobs`
failed to register all three job kinds) before it was fixed here.

**Rule for editing `run.sh` inside any of these templates:**

- **Plain env var read, no default** (`${FOO}`) — drop the braces and write bash's bare form,
  `$FOO`, instead (valid whenever `$FOO` isn't immediately followed by another identifier
  character). HCL only treats `${` specially, so a brace-less `$FOO` passes through untouched and
  bash still expands it normally at runtime, since Nomad exports `NOMAD_TASK_DIR`,
  `NOMAD_META_<key>`, `NOMAD_ALLOC_ID`, etc. as real environment variables in the task's process.
- **Needs bash syntax that requires braces** (`${VAR:-default}`, `${VAR:+alt}`, nested defaults,
  etc.) — keep the braces but escape the leading `$` by doubling it: `$${VAR:-default}`. Nomad's
  HCL2 template parser turns a literal `$${` into a literal single `$` in the rendered output,
  which is exactly the bash syntax you want; a stray `%{` (Nomad's *directive* syntax, used for
  `for`/`if` inside templates) would need the same doubling (`%%{`) if it ever shows up in bash
  content, though none of these templates currently need it.
- **Genuine Nomad-side interpolation** — `${meta.pool}` in a `constraint` block, `${NOMAD_TASK_DIR}`
  in `config.args` (outside any heredoc) — stays exactly as `${...}`, unescaped. These are resolved
  by Nomad itself, not by bash, and are unaffected by this bug (they're not inside the `run.sh`
  heredoc).

When in doubt, prefer the plain-`$NAME` form — it sidesteps the escaping question entirely and is
what most of `run.sh` uses today.

## Testing the contract

```console
$ go test ./nomad/...                    # renders every (kind × pool × AllowDockerSocket) job,
                                          # asserts its shape, and parses it with Nomad's own
                                          # jobspec2 library (github.com/hashicorp/nomad/jobspec2)
                                          # offline — the same regression check that would have
                                          # caught the HCL-escaping bug above without needing a
                                          # live Nomad cluster.

# against a real cluster: nomad/jobs/*.nomad.hcl is a Go text/template (see the {{.Kind}}/{{.Pool}}
# placeholders), not valid HCL on its own — render it first (e.g. via the `render` helper used in
# nomadjobs_test.go, or by having grove's own EnsureJobs write it out), then:
$ nomad job validate /tmp/rendered-build-macos.hcl
$ echo 'echo hello from grove' > /tmp/script.sh
$ nomad job dispatch -meta requester=cli -payload /tmp/script.sh grove-shell-linux
$ nomad job dispatch -meta requester=cli -meta repo=https://github.com/gm2211/grove -meta ref=main \
    -payload build-script.sh grove-build-linux
```
