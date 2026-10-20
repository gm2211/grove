# grove — architecture

grove turns a handful of always-on machines you already own (Apple Silicon Macs, a Linux box) into a
private cloud for CI builds, deploys and coding-agent sessions. One CLI, one control plane, reachable
only over your Tailscale tailnet.

## The layering

```
                 ┌──────────────────────────────────────────────┐
  intent         │  agent orchestrator / Claude Code / Codex / CLI │
                 └──────────────┬───────────────────────────────┘
                                │ HTTP (grove API)  /  MCP (stdio)
                 ┌──────────────▼───────────────────────────────┐
  control plane  │  grove server  — aggregates + runs jobs        │
                 │   • fleet reconciler (desired VMs → Orchard)   │
                 │   • job runner (per-pool queue → sealed VM)    │
                 │   • job logs + artifacts on local disk         │
                 │   • embedded web UI                            │
                 └──────────────┬───────────────────────────────┘
                                │ Orchard REST (VM lifecycle + exec into guests)
  machines       ┌──────────────▼───────────────────────────────┐
                 │ Orchard controller                             │
                 └──────────────┬───────────────────────────────┘
                                │ workers dial out
                 ┌──────────────▼───────────────────────────────┐
  each Mac       │ Orchard worker  →  tart  (Softnet, host blocked)│
                 │   ├─ Linux VM   { docker }  ← one container per job
                 │   └─ macOS VM   { xcode  }  ← one job per VM, as an unprivileged user
                 └────────────────────────────────────────────────┘
```

Two systems, and the seam is deliberate:

| Layer | Owns | Never knows about |
|---|---|---|
| **Orchard** (our fork) | VM lifecycle: create, place, restart, TTL, delete; exec into a guest | jobs |
| **grove** | desired fleet state, the job queue and runner, logs, artifacts, API, UI, install | — it's the only layer that sees both |

Why not Kubernetes: there is no macOS container runtime and no upstream macOS kubelet; every
"Mac as k8s node" option is a third-party virtual-kubelet.

Why VMs and no separate job scheduler: the VM is the isolation boundary protecting the host OS,
and every VM is **sealed** (Softnet, host blocked: it reaches the public internet and nothing at
home), so nothing inside a VM can dial back to a scheduler anyway. grove used to run a Nomad client
in each guest; it now drives every job from `grove serve` itself through Orchard exec, which needs
no agent, token or network path inside the VM. Scheduling needs are small and pool-shaped: a FIFO
queue per pool, and concurrency bounded by the pool's sealed VM slots (a Linux VM packs several
containers, one slot per job; a macOS VM takes exactly one job, since Apple's
Virtualization.framework caps macOS guests at 2 per Mac and the VM is recycled after each job).

### Job runner and restarts

`internal/dispatch` keeps the job history in `jobs.json` (state dir) and each queued job's env in a
0600 spool beside it until the job starts. Each pool has a FIFO queue; one job at a time per pool
claims a VM slot (`internal/sandbox` slot files), so jobs start in submission order and as many run
at once as the pool has slots. The runner is in-process (a goroutine per running job, no child
process): cancel maps to exit 143, the job's timeout to 124, a VM that disappears to 125, and a VM
that refuses a job while draining (75) is retried on another VM before the job ever starts.

When `grove serve` restarts, running jobs are **not** re-attached: their in-process runner is gone
and a sealed VM's partially-run job can't be trusted to resume cleanly. Each such job is marked
`lost` (exit 125) with a reason, and the VM it was on is deleted so the reconciler recreates it
clean. Queued jobs whose spool survived are requeued in submission order; a queued job without its
spool (env values unrecoverable) is marked lost. Jobs recorded by a Nomad-era grove that never
finished are marked lost too.

## Machine roles

| Role | Runs | Where |
|---|---|---|
| `control-plane` | Orchard controller, grove server (API + UI + job runner), job logs and the artifact store (local directories) | the always-on Linux box, or one Mac |
| `worker` | Orchard worker (+ tart), launchd-supervised | every Mac |
| `client` | just the `grove` CLI / MCP server pointed at the control plane | your laptop |

Workers and clients dial **out** to the control plane over the tailnet. No inbound ports on any
Mac. Worker VMs are not on the tailnet at all. Tailscale standalone variant on hosts (not App
Store — that one doesn't start before login).

Control plane owns shared scheduling state; it is not a sole dispatcher. Every enrolled device
receives an independent, revocable `read + build + agent` credential and can inspect or submit
purpose-specific work. Raw shell jobs require operator credential or an explicitly issued
`dispatch:shell` scope (plain `dispatch` covers build + agent only); named secrets are rejected
until grove can resolve them.
Raw device tokens live only in that device's Keychain; control plane persists SHA-256 hashes.
Operator credential alone can enroll/revoke devices or mutate worker/VM state. Compromise of
control-plane host still compromises cluster administration, so that host remains part of trusted
computing base even when most dispatch originates elsewhere.

Worker enrollment does not keep Orchard `admin:write`. Grove's Orchard issuer has only
`service-account:issue-worker`, which Orchard limits server-side to creating `grove-worker-*`
accounts with exactly `compute:write` and `compute:connect`.

## Recycling / hygiene

Nothing long-lived is cleaned, only replaced:

1. Every worker VM has an Orchard **TTL** (fork feature) — e.g. 12h.
2. When it expires, Orchard deletes it via the normal path, which runs the VM's **ShutdownScript**
   (fork feature) *inside the guest*: it marks the VM draining (so no new job starts there) and
   waits for its running jobs to finish. grove's runner retries a job a draining VM refused on
   another VM.
3. grove's fleet reconciler notices the VM is gone and recreates it from the (freshly pulled) image.

A macOS VM is also deleted after every job it ran, and recreated the same way.

## Repository layout and ownership

```
cmd/grove/                 main.go — cobra root
internal/cli/              one file per command; each registers itself via init() on Root
internal/config/           ~/.config/grove/config.yaml  (CONTRACT — see below)
internal/orchard/          Orchard client interface + impl over github.com/cirruslabs/orchard/pkg/client (fork)
internal/fleet/            fleet.yaml spec → desired (sealed) VMs; reconciler loop
internal/dispatch/         JobRequest → per-pool queue → in-process runner; status; logs
internal/sandbox/          one job in a sealed VM through Orchard exec (clone, bundle, run, artifacts)
internal/server/           HTTP API (/api/v1/…) + embedded UI; token auth
internal/gitauth/          operator-armed credential for cloning private repos (off by default)
internal/mcp/              MCP server (stdio) exposing dispatch/status/logs/fleet tools
internal/install/          role bootstrap: brew deps, launchd plists, configs, tailscale checks
ui/                        Vite + React + TS SPA; built output embedded by internal/server/ui.go
images/                    Packer templates: linux-worker (docker), macos-worker (toolchain)
scripts/install.sh         curl | sh → brew tap + `grove install`
docs/                      INSTALL.md (written for Claude to follow), OPERATIONS.md, JOBS.md, IMAGES.md, MCP.md, ORCHESTRATOR.md
```

**Contract packages** (`config`, `orchard`, `dispatch`, `fleet` *types*) define the Go
interfaces every other package builds against. Implementations may change; the interfaces are the seam
that lets components be built in parallel. Extend interfaces additively.

## Fleet spec (`fleet.yaml`)

```yaml
pools:
  - name: linux
    image: ghcr.io/gm2211/grove-linux-worker:latest
    perWorker: 1            # VMs of this pool per online Orchard worker
    cpu: 4
    memory: 8192            # MiB
    ttl: 12h
    labels: {}               # extra Orchard *worker selectors* — see below, usually left empty
    workerSelector: {}      # only workers whose labels ⊇ this map
    network: isolated        # the only mode: Softnet, host blocked (see below)
  - name: macos
    image: ghcr.io/gm2211/grove-macos-worker:latest
    perWorker: 1
    cpu: 4
    memory: 8192
    ttl: 12h
```

Pool names are restricted to lowercase letters/digits, no dashes or dots (`fleet.Load`'s
validation) — VM names are parsed back as `<pool>-<worker>-<n>` (`fleet.ParseVMName`), and worker
names (real hostnames) routinely contain both, so the pool component must be unambiguous.

Reconciler: for each online, non-paused worker matching `workerSelector`, ensure `perWorker` VMs
named `<pool>-<worker>-<n>` exist, `restart_policy: OnFailure`, `ttl_seconds` set, startup/shutdown
scripts from the pool. A VM's pool and host are never labelled — they're derived from its name and
its observed placement (`fleet.PoolAndHost`): pool from parsing the name, host from Orchard's
`worker` field once scheduled, falling back to the pinned worker while still pending. Missing →
create; extra → delete; spec drift (image/cpu/memory/diskSize/restartPolicy/ttl/scripts/network)
→ delete + recreate.

**`network`** is always `isolated`: every pool is sealed. Its VMs run on Softnet with the host
blocked (`netSoftnet` + `netSoftnetBlock: ["out @host"]`), use public DNS and keep job containers
behind an in-guest fence: they reach the public internet and nothing at home (no LAN, no tailnet,
no host services). An older fleet.yaml that says `network: shared` (or leaves it out) still loads:
the pool is sealed anyway and `grove serve` / `grove fleet` print a warning for an explicit
`shared`; `allowDockerSocket: true` is likewise switched off with a warning. `grove serve`
(`internal/sandbox`) drives each job in a sealed VM from outside through Orchard exec: push the
checkout in, run it in a fresh container, follow its logs, poll it, pull its artifacts out. A
macOS guest has no container runtime: there the job runs as the unprivileged job user in a fresh
run directory, and the VM serves that one job only (the runner deletes it afterwards and the
reconciler recreates it from the image). Details: docs/OPERATIONS.md "Sealed pools".

**VM labels are a hard scheduler selector, not free-form tags.** Orchard's scheduler only ever
places a VM on a worker whose own labels are a *superset* of the VM's
(`internal/controller/scheduler` in the gm2211/orchard fork, `worker.Labels.Contains(vm.Labels)`).
So the VM labels grove sends are ONLY the `org.cirruslabs.orchard.worker-name` pin (which every
worker automatically satisfies for itself) plus whatever a pool's `labels:` map declares — and a
label in that map must also be present, with the same value, on any worker meant to run the pool
(`orchard worker run --labels k=v`), or that worker can never satisfy the selector and the pool's
VMs sit `pending` on it forever. This is why pool/host bookkeeping moved out of VM labels
entirely: `pool=<pool> host=<worker>` labels grove used to add had no matching worker labels by
default, so every VM stuck pending.

## Job model

A **JobRequest** is self-contained: which pool, what repo+ref to fetch, what script to run, which env
and secrets to inject, timeout (a duration string like `"30m"`/`"2h"`, or a number of *seconds* —
never nanoseconds; see `docs/JOBS.md` "JobRequest.timeout wire format"). grove queues it on its pool and
runs it in one of that pool's sealed VMs. Kinds:

- `build` — clone `repo@ref` (on the control plane), run `script` in the VM, exit code = job
  status. The control plane then copies `./artifacts/**` out of the VM into the artifact store: a
  directory on the control plane's disk (`artifacts.dir`, default
  `~/.local/state/grove/artifacts`) that only `grove serve` touches — no service, port or
  credential.
- `agent` — long-running Claude Code / Codex session against a checkout; reconnectable logs.
- `shell` — run an arbitrary command (what `grove exec` and the MCP `grove_run` tool use).

Secrets never go into images: they arrive as job env when the job starts.

A job's stdout/stderr are written as it runs to one file per job on the control plane
(`~/.local/state/grove/logs/<id>.log`, 0600, JSON lines tagged by stream), which the logs API
reads and follows. Logs expire with the job's artifacts: 15 minutes after a successful job, an
hour after any other outcome.

Cloning a **private** repository needs a credential, and grove has none until an operator arms one
through the control plane (`grove github enable`, `PUT /api/v1/github/sourcing`). While it is
armed, `grove serve` uses it for the clone it runs on the control plane when a build/agent job
whose repo URL is in scope starts; the token never enters the VM, the job's env, its log or the
job history. Disarming it, or letting its TTL lapse, wipes it. See `docs/JOBS.md` → "Private repositories".

## grove HTTP API (v1)

All under `/api/v1`, `Authorization: Bearer <token>`, bind to the tailnet address.

| Method | Path | Purpose |
|---|---|---|
| GET | `/fleet` | `{workers[], vms[], nodes[], fetchedAt, totals}` normalised: name, host, pool (vm entries only, derived — see above), arch, online, cordoned, capacity, running; `totals` is at-a-glance counts (workersOnline/Cordoned, vmsRunning, jobsRunning/Pending). `nodes` is always `[]` and `nodesReady/Draining` always 0 (kept for older clients). A VM's `raw` never includes its startup/shutdown scripts |
| POST | `/vms/{name}/recycle` | delete the VM through Orchard (its shutdown script lets running jobs finish first); reconciler recreates. 202 `{drainStarted: true}` |
| POST | `/workers/{name}/pause` · `/resume` | Orchard cordon |
| GET | `/jobs` · `/jobs/{id}` | list / status. An operator sees every job; any other credential sees only the jobs it submitted (403 on someone else's). `request.env` keeps its keys but every value reads `[redacted]`, for every caller |
| POST | `/jobs` | JobRequest → `{id}`; JobRequest.idempotencyKey replays the existing job (no re-dispatch) instead of creating a new one; JobRequest.timeout is a duration string ("30m", "2h") or a number of *seconds* (never nanoseconds) — see `docs/JOBS.md` |
| GET | `/jobs/{id}/logs?follow=1` | same ownership rule as `/jobs/{id}`. SSE / chunked log stream; read from the job's log file on the control plane; `Accept: application/x-ndjson` + `&sinceOffset=N` streams `{offset,ts,stream,line}` objects, resumable by offset |
| GET | `/jobs/{id}/artifacts/{path}` | streams one artifact from the control plane's artifact store; same ownership rule as `/jobs/{id}`. A path with a `.`/`..`/empty segment or a leading `/` (checked after URL-decoding) is a 400. Always served `Content-Disposition: attachment` + `X-Content-Type-Options: nosniff`, so a browser downloads it rather than rendering job output on grove's origin |
| DELETE | `/jobs/{id}` · POST `/jobs/{id}/cancel` | cancel (same operation, two spellings) |
| GET · POST · DELETE | `/access/devices` · `/access/devices/{id}` | operator-only: list device credentials / mint one (`{name, scopes}` → `{token, device}`; the raw token is returned only here, once) / revoke one |
| GET | `/healthz` | `{ok, version, orchard, serverTime}`; orchard is "up" or "down" |
| GET · PUT · DELETE | `/github/sourcing` | operator-only: read / arm / disarm the credential jobs use to clone PRIVATE repositories. Off by default, never echoes the token — see `docs/JOBS.md` |

The same operations are exposed as MCP tools (`grove mcp`) so Claude Code / Codex can use the fleet
directly, and consumed by an agent orchestrator's `grove` executor.

## Fork of Orchard

`github.com/gm2211/orchard` (module path unchanged: `github.com/cirruslabs/orchard`; grove uses a
`replace` directive). Adds:

- `shutdown_script` + `shutdown_script_timeout_seconds` on VMs — run inside the guest before deletion, fail-open.
- `ttl_seconds` on VMs — controller deletes VMs older than TTL.

Both are generic and intended to be offered upstream.
