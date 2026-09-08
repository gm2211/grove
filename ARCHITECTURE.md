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
  control plane  │  grove server  — aggregates + dispatches       │
                 │   • fleet reconciler (desired VMs → Orchard)   │
                 │   • job dispatch   (JobRequest → Nomad)        │
                 │   • embedded web UI                            │
                 └───────┬──────────────────────────┬───────────┘
                         │ Orchard REST              │ Nomad HTTP
  machines       ┌───────▼────────┐          ┌──────▼─────────┐
                 │ Orchard        │          │ Nomad server    │
                 │ controller     │          │ (job scheduler) │
                 └───────┬────────┘          └──────▲─────────┘
                         │ workers dial out          │ clients dial out
                 ┌───────▼──────────────────────────┴───────────┐
  each Mac       │ Orchard worker  →  tart                        │
                 │   ├─ Linux VM   { nomad client, docker }  ← jobs packed as containers
                 │   └─ macOS VM   { nomad client, xcode  }  ← jobs packed as processes
                 └────────────────────────────────────────────────┘
```

Three separate responsibilities, three separate systems, and the seams are deliberate:

| Layer | Owns | Never knows about |
|---|---|---|
| **Orchard** (our fork) | VM lifecycle: create, place, restart, TTL, delete | jobs, Nomad |
| **Nomad** | job scheduling *inside* VMs: placement, packing, queueing, retries, logs | VMs, Tart, Orchard |
| **grove** | desired fleet state, job submission API, UI, install | — it's the only layer that sees both |

Why not Kubernetes: there is no macOS container runtime and no upstream macOS kubelet; every
"Mac as k8s node" option is a third-party virtual-kubelet. Nomad's client runs natively in both Linux
and macOS guests, so one job scheduler covers the whole fleet.

Why VMs *and* a job scheduler: the VM is the isolation boundary protecting the host OS; the Nomad client
inside it packs many jobs per VM. One VM per job would cap macOS at 2 concurrent jobs per Mac
(Apple's Virtualization.framework limit on macOS guests — Linux guests are uncapped).

## Machine roles

| Role | Runs | Where |
|---|---|---|
| `control-plane` | Orchard controller, Nomad server, grove server (API + UI), MinIO (artifacts) | the always-on Linux box, or one Mac |
| `worker` | Orchard worker (+ tart), launchd-supervised | every Mac |
| `client` | just the `grove` CLI / MCP server pointed at the control plane | your laptop |

Everything dials **out** to the control plane over the tailnet. No inbound ports on any Mac.
Tailscale standalone variant (not App Store — that one doesn't start before login).

## Recycling / hygiene

Nothing long-lived is cleaned, only replaced:

1. Every worker VM has an Orchard **TTL** (fork feature) — e.g. 12h.
2. When it expires, Orchard deletes it via the normal path, which runs the VM's **ShutdownScript**
   (fork feature) *inside the guest*: `nomad node drain -self -enable -deadline 2h` then wait for zero
   allocations. The guest drains itself; Orchard never learns Nomad exists.
3. grove's fleet reconciler notices the VM is gone and recreates it from the (freshly pulled) image.

Tailscale inside guests uses **ephemeral tagged auth keys**, so recycled VMs vanish from the tailnet.

## Repository layout and ownership

```
cmd/grove/                 main.go — cobra root
internal/cli/              one file per command; each registers itself via init() on Root
internal/config/           ~/.config/grove/config.yaml  (CONTRACT — see below)
internal/orchard/          Orchard client interface + impl over github.com/cirruslabs/orchard/pkg/client (fork)
internal/nomad/            Nomad client interface + impl over github.com/hashicorp/nomad/api
internal/fleet/            fleet.yaml spec → desired VMs; reconciler loop
internal/dispatch/         JobRequest → parameterized Nomad job; status; logs
internal/server/           HTTP API (/api/v1/…) + embedded UI; token auth
internal/mcp/              MCP server (stdio) exposing dispatch/status/logs/fleet tools
internal/install/          role bootstrap: brew deps, launchd plists, configs, tailscale checks
ui/                        Vite + React + TS SPA; built output embedded by internal/server/ui.go
images/                    Packer templates: linux-worker, macos-worker (nomad + tailscale + tools)
nomad/jobs/                build.nomad.hcl, agent.nomad.hcl, shell.nomad.hcl (parameterized)
scripts/install.sh         curl | sh → brew tap + `grove install`
docs/                      INSTALL.md (written for Claude to follow), OPERATIONS.md, JOBS.md, IMAGES.md, MCP.md, ORCHESTRATOR.md
```

**Contract packages** (`config`, `orchard`, `nomad`, `dispatch`, `fleet` *types*) define the Go
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
    allowDockerSocket: false # opt-in only; mounts the VM's docker socket into jobs (see docs/JOBS.md)
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
create; extra → delete; spec drift (image/cpu/memory/diskSize/restartPolicy/ttl/scripts) → delete +
recreate.

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
never nanoseconds; see `docs/JOBS.md` "JobRequest.timeout wire format"). grove maps it onto a
**parameterized Nomad job** (`nomad job dispatch`)
constrained to `meta.pool`. Kinds:

- `build` — clone `repo@ref`, run `script`, upload `./artifacts/**` to MinIO, exit code = job status.
- `agent` — long-running Claude Code / Codex session against a checkout; reconnectable logs.
- `shell` — run an arbitrary command (what `grove exec` and the MCP `grove_run` tool use).

Secrets never go into images: they arrive as Nomad `template`/env at dispatch time.

## grove HTTP API (v1)

All under `/api/v1`, `Authorization: Bearer <token>`, bind to the tailnet address.

| Method | Path | Purpose |
|---|---|---|
| GET | `/fleet` | `{workers[], vms[], nodes[], fetchedAt, totals}` normalised: name, host, pool (vm entries only, derived — see above), arch, online, cordoned, capacity, running; `totals` is at-a-glance counts (workersOnline/Cordoned, vmsRunning, nodesReady/Draining, jobsRunning/Pending) |
| POST | `/vms/{name}/recycle` | drain (via Nomad) then delete; reconciler recreates |
| POST | `/workers/{name}/pause` · `/resume` | Orchard cordon |
| GET | `/jobs` · `/jobs/{id}` | list / status |
| POST | `/jobs` | JobRequest → `{id}`; JobRequest.idempotencyKey replays the existing job (no re-dispatch) instead of creating a new one; JobRequest.timeout is a duration string ("30m", "2h") or a number of *seconds* (never nanoseconds) — see `docs/JOBS.md` |
| GET | `/jobs/{id}/logs?follow=1` | SSE / chunked log stream; `Accept: application/x-ndjson` + `&sinceOffset=N` streams `{offset,ts,stream,line}` objects, resumable by offset |
| GET | `/jobs/{id}/artifacts/{path}` | streams one uploaded artifact from the bucket via the server's own credentials |
| DELETE | `/jobs/{id}` · POST `/jobs/{id}/cancel` | cancel (same operation, two spellings) |
| GET | `/healthz` | `{ok, version, orchard, nomad, serverTime}`; orchard/nomad are "up" or "down" |

The same operations are exposed as MCP tools (`grove mcp`) so Claude Code / Codex can use the fleet
directly, and consumed by an agent orchestrator's `grove` executor.

## Fork of Orchard

`github.com/gm2211/orchard` (module path unchanged: `github.com/cirruslabs/orchard`; grove uses a
`replace` directive). Adds:

- `shutdown_script` + `shutdown_script_timeout_seconds` on VMs — run inside the guest before deletion, fail-open.
- `ttl_seconds` on VMs — controller deletes VMs older than TTL.

Both are generic and intended to be offered upstream.
