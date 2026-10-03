# Changelog

All notable changes to grove are documented here.

## Unreleased

- **Security:** job history no longer keeps `env` values: the stored job (and every job the API
  returns) lists each key with the value `[redacted]`. `secrets` is rejected until grove can
  resolve named secrets, instead of being accepted and silently ignored.
- **Security:** the private-repo clone token no longer reaches a job's script. build and agent
  jobs clone in a separate `source` prestart task that alone receives the token (in the dispatch
  payload, never `env_json`), passes it to git only through its own environment, and deletes it
  before `main` starts. A failed clone still shows in the job's logs and exit code. Scripts that
  relied on run.sh's `gh auth setup-git` must set up git auth themselves.
- **Security:** macOS jobs no longer run as root. The fleet startup script creates a hidden,
  passwordless `_grovejob` account (home `/private/var/grove-job`) and the job's `main` task runs
  as it.
- **Security:** `grove github enable` and `PUT /api/v1/github/sourcing` require at least one
  repository; a credential armed earlier with no repositories named clones nothing until it is
  re-armed. Only `https://` repo URLs are accepted for build/agent jobs, refs may not start with
  `-`, and env keys must be shell variable names.
- **Security:** `grove install` no longer installs anything it downloaded unverified. Nomad
  (Linux, now pinned to 2.0.7 instead of "latest"), MinIO (Linux, `RELEASE.2025-09-06T17-38-46Z`),
  the MinIO client and Homebrew's installer (pinned commit instead of `HEAD`) are checked against
  SHA-256 digests embedded in grove and refused on mismatch. `scripts/install.sh` verifies the
  Linux grove tarball against the release's `checksums.txt` (`GROVE_VERSION` pins a release) and
  pins Homebrew's installer the same way. See docs/INSTALL.md "Verified downloads".
- **Security:** the control plane's artifact credential in `config.yaml` is now a MinIO user
  limited to the `grove` bucket (`grove-artifacts` policy), not MinIO's root credential. Re-running
  `grove install --role control-plane` migrates an existing install and reloads `grove serve`.
- **Security:** `grove setup` shows the control plane it discovered and asks for confirmation, or
  a choice when several answer, instead of joining the first peer on :6130. Unattended runs need
  `--server URL`, or `--yes` when exactly one control plane answers.
- `fleet.yaml` pools take `network: isolated`: VMs run on Softnet with their host Mac blocked,
  use public DNS, fence job containers off private and tailnet addresses, and join the tailnet
  under their own tag (`tailscale.tags`, default `tag:grove-vm`) only to reach Nomad. A pool whose
  isolation can't be set up is blocked and reported instead of booting unusable VMs. Linux pools
  only. See docs/OPERATIONS.md "Isolating a pool's network".
- `grove install --role worker --softnet` installs Softnet and prints the one-time command that
  lets it become root; `grove doctor` reports whether it can.
- `grove config set <key> -` reads the value from stdin, so secrets stay out of shell history.
- Fleet page lists the Macs waiting to join, with an **Approve** button beside each, so enrolling
  a Mac no longer means reading its code off one machine's terminal and retyping it on another
  (`GET /api/v1/join/pending`, operator scope).
- `GET /join` serves an unauthenticated page with the install commands, this control plane's
  address, and a QR code of its own link (`GET /join/qr.svg`) — so a Mac that has never met grove
  can be told what to run without anyone typing a tailnet address.

## v0.1.2

- README overhaul: screenshots, real command examples, Mermaid flow diagrams (`docs/FLOW.md`).
- `grove install` trusts third-party Homebrew taps (`brew trust`) before installing; `grove doctor`
  reports untrusted taps.
- Tart images built and published for the first time: `ghcr.io/gm2211/grove-linux-worker`
  (Ubuntu arm64, Nomad 2.0.5, Docker 29.8, Tailscale) and `ghcr.io/gm2211/grove-macos-worker`
  (macOS Sequoia + Xcode 16.4, Nomad, Tailscale). Template fixes: macOS disk ≥ 140 GB base,
  Homebrew on PATH in Packer's non-login SSH shell.

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
- Agent-orchestrator executor contract — see [docs/ORCHESTRATOR.md](docs/ORCHESTRATOR.md).

**Not yet**

- Only live-tested on a single-host smoke stack — not yet run against a real multi-Mac fleet.
- Tart VM images aren't published to `ghcr.io` yet; build your own from `images/`.
- Per-dispatch CPU/memory sizing (`JobRequest.Resources`) is validated against pool defaults but
  not yet applied to the running job's actual resource allocation — see
  [docs/JOBS.md](docs/JOBS.md) "Per-job resource sizing".
