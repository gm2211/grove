# Changelog

All notable changes to grove are documented here.

## Unreleased

- **The control-plane install no longer takes the control plane down.** Its launchd plists and
  systemd units named `nomad` and `grove` bare, and launchd/systemd start jobs with a PATH that
  has neither `~/.local/bin` nor Homebrew, so Nomad exited 78 and `grove serve` never started.
  Units now name the absolute path (this shell's PATH, then `~/.local/bin`, then where the
  install puts it). Relaunching a service also waits for launchd to finish unloading it before
  `launchctl bootstrap`, which failed with "5: Input/output error" on a still-loaded job.

- **`grove registry login` logs an installed worker in to the image registry.** `pbpaste | grove
  registry login --user <github-username>` runs `tart login` with the token over stdin (or from
  `$GROVE_REGISTRY_TOKEN`, never an argument) and logs in again even for the same user, so it is
  also how a token is rotated. Re-running `grove install --role worker` for that would skip the
  login once the marker matched.

- **Re-running `grove install --role worker` keeps the installed worker.** Without
  `--controller`/`--token` it reuses the controller URL in the existing wrapper and the credential
  already in Keychain. Before, it rewrote the wrapper with an empty controller and restarted the
  worker, which then couldn't reach Orchard. With no worker installed, it now refuses and asks for
  `--controller`.

- **Credentials can expire, and a local client can use a Unix socket.** `grove access issue
  --expires-in 180d` (`expiresIn` over HTTP) issues a credential that stops working after that
  long; `grove access list` and `GET /api/v1/whoami` show when. `server.socket` and
  `server.socketUsers` make `grove serve` also listen on a Unix socket that only those macOS
  users can connect to (checked against the peer the kernel reports), with a bearer token still
  required on every request. This is how Argos on the control-plane Mac talks to grove.

- **Every worker runs the credential broker.** Approving a Mac's `grove setup` now also issues it
  a credential with only the `broker` scope, named after the worker. Setup keeps it in Keychain
  and loads a `com.grove.broker` LaunchAgent running `grove broker`, which reads it from there.
  Rerunning setup replaces it and revokes the old one. `grove doctor` gets a `broker` row, and the
  broker's `/healthz` now says whether its last sync worked.

- **Jobs can carry credentials they never see.** `JobRequest.credentials` (`kind`, `env`, `token`)
  gives the job a `gph1_…` placeholder under `env`, keeps the token in the control plane's
  memory only, and serves it to the broker on the worker the job lands on, until the job ends.
  `PUT /api/v1/jobs/{id}/credentials` puts fresh tokens behind the same placeholders. Refused on
  sealed pools. `grove broker --listen` now takes an IP only; the port is always 3128.

- **Credential broker (not wired into dispatch yet).** `grove broker` runs on a worker Mac as the
  proxy its job VMs use. A job gets a placeholder (`gph1_` and 256 random bits) instead of a real
  token; on requests to GitHub, Anthropic or OpenAI/ChatGPT the broker swaps it for the real token
  in the `Authorization`/`x-api-key` header only, and masks that token (raw, URL- or JSON-escaped,
  base64) in the response. Everything else is tunnelled, to public addresses only. Entries live in
  memory, come from the control plane (`GET /api/v1/broker/entries`, a credential with only the new
  `broker` scope, named after the worker) and die when the job ends or a 10-minute lease runs out.
  Dispatch, the VM side and the worker install follow in later changes.

- **grove only listens on the tailnet.** `grove serve` refuses to start when `server.listen` is a
  wildcard (`0.0.0.0`, `:6120`) or resolves to anything but loopback or a Tailscale address, and
  `grove doctor` has a `grove-listen` row for it. Fix with `grove config set server.listen
  <tailnet-ip>:6120`.

- **No authentication configured now means refused, not open.** An API server with no token,
  device store or Tailscale auth answers every `/api/v1` request with 503 instead of treating the
  caller as the operator. `grove serve` always has a device store, so a normal install is
  unaffected; this closes the path where a relocated or half-configured control plane runs open.

- **Pull-request builds of the runner image get no package token.** `images.yml` builds a PR's
  `images/runner` without pushing and with `contents: read` only; `packages: write` is now
  limited to the push-to-main job.

- **`allowDockerSocket` is gone.** Mounting the VM's docker socket gave a job root on the guest,
  and with it a way past the network fence. Every Linux job now runs in its pool's `runnerImage`;
  the per-job `image` meta is ignored on every pool. A `fleet.yaml` that still sets
  `allowDockerSocket` is refused at load: delete the line. The Linux worker image also turns off
  Docker host volumes (takes effect when the image is rebuilt).

- **macOS clones are fenced too.** A macOS job's `source` task stays root (to hide the clone token
  from the job) but runs every git command as a new unprivileged `_grovesrc` user, which the
  guest's pf fence keeps off home networks alongside the job user. The token reaches git's
  credential helper through a file only `_grovesrc` can read, removed when the task ends. A macOS
  VM made before this release fails its clones until it is recreated with the new startup script.

- **Jobs reach the internet, never home.** Every shared pool's guest now fences its jobs off
  private, tailnet (CGNAT) and link-local addresses: Linux job containers in `DOCKER-USER` (the
  sealed pools' fence, plus DNS to the guest's resolvers), macOS jobs with a pf anchor on the job
  user, reloaded at boot. The guest's own Nomad client and Tailscale are unaffected. There is no
  opt-out. The startup script changes, so every shared VM is recreated once on upgrade.

- **Worker Macs no longer authenticate by Tailscale identity.** A shared pool's jobs reach the
  control plane through their host Mac's network, so they arrived as the Mac's identity, which
  could carry its owner's operator grant. grove now refuses Tailscale identity auth from any
  Orchard worker's Mac (by MagicDNS name, failing closed until the worker list loads); workers use
  their Keychain device credential. `server.tailscale.trustWorkerHosts: true` restores the old
  behaviour. docs/OPERATIONS.md no longer suggests granting a user login `operator`.

- **A web page can no longer act as your machine's Tailscale identity.** State-changing API
  requests a browser sends from another site (`Sec-Fetch-Site`, or an `Origin` that doesn't match
  `Host`) are refused with 403, and tailnet-identity auth is only accepted on the control plane's
  own names (an IP, a MagicDNS name, `localhost`, or `server.url`'s host), so a DNS-rebinding page
  can't read or dispatch either. The CLI, argos and the same-origin UI are unaffected.

- **Idempotency keys are per submitter.** Another caller reusing a key gets its own job instead of
  being handed (or blocking) the original caller's.

- **Elastic pools: VMs follow the queue.** A pool with `maxPerWorker` (and optional
  `minPerWorker`) gets one more VM while its jobs wait unplaced, on the roomiest matching worker
  (Orchard VM slots, cores, memory, and at most 2 macOS VMs per Mac). If no worker has room, an
  idle VM of another elastic pool with no waiting jobs is swapped out for it, at most once per
  worker every 15 minutes. VMs idle for 15 minutes above the minimum are removed. `grove fleet
  plan` and the serve log give the reason for each change. Pools without `maxPerWorker` keep
  exactly `perWorker`. See ARCHITECTURE.md "Elastic pools".

- **Linux jobs fall back to macOS.** A job a shared Linux pool can't place moves to the shared
  `macos` pool: after 20 seconds when the pool has no node at all, after 3 minutes when its nodes
  are busy. The job keeps its id and gains `fallback: {from, to, at, reason}`. Jobs whose last 3
  runs on macOS all failed (same kind, pool and repo) stop falling back for 7 days and wait for
  Linux. Opt out per job with `pinPool: true`, per pool with fleet.yaml `fallback: none`. See
  docs/JOBS.md "Falling back to macOS".

- **A boot-looping VM is rebuilt too**, not only a failed one: a VM Orchard has restarted 3 or
  more times (`fleet.BootLoopRestarts`) is deleted and recreated, at most once per 10 minutes.

- **A failed `tailscale up` no longer stops a shared-pool VM's startup script.** Under `set -eu` an
  expired or revoked `tailscale.authKey` ended the script before Nomad restarted with the VM's
  meta, so the VM's Nomad node had no `meta.pool`, took no job, and Orchard restarted the VM
  forever (40,426 times on one Linux worker). The guest now logs the failure and carries on;
  it reaches the control plane through its host's NAT anyway. This changes the generated Linux
  startup script, so existing Linux VMs are recreated once.

- **A job marked lost for never being placed now stops its Nomad job.** It used to stay queued in
  Nomad and run, unwatched, as soon as a matching node joined. `grove serve` also stops the
  leftover Nomad jobs of such lost jobs once at startup.

- **A failed VM is rebuilt instead of holding its slot forever.** Orchard's restart policy never
  touches a VM whose start failed, and the reconciler treated it as up to date, so a pool's only
  VM could sit "failed" for weeks while every job for that pool ended "no allocation within 30m".
  The reconciler now deletes and recreates a VM that has been failed for 10 minutes
  (`fleet.FailedVMRetry`), at most once per 10 minutes, and logs Orchard's failure message as the
  reason.

- **Down Nomad nodes are purged.** Each VM boot registers a new Nomad node and the old one stayed
  "down", so a VM stuck restarting left thousands behind (and `/fleet` fetches every node).
  `grove serve` now purges nodes that stay down for 10 minutes; a live node simply re-registers.
  `grove status` shows nodes by status (`3 (down 1, ready 2)`) instead of one total.

- **A Nomad 403 from the node-token sweep is logged once an hour, with the fix**, instead of every
  30 seconds. It means `nomad.token` in `config.yaml` isn't a management token; per-VM node
  tokens are then never revoked.

- Sealed jobs (`grove sandbox run`) no longer depend on the guest's `timeout(1)`. `run.sh` keeps
  the time limit itself (TERM at the limit, KILL 10 seconds later, exit 124) and runs the script in
  its own process group, so a cancel reaches it exactly once and reports the job's own exit
  status. uutils' `timeout`, Ubuntu 26.04's default, exits 124 or dies leaving the job running
  when it is TERMed; this also fixes CI's `TestRunMacOS_Cancel` on `ubuntu-latest`.

- **Tailnet callers no longer need a grove token.** A request with no `Authorization` header from
  a Tailscale address is authenticated by `tailscale whois` on its peer address and gets the scopes
  the tailnet policy grants it through the `github.com/gm2211/grove` app capability, or that
  `server.tailscale.grants` in `config.yaml` gives its tag or login. Bearer tokens and device
  credentials keep working unchanged, and a token, when sent, always wins. Off with
  `server.tailscale.disabled: true`. See docs/OPERATIONS.md "Tailscale identity instead of tokens".

- **Named job secrets.** `JobRequest.secrets` was reserved and rejected; it now resolves names an
  operator set with `grove secret set NAME --allow tag:argos` (or `PUT /api/v1/secrets/{name}`)
  into the job's environment at dispatch. The submitter never handles the value and job history
  keeps only names. A non-operator may name only secrets granted to its id or Tailscale tags. See
  docs/OPERATIONS.md "Named secrets for jobs".

- **MinIO is gone: build artifacts are kept in a plain directory on the control plane**
  (`artifacts.dir` in `config.yaml`, default `~/.local/state/grove/artifacts`, next to `jobs.json`).
  Only the control plane ever read or wrote artifacts, so there is no longer an artifact service,
  port, bucket or credential: `grove sandbox run` stores what it copies out of a sealed VM
  (files 0600 in 0700 directories, written by atomic rename, paths confined to the directory), and
  `grove serve` serves and expires them exactly as before (15 min / 1 h retention, same API).
  `grove install --role control-plane` no longer installs MinIO or its client; re-running it on an
  existing control plane stops and removes the old MinIO service, drops the `artifacts.endpoint`/
  `bucket`/`accessKey`/`secretKey` keys from `config.yaml` and sets `artifacts.dir`. Until then an
  old `config.yaml` still loads and `grove serve` just warns that those keys are ignored. MinIO's
  data under `~/.config/grove/minio` is left for you to delete (artifacts are not migrated). Shared
  (non-sealed) pools' build template no longer tries to upload with `mc`, and the runner image no
  longer ships `mc`: those builds don't collect artifacts for now. See docs/OPERATIONS.md
  "Artifacts".

- macOS pools can be sealed too (`network: isolated` on the `macos` pool). The VM runs on Softnet
  with the host blocked, public DNS on every network service, and its Nomad client and Tailscale
  turned off, exactly like a sealed Linux VM. With no container runtime in the guest, `grove
  sandbox run --os darwin` runs the job as the unprivileged `_grovejob` user in a fresh run
  directory, and every sealed macOS VM runs **one job only**: once the job is over, grove deletes
  the VM and the fleet reconciler recreates it fresh from the image, so expect a VM boot before
  each job. Timeouts, cancellation, lost VMs, logs and build artifacts work as on sealed Linux
  pools. See docs/OPERATIONS.md "Isolating a pool's network".

- Build artifacts now expire: `grove serve` deletes a job's artifacts 15 minutes after it succeeds
  and 1 hour after it fails, is canceled or is lost (`artifacts.retention.success`/`.failure` in
  `config.yaml`). Expired jobs report `artifactsExpiredAt`. The first sweep after upgrading deletes existing
  artifacts that are already past those windows. See docs/JOBS.md "Artifact retention".

- **Security:** re-running `grove setup` on a Mac no longer leaves its earlier device credentials
  valid. Once the Mac is back online with its new credential, grove revokes the older setup
  credentials with the same name. Operator-issued credentials (`grove access issue`) are untouched.

- **Security:** the control plane's Nomad runs with ACLs on. `grove install --role control-plane`
  bootstraps them and stores the management token in `config.yaml` (`nomad.token`); re-running it
  migrates an install that had ACLs off (restarts Nomad once). Each VM the fleet reconciler creates
  gets its own node-only Nomad token, in its shutdown script's environment only, so it can drain
  itself on recycle; the token is revoked once the VM is gone. The VM's Nomad client enforces ACLs
  too. Existing VMs pick this up on their next recycle. See docs/OPERATIONS.md "Nomad ACLs and node
  tokens".

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
- `fleet.yaml` pools take `network: isolated`, which seals their VMs: Softnet with the host Mac
  blocked, public DNS, a fence for job containers, and no Nomad client, Tailscale login or
  credential inside. A sealed VM reaches the public internet and nothing at home. Its pool's jobs
  run from the control plane instead (`grove sandbox run`, whose Nomad client the control-plane
  install now enables): it clones there, pushes the checkout into a fresh container in a sealed VM,
  streams its logs, polls it, and pulls its artifacts out. A job whose VM stops answering is
  reported lost. Linux pools only. See docs/OPERATIONS.md "Isolating a pool's network".
- Re-running `grove install --role control-plane` restarts Nomad when its config changed, so a new
  config takes effect.
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
