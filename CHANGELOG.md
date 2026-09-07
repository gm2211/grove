# Changelog

All notable changes to grove are documented here.

## v0.1.1

Fixes found by running the whole stack live (single-host smoke stack: `orchard dev --synthetic`,
`nomad agent -dev`, `grove serve`) after v0.1.0:

- `grove serve` now runs the fleet reconciler loop, so VMs deleted by Orchard on TTL expiry are
  recreated (`--fleet-reconcile-interval`, `--no-fleet-reconcile`; `GET /api/v1/fleet/reconcile`).
- `JobRequest.timeout` accepts a duration string (`"30m"`) or a number of seconds — never
  nanoseconds. Sub-second timeouts are rejected.
- Log streaming: fixed a race in the Nomad client that dropped buffered log frames; `--follow`
  waits for the allocation, streams, and exits with the job's exit code; follow on a finished job
  returns immediately instead of hanging.
- Jobs whose Nomad job vanished, or that never got an allocation within 30m, are marked `lost`
  with a reason; pending jobs surface Nomad's blocked-evaluation reason.
- Orchard VM labels are scheduler selectors: pool/host are derived from the VM name and worker;
  drift is detected field-by-field. Without this every VM stayed `pending`.
- `run.sh` uses a portable timeout (macOS has no GNU `timeout`); macOS Nomad clients get
  `cpu_total_compute` set (Apple Silicon fingerprints ~24 MHz otherwise).
- Job resources come from `fleet.yaml` pool defaults (`jobCPU`/`jobMemory`); docker-socket
  mounting is opt-in per pool (`allowDockerSocket`).
- UI streams logs via fetch + NDJSON with the bearer header (EventSource sent the token in the URL).
- Install: `--roles` passed per value, Tart from `openai/tools`; grove is its own Homebrew tap.

## v0.1.0

First usable cut of grove: turn Macs (and a Linux box) you already own into a private CI/agent
cloud, reachable only over Tailscale.

**What works today**

- Fleet reconciler that keeps Orchard VMs matching `fleet.yaml`, including per-VM TTL and
  shutdown-hook support.
- Parameterized Nomad jobs for `build` / `agent` / `shell` kinds — dispatch, status, and log
  streaming (plain, SSE, and NDJSON) end to end.
- Job reconciliation against Nomad detects orphaned and stuck jobs: a dispatched Nomad job that no
  longer exists (e.g. after a Nomad server restart with a wiped data dir) or one that's sat pending
  longer than a configurable timeout is now marked `lost` with an explanatory `failureReason`,
  instead of staying `pending` forever. A background sweep (`grove serve`, every 30s by default)
  reconciles every non-terminal job so this doesn't depend on something polling `Get`.
- Pending-placement diagnostics: a pending job's status now surfaces *why* Nomad hasn't placed it
  yet (constraint filtered, resources exhausted, node class filtered, or no nodes available),
  derived from Nomad's own blocked-evaluation metrics — in `grove jobs get`, the HTTP API, and the
  job detail page in the UI.
- HTTP API (`/api/v1/...`) with token auth, plus an embedded React/Vite control-plane UI (fleet
  view, job list, job detail with live logs).
- MCP server (stdio) so coding agents can dispatch and follow grove jobs directly.
- `grove install` role bootstrap planner (`worker` / `control-plane` / `client`) and `grove doctor`
  health checks.
- Argos integration — see [docs/ARGOS.md](docs/ARGOS.md).

**Not yet**

- Only live-tested on a single-host smoke stack — not yet run against a real multi-Mac fleet.
- Tart VM images aren't published to `ghcr.io` yet; build your own from `images/`.
- Per-dispatch CPU/memory sizing (`JobRequest.Resources`) is validated against pool defaults but
  not yet applied to the running job's actual resource allocation — see
  [docs/JOBS.md](docs/JOBS.md) "Per-job resource sizing".
