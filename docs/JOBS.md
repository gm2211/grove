# grove — the job dispatch contract

This is the contract between a `JobRequest` (`internal/dispatch/dispatch.go`) and the parameterized
Nomad jobs it gets mapped onto (`nomad/jobs/*.nomad.hcl`). Read this if you're implementing
`internal/dispatch`'s `Service.Submit`, building a `grove` executor for an agent orchestrator, or
writing a script
meant to run as a grove job (CI builds, agent sessions, `grove exec`).

## The three kinds

| `JobRequest.Kind` | Nomad job | Clones `repo@ref`? | Uploads artifacts? | Notes |
|---|---|:---:|:---:|---|
| `build` | `grove-build-<pool>` | yes | sealed pools: `./artifacts/**` (see below) | CI-style: run a script, keep what it produced. On a shared pool artifacts are not collected yet: the VM has no way to write to the control plane's artifact store. |
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
// build only: the image the main task runs in. Chosen here and nowhere else: the validated
// req.Environment.Image, else the pool's runnerImage. Never copied from req.Meta (which rides in
// grove_meta_json) and always set, so the template fails closed if it is ever missing.
meta["task_image"] = taskImage
// build/agent: "grove-payload/1\n" + cloneToken + "\n" + req.Script (only the source task reads
// it); shell: req.Script. See encodePayload.
payload := encodePayload(req.Kind, cloneToken, req.Script)
client.Dispatch(ctx, "grove-"+string(req.Kind)+"-"+req.Pool, meta, payload)
```

`req.Secrets` names operator-set secrets (`internal/secrets`, `grove secret set`). The API checks
each name against the secret's grants before `Submit` (an operator may name any secret; anyone
else only one whose `allow` list holds its principal id or one of its Tailscale tags, and a missing
secret and an ungranted one get the same 403). `Submit` then resolves the values and folds them
into `env_json`, each exported under its own name, so a rotated secret reaches the next job with
no re-registration. A name that is also an `env` key, a name given twice, or a name that isn't a
shell variable name is a 400. The values exist only in that dispatch: the stored job and every
`Job` the API returns keep the names, never the values. Like every `env` value, they are visible
to anyone who can read the dispatched job's meta in Nomad, so keep Nomad's ACLs on.

`env` values go to Nomad with the dispatch and nowhere else: the stored job, and every `Job` the
API returns, keeps each key with the value `[redacted]`.

`req.Credentials` are real tokens the submitter hands grove for this one job, which the job never
sees (see docs/OPERATIONS.md "The credential broker"):

```json
"credentials": [
  {"kind": "github",    "env": "GH_TOKEN",                "token": "ghs_…"},
  {"kind": "anthropic", "env": "CLAUDE_CODE_OAUTH_TOKEN", "token": "sk-ant-oat01-…"}
]
```

`Submit` mints a placeholder (`gph1_…`) for each and puts the placeholder in `env_json` under
`env`. The token stays in the control plane's memory, where the broker on the worker the job lands
on pulls it. `kind` is `github`, `anthropic` or `openai` and fixes the hosts the token can reach.
The stored job and the API keep `kind` and `env` only. Credentials are refused on a sealed pool,
whose VMs can't reach a broker, and a job with credentials never falls back onto one. An unknown
kind, an `env` that is also an `env` key or named secret, a name given twice, more than 8, or a
token that isn't printable ASCII without spaces is a 400.

Tokens expire, and a control-plane restart forgets them. The submitter puts fresh ones behind the
same placeholders with `PUT /api/v1/jobs/{id}/credentials` (the submitting device or an
operator; 404 for an unknown job, 409 once it has ended):

```json
{"credentials": [{"env": "GH_TOKEN", "token": "ghs_…new"}]}
```

## Falling back to macOS

Most Linux build scripts run on macOS too, so a job a shared Linux pool can't place moves to the
shared `macos` pool instead of waiting:

- **When:** 20 seconds after submit when Nomad reports no node of the pool at all (every node it
  evaluated was filtered out by `meta.pool`), or after 3 minutes unplaced when the pool's nodes are
  all busy. A job moves at most once.
- **How:** grove reads the dispatched job's meta and payload back from Nomad, dispatches them to
  `grove-<kind>-macos`, then stops and purges the Linux copy, so exactly one copy is ever queued.
  The job keeps its id; `job.fallback` says `{from, to, at, reason}`. On macOS it runs as a plain
  process of the `_grovejob` user, not in the runner container.
- **Opting out:** `"pinPool": true` on the JobRequest keeps a job in its pool (use it for scripts
  that need Linux: docker, apt-get, ELF binaries). A job with an `environment` (see "Job
  environments") falls back only when it carries a `toolchain` and the fallback pool is a macOS
  pool with `toolchains` enabled (see "Native toolchain on macOS"); an image-only environment
  never falls back, since macOS has no containers to run its image in. `pinPool` always wins. A
  job with `resources` also never falls back to a pool whose per-job size (`jobCPU`/`jobMemory`) is
  smaller than the hint. Per pool, fleet.yaml `fallback: none` turns it off, or `fallback: <pool>` picks
  another shared pool. Sealed (`network: isolated`) pools never fall back or take fallback jobs.
- **Learning what doesn't work there:** jobs are grouped by kind, pool and repo. When the last 3
  fallback runs of a group all failed (timeouts and cancels don't count), that group stops falling
  back for 7 days after the last failure, and its jobs wait for their own pool, with
  `pendingReason` saying why. A success among those runs keeps it on. The verdict is computed from
  the job history, so it survives restarts and expires on its own.

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
one fixed size regardless of what `Resources` says. (Fallback does check it: a job doesn't move to
a pool whose per-job size is smaller than its hint.) A pool the service has no config for at all
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

## Private repositories

A `build`/`agent` job clones `repo@ref` with whatever credentials the allocation has, which by
default is none: **public repositories only**. Cloning a private one requires an operator to arm a
GitHub credential on the control plane first. It is off on a fresh install, there is no
config.yaml key or environment variable that turns it on, and nothing is armed by simply having a
token on the machine.

```
# arm it (the token comes from $GH_TOKEN, or pipe it in with --token-stdin — never argv)
grove github enable --repo gm2211/grove --allow tag:argos --ttl 4h
grove github status
grove github disable          # wipes the stored token
```

Same thing from the UI (Settings → "Private repository sourcing") or the API:

```
GET    /api/v1/github/sourcing   → {enabled, hosts, repos, allow, tokenFingerprint, expiresAt, …}
PUT    /api/v1/github/sourcing   ← {token, repos, hosts?, allow?, ttl?}
DELETE /api/v1/github/sourcing
```

`ttl` defaults to 24h and may be at most 720h: an armed credential always expires. `allow` is who,
besides an operator, may submit a job that clones with it.

All three need the **operator** credential, reads included — the status names the repositories the
control plane is currently willing to clone privately.

While it is armed, `internal/dispatch`'s `Submit` asks the store about each build/agent job's
`repo` and submitter and, when both are in scope, puts the token in that dispatch's payload, which
only the job's `source` prestart task receives. What is in scope:

- **submitters** — an operator, or a principal on the `allow` list: a device credential's id, a
  Tailscale identity's `tailscale:<stable id>`, or a Tailscale tag (`tag:argos`), the syntax named
  secrets use. An empty list is operators only. The submitter is the principal the API
  authenticated, never anything in the request body. A job from anyone else is dispatched with no
  token and fails at `git clone` like any private repository with nothing armed; the response is
  the same, so it says nothing about what is armed. A dispatch credential is deliberately not
  enough: every worker Mac has one, and a worker Mac can read the token out of a VM it hosts.
  (A job moved to a fallback pool or dispatched again after an eviction reuses the payload its
  first dispatch was given; the question is asked once, at submit.)
- **hosts** — `github.com` unless you passed `--host` (for GitHub Enterprise).
- **repos** — exactly the ones you passed with `--repo` (required); `owner/*` covers one owner.
  A credential armed by an older grove with no repos named covers nothing until re-armed.
- **`https://` URLs only.** A token does nothing for `git@github.com:owner/repo.git`, so an SSH
  remote is never given one. Use the HTTPS clone URL for private repos.
- **the caller's own token wins.** A job that already sets `GH_TOKEN` or `GIT_TOKEN` in its `env`
  keeps its value, and the source task clones with it; the armed credential only fills a gap.

Where the token does and doesn't go:

- It goes into that one job's Nomad **dispatch payload**, and into the control plane's own
  `github.json` (mode 0600, beside `devices.json`). Never `env_json` or any dispatch meta: Nomad
  exports those to every task, including the one running the caller's script.
- Inside the allocation only the `source` prestart task gets the payload. It closes its own task
  directory (0700), hands the token to git through its environment and an inline credential
  helper (never argv, never a config file, never the checkout's `.git/config`), and deletes the
  payload before `main` starts. `main` runs the caller's script with no path to it; on macOS it
  also runs as the unprivileged `_grovejob` account, so root-owned files stay out of reach.
- It is never written to the job history, never part of `GET /jobs/{id}` (the job carries only
  `repoCredentialUsed: true`), and never returned by the sourcing API, which reports a SHA-256
  `tokenFingerprint` instead.
- **Caveat:** the dispatched job spec still holds the payload, so anything that can read Nomad's
  job API can read the token while the job exists. Keep Nomad ACLs on (the installer's default; workers' client APIs
  bind 127.0.0.1 and enforce ACLs, so an anonymous request from inside a job is refused). Scope the token
  narrowly (a fine-grained, read-only token limited to the repos you named), keep `--ttl` short,
  `--allow` only the orchestrator that needs it, and disable it when the run is done. A GitHub App
  installation token minted per job would shrink this further; grove doesn't do that yet.

`--ttl` is the reason this is "on demand" rather than a switch you forget: the credential
auto-disarms and is wiped from disk the first time anything touches the store after it lapses.
With no TTL it stays armed until `grove github disable`.

## What runs inside the allocation

1. Nomad places the allocation on a node whose `meta.pool` (set in the image's `grove-meta.hcl`,
   see docs/IMAGES.md) matches the job's constraint.
2. shell: the dispatch payload (`req.Script`) is written to `${NOMAD_TASK_DIR}/script.sh`.
   build/agent: a `source` prestart task receives the payload instead, writes the script to
   `${NOMAD_ALLOC_DIR}/data/grove/script.sh`, runs `git clone --depth 50 -- $repo` into
   `${NOMAD_ALLOC_DIR}/data/grove/work` and `git checkout $ref` there (with the clone token, if
   any, as described under "Private repositories"), deletes the payload, and records its log and
   exit code for `main`.
3. A `template`-delivered `run.sh` at `${NOMAD_TASK_DIR}/run.sh` is what the `main` task execs:
   - (build/agent) replays the source task's log and exits with its code if it failed,
   - expands `env_json` into `export`s (`jq`; keys that aren't shell variable names are skipped),
   - (build/agent) `cd`s into the checkout,
   - runs `script.sh` under `run_with_timeout $T` (see "Portable timeout" below), where
     `T="${timeout_seconds:-3600}"`,
   - exits with `script.sh`'s exit code, which becomes the Nomad task's (and therefore the
     allocation's, and therefore `Job.ExitCode`'s) exit status.

### Sealed pools: the job runs from the control plane

A pool with `network: isolated` registers every kind from `nomad/jobs/sandbox.nomad.hcl` instead.
Its VMs run no Nomad client (they can't reach home), so the allocation is a single raw_exec task
`main` on the control plane's own Nomad client (constraint `${meta.grove_sandbox_host} = "true"`),
running `grove sandbox run` (`internal/sandbox`). The meta and payload contract above is
unchanged; what differs is where each step happens:

- the payload (with any clone token) is read and deleted by `grove sandbox run`, which fetches the
  repository on the control plane with the same rules and exit codes as the `source` task, but
  with `--no-checkout`: no file of the repository is written there;
- that `.git`, `script.sh`, an `env.sh` built from `env_json` (minus `GH_TOKEN`/`GIT_TOKEN`) and a
  `run.sh` wrapper are copied into a fresh container (`docker create --init`, the pool's runner
  image, `--cpus`/`--memory` from the pool's job sizing) in one of the pool's sealed VMs, through
  Orchard exec; the container checks out `ref` in `/workspace` (exit 1 if it can't), then runs
  `script.sh` there under `timeout $T`;
- its stdout and stderr stream into the task's own (`docker logs --follow --timestamps`, resumed
  without repeats if the stream drops), so `GET /jobs/{id}/logs` reads the same;
- the task exits with the container's exit code; 124 on a timeout; 125 when the VM stopped
  answering for a minute, which `internal/dispatch` reports as `lost` for a sealed pool (a script's
  own 125 is reported as 1);
- (build) `./artifacts/**` are copied out by the control plane and stored under
  `<artifact_prefix>/` in its artifact store, a directory on its own disk (`artifacts.dir` in
  `config.yaml`, default `~/.local/state/grove/artifacts`; files 0600, written by atomic rename).
  Only regular files are taken, at paths that stay inside `artifacts/`; at most 2 GiB per job.

A sealed **macOS** pool (`--os darwin`) has no container runtime, so the same steps run as plain
processes, through `sudo` over the same Orchard exec:

- the bundle is unpacked, as the job user `_grovejob`, into a fresh run directory
  `/private/var/grove-job/runs/<alloc>` (mode 0700): `workspace/` (the `.git`; `run.sh` checks out
  `ref` there), `grove/` (`run.sh`, `script.sh`, `env.sh`) and `tmp/` (the job's `TMPDIR`);
- a detached root launcher starts `run.sh` as `_grovejob` in its own process group, with an
  empty environment plus `HOME`, `USER`, `LOGNAME`, `TMPDIR`, the image's `PATH` and the caller's
  `env.sh`; the job's stdout, stderr and exit code go to a root-only directory
  `/private/var/grove-sandbox/<alloc>` that the job can't write;
- logs are followed with `tail -c +<offset> -f`, resumed at the exact byte if the stream drops;
- the job's timeout is kept by `run.sh` itself, on macOS and Linux alike (no `timeout(1)`: macOS
  has none, and uutils' — Ubuntu 26.04's default — exits 124 or dies leaving the job running when
  it is TERMed): the script runs in its own process group, which gets TERM at the limit and KILL
  10 seconds later, and the job reports 124. `grove sandbox run` keeps a backstop timer too: past
  the timeout (plus a minute) it sends TERM to the job's process group, then KILL to it and to every
  process of the job user. A cancel TERMs the job once and reports the job's own exit status, as
  `docker stop` would;
- `./artifacts` is read back as a tar, as the job user;
- the job's CPU/memory sizing and runner image don't apply: the job has the whole VM. Once it's
  over, grove deletes the run directory and then the VM itself, which the fleet reconciler
  recreates from the image, so no job ever runs in a VM another job has used.

Exit codes are the same as on a Linux sealed pool (124, 125 → lost, a script's own 125 → 1).

### When a sealed pool's VM dies: reruns, quarantine, sick machines

Jobs waiting for a VM slot of a pool start in the order they were submitted. Each waiting
`grove sandbox run` holds a ticket in `<state>/grove/sandbox-queue/<pool>/`, and a job takes a
free slot only when fewer jobs are waiting ahead of it than there are free slots.

When a job's VM stops answering, `grove sandbox run` reruns the job in another VM, on another Mac
when the pool has one. A job runs at most 3 times. Only a lost VM starts a rerun: a script that
fails, times out or is canceled is never rerun. `JobRequest.retryLost` decides; when it isn't set,
build and shell jobs are rerun and agent jobs are not, since an agent may already have pushed or
posted something.

A job lost on 2 different Macs, after its script started, is a poison pill: it most likely takes
the machines down itself. grove stops running it, and it ends `failed` with `quarantined: true`
and a `failureReason` that names the Macs. A VM lost before the job's script started says nothing
about the job, so it doesn't count.

The driver writes each job's runs to `<state>/grove/attempts/<job id>.json` on the control
plane. The job itself never runs there. `grove serve` folds the record into `Job.attempts` and
`Job.placement`, then deletes it once the job has ended. `grove serve` also watches the attempts
of the last hour:

- a VM that lost 2 different jobs, with none finishing in it in between, is deleted, and the
  fleet reconciler rebuilds it;
- a Mac whose VMs lost 3 different jobs, in at least 2 of its VMs and with none finishing in
  between, has its scheduling paused, so no new job goes to it until an operator runs `grove
  worker resume`.

Quarantined jobs are left out of both counts, and so are `evicted` attempts: when a Mac is snoozed
with `grove worker snooze --now`, the driver stops the job in that Mac's VM and reruns it in
another machine's, recording the first attempt as `evicted`. It is not a loss, so it never counts
toward quarantine, the 3 runs or the health rules above.

A job that cannot start because of a snooze is queued, not failed. Its `pendingReason` says so
and `queuedUntil` is when the snoozed worker is due back. The queue belongs to the pool: any other
matching worker takes the job as soon as it is free, and the 30 minute pending timeout counts
from `queuedUntil`. A shared-pool job moved off a snoozed worker by `--now` lists the moves in
`evictions` (`worker`, `at`, `reason`). Reruns and quarantine cover sealed pools only; a
shared pool's job that is lost stays lost.

### Artifact retention

Artifacts are kept briefly, so the artifact store never grows without bound: `grove serve` deletes a
job's `<artifact_prefix>/` **15 minutes** after the job succeeded and **1 hour** after any other end
(failed, canceled, lost), counting from the later of the job's finish and its last upload. Files
under `jobs/` that belong to no job grove knows about expire 1 hour after their last write. Once
deleted, the job's `artifacts` list is empty and `artifactsExpiredAt` says when. Both durations
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

### Portable timeout: macOS raw_exec hosts have no GNU `timeout(1)`

GNU coreutils' `timeout(1)` does not exist on stock macOS (no coreutils installed by default), but
every `run.sh` used to `exec timeout $T bash -eo pipefail script.sh` unconditionally. On a `macos`
pool node (`raw_exec` driver, native process, no container) that failed every dispatch with exit
127 (`…/run.sh: line N: exec: timeout: not found`) before ever running the caller's script.

Each `run.sh` now defines a `run_with_timeout` shell function instead of calling `timeout`
directly:

- If `command -v timeout` finds a real `timeout` binary (true inside the `docker`-driver `linux`
  pool's `grove-runner` image, which is Linux-based and carries GNU coreutils), it's used
  directly — same behavior as before.
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
- `build.nomad.hcl`'s `run.sh` (which captures `code=$?` under `set +e` and exits with it, rather
  than `exec`ing into the last command) calls `run_with_timeout` the same
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

## Job environments

A `build` job can name the image its script runs in, so a project brings its own toolchain
instead of the pool's `grove-runner`:

```json
{"kind": "build", "pool": "linux", "repo": "https://github.com/acme/app", "script": "make test",
 "environment": {"image": "ghcr.io/acme/builder@sha256:<64 hex>"}}
```

The caller only *requests* an image; the control plane decides. `Submit` returns 400 unless all of
these hold:

- The reference is `<registry>/<repository>@sha256:<64 lowercase hex>`: an explicit registry host
  (`ghcr.io/…`, `host:5000/…`, `localhost/…`), a lowercase OCI path, a digest, at most 512 bytes. A
  tag can be repointed after review; a digest cannot.
- **A tag is accepted, but the control plane pins it.** `ghcr.io/acme/builder:v1` is accepted when
  the pool allows `ghcr.io/acme/builder` (see below) and the control plane can resolve the tag: at
  submit it asks the registry for the tag's digest (an OCI distribution `HEAD
  /v2/<repo>/manifests/<tag>`, authenticated with the credential armed by `grove registry pull
  enable` when that covers the repository, anonymous otherwise) and runs the job as
  `ghcr.io/acme/builder@sha256:…`. The job record, dispatch meta (`task_image`) and the Nomad job all
  carry only that digest, so a tag moved after submit changes nothing for this job, and a retry
  runs the same bytes. If the registry can't be reached, refuses the credential or has no such
  tag, `Submit` returns 400 naming the image; it never falls back to the tag. A reference with no
  tag at all (`ghcr.io/acme/builder`) is refused too. A macOS toolchain job ignores `image`, so it
  is not resolved.
- The job's pool lists the repository in fleet.yaml `environments.allow`:

  ```yaml
  pools:
    - name: linux
      environments:
        allow:
          - ghcr.io/acme/            # a prefix: ends in "/", each path below it
          - registry.example.com/team/builder   # an exact repository
  ```

  Entries need an explicit registry host and no tag or digest; a prefix matches at the `/`
  (`ghcr.io/acme/` does not allow `ghcr.io/acme-evil/x`). A pool with no `environments` block
  accepts none, and `grove serve` refuses the block on the `macos` pool and on sealed pools.
- The kind is `build` (agent and shell jobs have no `environment` yet) and the pool is a shared
  Linux pool.

**Image contract.** The image needs `/bin/bash`, coreutils and `jq` (`run.sh` checks for `jq` first
and fails with a clear message). It does not need `git`: cloning is a separate step. grove runs
it with `entrypoint = ["/bin/bash"]` (the image's own ENTRYPOINT never runs), as `root` in the
container whatever its `USER` says, with a 15-minute `image_pull_timeout`; Nomad's image GC removes
a pulled image a day after its last use (`images/linux-worker/files/client.hcl`).

**How it is delivered.** build.nomad.hcl's `main` task has `image = "${NOMAD_META_task_image}"`.
(Nomad does interpolate `NOMAD_META_*` in the docker `image` field; [hashicorp/nomad#6247], once
cited here as open, is closed. `TestTaskImageInterpolationRealNomad` proves it against a real
agent.) grove sets meta `task_image` on **every** build dispatch: the validated environment image,
else the pool's `runnerImage`. With it missing the task fails to start, never runs a guessed image.
A caller's `Meta["image"]` is no longer forwarded anywhere; it only round-trips in
`grove_meta_json`. A redispatch (eviction, fallback) carries the meta unchanged; one for a build
older than `task_image` gets its pool's runner image, and an environment job missing it fails.

**Security.** The `source` prestart task, which holds the private-repo clone token, always runs in
the pool's trusted `runnerImage`, never in the environment image; the environment image runs only
the caller's script, which never sees that token (see "Private repositories"). The allowlist is
the trust decision: an allowed repository is code the operator vouches for, pinned to a digest so
the vouching can't be changed under them. Jobs still get no docker socket (`allowDockerSocket` is
refused), so an environment image gains nothing over the runner image except its contents.

The feature is advertised as `features: ["environment", "toolchain"]` in `/healthz`, so a client
can tell a server that would silently ignore a field from one that enforces it.

### Native toolchain on macOS

macOS has no containers, so an image can't be the environment there. An `environment` can instead
carry a `toolchain`: exact versions of native tools, installed on the macOS worker by a dedicated
user and cached by the hash of the spec.

```json
{"kind": "build", "pool": "linux", "repo": "https://github.com/acme/app", "script": "cargo test",
 "environment": {"image": "ghcr.io/acme/builder@sha256:<64 hex>",
   "toolchain": {"rust": {"version": "1.99.1", "components": ["clippy", "rustfmt"]},
                 "node": {"version": "22.11.0"}, "python": {"version": "3.12.7"}}}}
```

`Submit` returns 400 unless the toolchain passes these checks:

- The tools are a fixed set: `rust`, `node`, `python`. At least one; any other key is refused.
- Versions are exact: rust `1.x.y`, node `x.y.z`, python `3.x.y`. `"22"`, `"latest"`, `"nightly"`,
  ranges and prefixes like `v22.11.0` are refused, so the same spec always installs the same bytes.
- rust `components` are a subset of `clippy`, `rustfmt`, `rust-src`, `llvm-tools`.
- The canonical encoding (below) is at most 1 KiB.

grove does not forward the caller's JSON. It re-marshals the validated spec (keys sorted, components
sorted and unique, no whitespace) and sets meta `toolchain_json` to that, and `toolchain_hash` to the
hex sha256 of `"grove-toolchain/1\n"` followed by it. `Submit` is the only place either is set; a
caller's `Meta` never reaches them. A redispatch carries both unchanged and refuses to send a
toolchain job that lost them.

Where such a job can run:

- **A Linux pool:** as above, with the image required and validated (the image is what runs there).
  Because it carries a toolchain it may fall back to macOS (see "Falling back to macOS"), even
  though an image-only environment may not.
- **A macOS pool, submitted directly:** only a shared macOS pool with `toolchains: {enabled: true}`
  in fleet.yaml, and only an environment with a toolchain. `image` is optional and ignored; the
  job's `task_image` is the pool's runner image. `fleet.Load` accepts `toolchains` only on the
  shared `macos` pool, never on a Linux or sealed pool, and still refuses `environments` there.
- **Falling back to macOS:** the target must be a macOS pool with toolchains enabled and its per-job
  size must cover the job's `resources`. A pool without toolchains, a sealed pool or a smaller pool
  is never a target.

On the worker the installer runs as `_grovetool` from root-owned code and writes the cache; the job
runs as `_grovejob`, which can read the cache but not write it (spec S22).

[hashicorp/nomad#6247]: https://github.com/hashicorp/nomad/issues/6247

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
$ go test ./nomad/...                    # renders every (kind × pool) job,
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
