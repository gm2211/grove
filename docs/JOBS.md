# grove — the job contract

This is the contract for a `JobRequest` (`internal/dispatch/dispatch.go`): what grove does with
one from submission to exit code. Read this if you're working on `internal/dispatch` or
`internal/sandbox`, building a `grove` executor for an agent orchestrator, or writing a script
meant to run as a grove job (CI builds, agent sessions, `grove exec`).

## The three kinds

| `JobRequest.Kind` | Clones `repo@ref`? | Collects artifacts? | Notes |
|---|:---:|:---:|---|
| `build` | yes | `./artifacts/**` (see below) | CI-style: run a script, keep what it produced. |
| `agent` | yes | no | Long-running Claude Code / Codex session; a cancel gives it 8 minutes to shut down cleanly before it is killed. |
| `shell` | no | no | Arbitrary command — what `grove exec` and the MCP `grove_run` tool use. |

`pool` is any pool name in `fleet.yaml` (`linux`, `macos`, and any future pool); `Submit` refuses a
pool `grove serve` doesn't know (400 `dispatch: unknown pool …`).

## How a job runs

There is no external scheduler: `grove serve` (`internal/dispatch`) runs every job itself.

1. **Submit** validates the request, assigns an id and records the job in `jobs.json` as
   `pending`. `env` values are written only to a 0600 queue spool (`queue/<id>.json` beside
   `jobs.json`); the stored job, and every `Job` the API returns, keeps each key with the value
   `[redacted]`.
2. **Queue.** Each pool has a FIFO queue. The job at the head claims a free slot in one of the
   pool's sealed VMs (a Linux VM has as many slots as fit its CPU and memory at the pool's job
   sizing; a macOS VM has one). Jobs start in submission order; one pool never waits on another.
   While queued, `pendingReason` says whether the job waits for a slot or behind other jobs.
3. **Start.** The spool entry is deleted, the clone credential (if any) is resolved, and
   `internal/sandbox` runs the job in the VM — see "Inside the sealed VM" below. The job turns
   `running` and records its VM (`placement`) once the VM accepted it.
4. **End.** The job's exit code becomes `Job.ExitCode`, with these set by grove:
   - **124** — the job's `timeout` (default 1h) ran out (`timedOut: true`).
   - **125** → `lost` — the VM stopped answering, no sealed VM of the pool came up in time, or
     `grove serve` restarted while the job ran. A script's own 125 is reported as 1.
   - **143** → `canceled` — `DELETE /jobs/{id}` / `POST /jobs/{id}/cancel`.
   - **75** before the job started (its VM was draining) is not an outcome: the job goes back to
     the head of its queue, up to 3 times.

`req.Secrets` (names to resolve server-side) is reserved: nothing resolves names yet, so `Submit`
rejects a request that sends any rather than running a job that silently lacks them.

### Restarts

Jobs run inside the `grove serve` process. When it restarts, a job that was running is marked
`lost` ("grove serve restarted while the job was running; its sealed VM is recycled") and its VM
is deleted, so the reconciler brings a clean one back; grove does not re-attach to it. Queued jobs
keep their place and start again in submission order. A queued job whose spool is missing (its env
values can't be recovered) and any job a Nomad-based grove left unfinished are marked `lost`.

## `JobRequest.timeout` wire format

`timeout` is a `dispatch.Duration` (`internal/dispatch/duration.go`), not a raw `time.Duration` —
so it is never sent or received as nanoseconds. On the wire it is one of:

- a JSON **string** parsed by Go's `time.ParseDuration`: `"30m"`, `"2h"`, `"90s"`, `"1h30m"`; or
- a JSON **number** (integer or float), interpreted as **seconds** — `120` means 120 seconds, not
  120 nanoseconds, no matter how large or "nanosecond-looking" the number is.

Anything else (bool, object, array) is a 400. A completed `Job.request.timeout` read back from
`GET /jobs/{id}` is always the string form (e.g. `"2m0s"`). `Submit` rejects `0 < timeout < 1s`
with 400 `timeout must be at least 1s (send a duration string like "30m" or a number of seconds)`
— a timeout that small is almost certainly a caller sending the wrong unit, and the job's
`timeout $T` would otherwise kill it before it starts.

This exists because a caller once sent `"timeout": 120000` meaning milliseconds; decoded as raw
nanoseconds that's 120µs, which rounded down to a timeout of `0` seconds and killed the job
instantly (exit 124). Nobody should have to know grove's wire format uses
nanoseconds — now it can't, because it doesn't.

## Per-job resource sizing

A pool's job size comes from `fleet.yaml`'s `pools[].jobCPU`/`jobMemory` (`fleet.Pool.
JobCPUOrDefault()`/`JobMemoryOrDefault()`, 500 MHz / 1024 MiB when unset), which
`internal/cli/serve.go`'s `poolConfigs` turns into `dispatch.PoolConfig.CPU`/`Memory`. On a Linux
pool every job's container gets `--cpus`/`--memory` from it, and it decides how many jobs fit in
one VM at once. A macOS job always has its whole VM.

**These are pool-level, not per-dispatch.** `JobRequest.Resources` (`{cpu, memory}`) is
**validated only**: `Service.Submit` rejects a hint that exceeds the target pool's job size with a
400 (`dispatch: requested cpu=...MHz exceeds pool "..."'s job default ...`). A hint within budget
is otherwise a no-op. Real per-dispatch sizing is future work.

## Logs

A job's stdout and stderr are written, as they arrive, to one file on the control plane:
`~/.local/state/grove/logs/<id>.log` (0600), one JSON record per line (`{"t": time, "s":
"stdout"|"stderr", "l": line}`), so the order of lines across both streams is kept. Lines longer
than 64 KiB are split. `GET /api/v1/jobs/{id}/logs` (and its NDJSON mode) reads that file:

- **A queued job has no log yet.** `follow=false` (a one-shot `grove logs <id>`) returns
  `dispatch.ErrNoAllocationYet`, which the HTTP handler turns into **`200` with an empty body and
  an `X-Grove-Job-Status: pending` header**; `apiclient.Client.JobLogs` retries this a few times
  with a short backoff. `follow=true` (`grove dispatch --follow` / `grove logs --follow`) waits for
  the job to start, bounded by `?wait=<duration>` (default 10 minutes), then streams.
- **A running job** is followed until it ends, and every line it wrote is delivered before the
  stream closes.
- **A finished job** streams its whole log, or nothing once the log has expired (see "Artifact
  and log retention").

## Private repositories

A `build`/`agent` job's clone of `repo@ref` runs with no credentials by default: **public repositories only**. Cloning a private one requires an operator to arm a
GitHub credential on the control plane first. It is off on a fresh install, there is no
config.yaml key or environment variable that turns it on, and nothing is armed by simply having a
token on the machine.

```
# arm it (the token comes from $GH_TOKEN, or pipe it in with --token-stdin — never argv)
grove github enable --repo gm2211/grove --ttl 4h
grove github status
grove github disable          # wipes the stored token
```

Same thing from the UI (Settings → "Private repository sourcing") or the API:

```
GET    /api/v1/github/sourcing   → {enabled, hosts, repos, tokenFingerprint, expiresAt, …}
PUT    /api/v1/github/sourcing   ← {token, repos?, hosts?, ttl?}
DELETE /api/v1/github/sourcing
```

All three need the **operator** credential, reads included — the status names the repositories the
control plane is currently willing to clone privately.

While it is armed, `internal/dispatch` asks the store about each build/agent job's `repo` when the
job **starts** (not when it is submitted, so arming or disarming affects queued jobs too) and,
when it is in scope, hands the token to the clone the control plane runs for that job. What is in
scope:

- **hosts** — `github.com` unless you passed `--host` (for GitHub Enterprise).
- **repos** — exactly the ones you passed with `--repo` (required); `owner/*` covers one owner.
  A credential armed by an older grove with no repos named covers nothing until re-armed.
- **`https://` URLs only.** A token does nothing for `git@github.com:owner/repo.git`, so an SSH
  remote is never given one. Use the HTTPS clone URL for private repos.
- **the caller's own token wins.** A job that already sets `GH_TOKEN` or `GIT_TOKEN` in its `env`
  keeps its value, and the clone uses it; the armed credential only fills a gap.

Where the token does and doesn't go:

- It lives in the control plane's own `github.json` (mode 0600, beside `devices.json`) and, for
  one job, in `dispatch.Run.CloneToken`, which `internal/sandbox` hands to `git` on the control
  plane through its environment and an inline credential helper (never argv, never a config file,
  never the checkout's `.git/config`).
- It never enters the VM: the checkout is copied in without it, and `GH_TOKEN`/`GIT_TOKEN` are
  dropped from the job's env there too. The caller's script has no path to it.
- It is never written to the job history, the queue spool or the job's log, never part of
  `GET /jobs/{id}` (the job carries only `repoCredentialUsed: true`), and never returned by the
  sourcing API, which reports a SHA-256 `tokenFingerprint` instead.
- Scope the token narrowly anyway (a fine-grained, read-only token limited to the repos you
  named), give it a `--ttl`, and disable it when the run is done.

`--ttl` is the reason this is "on demand" rather than a switch you forget: the credential
auto-disarms and is wiped from disk the first time anything touches the store after it lapses.
With no TTL it stays armed until `grove github disable`.

## Inside the sealed VM

Every pool is sealed (`network: isolated`; see docs/OPERATIONS.md "Sealed pools"): its VMs run no
agent and can't reach home, so `internal/sandbox`, inside `grove serve`, drives the job from the
control plane through Orchard exec:

- the repository is fetched on the control plane (with the clone token, if any) with
  `--no-checkout`: no file of the repository is written there; a clone that fails ends the job
  with git's error before any VM is used;
- that `.git`, `script.sh`, an `env.sh` built from the job's env (minus `GH_TOKEN`/`GIT_TOKEN`) and a
  `run.sh` wrapper are copied into a fresh container (`docker create --init`, the pool's runner
  image, `--cpus`/`--memory` from the pool's job sizing) in one of the pool's sealed VMs, through
  Orchard exec; the container checks out `ref` in `/workspace` (exit 1 if it can't), then runs
  `script.sh` there under `timeout $T`;
- its stdout and stderr stream into the job's log file (`docker logs --follow --timestamps`,
  resumed without repeats if the stream drops);
- the job ends with the container's exit code; 124 on a timeout; 125 when the VM stopped
  answering for a minute, reported as `lost` (a script's own 125 is reported as 1);
- (build) `./artifacts/**` are copied out by the control plane and stored under
  `<artifact_prefix>/` in its artifact store, a directory on its own disk (`artifacts.dir` in
  `config.yaml`, default `~/.local/state/grove/artifacts`; files 0600, written by atomic rename).
  Only regular files are taken, at paths that stay inside `artifacts/`; at most 2 GiB per job.

A **macOS** pool has no container runtime, so the same steps run as plain
processes, through `sudo` over the same Orchard exec:

- the bundle is unpacked, as the job user `_grovejob`, into a fresh run directory
  `/private/var/grove-job/runs/<job>` (mode 0700): `workspace/` (the `.git`; `run.sh` checks out
  `ref` there), `grove/` (`run.sh`, `script.sh`, `env.sh`) and `tmp/` (the job's `TMPDIR`);
- a detached root launcher starts `run.sh` as `_grovejob` in its own process group, with an
  empty environment plus `HOME`, `USER`, `LOGNAME`, `TMPDIR`, the image's `PATH` and the caller's
  `env.sh`; the job's stdout, stderr and exit code go to a root-only directory
  `/private/var/grove-sandbox/<job>` that the job can't write;
- logs are followed with `tail -c +<offset> -f`, resumed at the exact byte if the stream drops;
- macOS has no `timeout(1)`, so the job's timeout is enforced from the control plane: past
  the timeout (plus a minute) it sends TERM to the job's process group, then KILL to it and to every
  process of the job user, and the job reports 124; a cancel does the same and reports the job's
  own exit status, as `docker stop` would;
- `./artifacts` is read back as a tar, as the job user;
- the job's CPU/memory sizing and runner image don't apply: the job has the whole VM. Once it's
  over, grove deletes the run directory and then the VM itself, which the fleet reconciler
  recreates from the image, so no job ever runs in a VM another job has used.

Exit codes are the same as on a Linux pool (124, 125 → lost, a script's own 125 → 1).

The job's runner image is fixed per pool: `pools[].runnerImage` in `fleet.yaml`, default
`ghcr.io/gm2211/grove-runner:latest`. A per-job `image` override is ignored, and no job ever gets
the VM's Docker socket (`allowDockerSocket` in an older fleet.yaml is switched off with a warning).

## Artifact and log retention

Artifacts are kept briefly, so the artifact store never grows without bound: `grove serve` deletes a
job's `<artifact_prefix>/` **15 minutes** after the job succeeded and **1 hour** after any other end
(failed, canceled, lost), counting from the later of the job's finish and its last upload. Files
under `jobs/` that belong to no job grove knows about expire 1 hour after their last write. Once
deleted, the job's `artifacts` list is empty and `artifactsExpiredAt` says when. A job's log
file follows the same rule (15 minutes after a success, 1 hour after any other end, counted from
the job's finish); once it is gone the logs endpoints return an empty stream and `logsExpiredAt`
says when. Both durations
are set in `config.yaml`:

```yaml
artifacts:
  dir: /home/you/.local/state/grove/artifacts   # where the store lives (set by grove install)
  retention:
    success: 15m
    failure: 1h
```

Older `config.yaml` files may still carry `endpoint`, `bucket`, `accessKey` and `secretKey` from
when artifacts lived in MinIO; they still load, are ignored (`grove serve` logs a warning), and
re-running `grove install --role control-plane` removes them.

The sweep runs with the job reconciler (every 30 seconds), so a deletion can land up to that much
after its deadline. Download what you need to keep.
