# Operating grove

## Recycling / hygiene

Nothing long-lived is cleaned in place — it's replaced. See ARCHITECTURE.md's "Recycling /
hygiene" section for the full mechanism; the short version:

1. Every worker VM has an Orchard **TTL** (fork feature, e.g. `ttl: 12h` in `fleet.yaml`).
2. When it expires, Orchard deletes the VM, running its **ShutdownScript** (fork feature) inside
   the guest first: it marks the VM draining (no new job starts there; grove retries a refused
   job on another VM) and waits for the jobs still running in it to finish.
3. grove's fleet reconciler notices the VM is gone (or spec drifted) and recreates it from the
   pool's `image`.

A macOS VM is also deleted right after the one job it ran, and recreated the same way.

To force a recycle right now (bad VM, want fresh image): `POST /api/v1/vms/{name}/recycle` (deletes
it through Orchard, which runs the same shutdown script first; the reconciler recreates it), or
`orchard delete <name>` directly.

## Jobs, queues and restarts

There is no separate job scheduler: `grove serve` runs every job itself.

- **Queues.** Each pool has a FIFO queue. Jobs start in submission order, as fast as the pool's
  sealed VMs have free slots: a Linux VM takes as many jobs at once as fit its CPU and memory at
  the pool's `jobCPU`/`jobMemory`, a macOS VM takes one. A queued job's `pendingReason` says
  whether it waits for a slot or behind other jobs of its pool. One pool's backlog never holds up
  another pool.
- **State.** Job history is `~/.local/state/grove/jobs.json`. A queued job's env values wait in
  `queue/<id>.json` beside it (0600) and are deleted the moment the job starts; the history keeps
  env keys only. Each job's output is `logs/<id>.log` beside it (0600).
- **Restarting or upgrading `grove serve`** (or the machine it runs on) loses the jobs running at
  that moment: they are marked `lost` (exit 125, "grove serve restarted while the job was
  running"), and the VMs they ran in are deleted so the reconciler recreates them clean. Queued
  jobs are kept and start again in order. Restart when the fleet is quiet if you can
  (`grove status` / the Jobs page shows what's running).
- **Exit codes grove sets:** 124 timeout, 125 lost (VM vanished or `grove serve` restarted),
  143 canceled. A VM that refuses a job because it's draining (75) is retried on another VM, up
  to 3 times, before the job starts.

## Upgrading from a Nomad-based grove

Older releases scheduled jobs with Nomad and could run pools on their host's shared network. Now:

1. Upgrade the `grove` binary on the control plane and re-run `grove install --role
   control-plane`. Its `legacy-nomad:retire` step stops and removes the Nomad LaunchAgent /
   systemd unit, and the config step drops `nomad.url`, `nomad.token` and `tailscale.authKey`
   from `config.yaml`. Nomad's config and data in `~/.config/grove/nomad` are left alone: delete
   them (and `brew uninstall nomad` on macOS, or `~/.local/bin/nomad` on Linux) once you're happy.
2. Jobs that Nomad was still running show as `lost`. Resubmit what you need.
3. Every pool is sealed now. `network: shared` (or no `network:`) in `fleet.yaml` is read as
   `isolated`, with a warning for an explicit `shared`; `allowDockerSocket` is ignored. The
   reconciler recreates every VM that isn't sealed yet. Make sure every worker Mac has Softnet
   able to become root first (`grove install --role worker` prints the step; `grove doctor` shows
   a `softnet` row), or those VMs fail to start.
4. Worker images no longer need Nomad or Tailscale. Older images still work: the sealed startup
   script keeps their Nomad client and Tailscale switched off.
5. Any Tailscale auth key you created for worker VMs can be revoked in the Tailscale admin console.

## Rolling a new image

Pool images (`ghcr.io/gm2211/grove-{linux,macos}-worker:latest`) are Packer-built under `images/`.
After pushing a new tag:

1. Update the pool's `image:` in `~/.config/grove/fleet.yaml` on the control plane.
2. Either wait for TTL-driven recycling to pick it up naturally, or recycle pool VMs immediately
   (`POST /api/v1/vms/{name}/recycle` per VM, or delete them directly — the reconciler notices spec
   drift and recreates against the new image either way).

## Pausing a Mac

To stop new VMs from being scheduled on a worker (maintenance, moving it, thermal issues) without
tearing down what's already running:

```
POST /api/v1/workers/{name}/pause     # Orchard cordon
POST /api/v1/workers/{name}/resume
```

Existing VMs on that worker keep running (and draining/recycling normally) until you resume it or
manually delete them.

## Rotating the registry token

If a worker authenticates to `ghcr.io` via `--registry-token` (rather than the packages being made
public — see docs/IMAGES.md "Packages are private by default"), rotate that token like any other
credential:

1. On GitHub, revoke the old PAT (**Settings -> Developer settings -> Personal access tokens**) and
   mint a new one with the same minimum `read:packages` scope.
2. Re-run, on each affected worker:

   ```bash
   GROVE_REGISTRY_TOKEN=<new PAT> grove install --role worker --registry-user <github-username> \
     --controller https://<control-plane-host>:6120 --token <bootstrap-token>
   ```

   (The `--controller`/`--token` flags are only needed if you're not re-supplying the whole
   command from your notes — `grove install` re-running with the *same* role/controller/token is
   idempotent and only the registry-login step actually does anything new.)
3. `tart login` overwrites the previous credential in the worker's own keychain; grove's marker
   file (`~/.config/grove/registry-login.ghcr.io`) only changes if `--registry-user` also changed,
   so step 2 re-runs `tart login` regardless (it doesn't know the *old* token was revoked, only
   that it was asked to log in again — the marker doesn't record a token or its age, so a "just
   rotate periodically" workflow always needs to re-run this command manually, not something
   `grove doctor` will nudge you about on a schedule).
4. `grove doctor`'s `registry-login` check confirms the marker is present; it can't (and doesn't
   try to) verify the *new* token actually works, since verifying would mean pulling a many-GB
   image (see docs/IMAGES.md "Size expectations") — the first real signal is the next VM the fleet
   reconciler creates (`grove vm ls`'s STATUS column) or the worker's Orchard log.

## Credentials for orchestrators and scripts

An orchestrator (an agent runner, a CI script) should not hold the operator token: it can approve devices,
recycle VMs and arm private-repo sourcing. Mint it its own revocable, scoped credential instead:

```bash
# on any machine with the operator credential; the raw token is printed to stdout once
grove access issue --name ci-bot --scope read --scope dispatch > ci-bot.token
grove access list                       # shows the new device id and its scopes
grove access revoke DEVICE_ID           # when it's no longer needed
```

Scopes: `read` (fleet, and the jobs this credential submitted — status, logs, artifacts),
`dispatch` (build + agent jobs), `dispatch:build`, `dispatch:agent`, `dispatch:shell` (raw `shell`
jobs; never implied by `dispatch`), `operator` (everything). A non-operator credential only ever
sees and cancels its own jobs, and job `env` values come back `[redacted]` to everyone. The same
thing over HTTP is `POST /api/v1/access/devices` with `{"name": ..., "scopes": [...]}`.

Re-running `grove setup` on a Mac gives it a new credential. Once that Mac is back online, grove
revokes the Mac's earlier setup credentials (same name, read/dispatch scopes only), so
`grove access list` holds one live credential per Mac. Credentials issued with `grove access issue`
are never revoked this way. Stale ones left from before this change go with the Mac's next
`grove setup`, or `grove access revoke DEVICE_ID` by hand.

## Letting jobs clone private repos (and turning it back off)

Grove has no GitHub credential until you give it one, so a job against a private repository fails
at `git clone` with `Repository not found`. Arm one only for as long as you need it:

```bash
# on any machine with the operator credential
export GH_TOKEN=github_pat_…                       # or: grove github enable --token-stdin < token.txt
grove github enable --repo gm2211/grove --ttl 4h   # --repo is required; --ttl is recommended
grove github status
grove github disable                               # wipes the token from the control plane
```

Notes worth knowing before you arm it:

- It applies **fleet-wide and immediately** — every build/agent job submitted while it's on, by
  any device that can dispatch, gets the credential for a repo in scope. Name each repo with
  `--repo owner/name` (required, repeatable, `owner/*` allowed), and use a fine-grained,
  read-only token limited to those repos.
- `--ttl` auto-disarms it and wipes the stored token; without one it stays armed until you run
  `grove github disable`. Prefer a TTL — that is what makes this on-demand rather than permanent.
- Use the **`https://`** clone URL. build/agent jobs with any other repo URL are refused.
- The same controls are in the web UI under **Settings -> Private repository sourcing** (operator
  credential only), and `grove github status` shows the fingerprint, scope, expiry and last use.
- Rotating: run `grove github enable` again with the new token; it replaces the armed one.

The full contract — how the token reaches the clone, what is and isn't stored, and who can read it
while a job runs — is in [docs/JOBS.md](JOBS.md#private-repositories).

## Sealed pools

(This section was called "Isolating a pool's network" when sealing was opt-in.)

Every pool is sealed: its VMs reach the public internet and nothing at home — not your LAN, not
your tailnet, not their host Mac, and not grove's control plane (Orchard, the artifact store, the
API). Every exchange between home and a running job is opened from home.

What that means for a pool's VMs:

- **Softnet, host blocked.** Each VM runs on Softnet instead of Tart's NAT, with
  `--net-softnet-block "out @host"`. The guest reaches public internet addresses only. The host
  can still open connections into the guest, which is how Orchard runs the startup script and how
  grove drives each job.
- **No agents, no credentials.** The VM runs no scheduler client and holds no tailnet key, no
  token, no artifact store key and no clone token. (The startup script still switches off the
  Nomad client and Tailscale that older worker images carry.)
- **Public DNS.** The guest resolves through 1.1.1.1 and 8.8.8.8 (Linux: `/etc/resolv.conf`;
  macOS: `networksetup -setdnsservers` on every network service), because the resolver DHCP hands
  out is the blocked host.
- **A fence for job containers** (Linux). `/usr/local/sbin/grove-egress-fence`, run before every
  Docker start and once by the startup script, keeps job containers off private, CGNAT and
  link-local addresses and off the guest itself.

How a job runs. `grove serve`, on the control plane:

1. waits for the job's turn in its pool's queue and a free slot in one of the pool's sealed VMs,
   skipping VMs that are shutting down;
2. fetches the repository on the control plane, with the armed clone token if there is one, but
   checks out no file there: the checkout happens inside the job's container;
3. through Orchard, creates a fresh container in that VM from the pool's runner image and copies
   in the checkout, the script and the job's environment (minus `GH_TOKEN`/`GIT_TOKEN`, which only
   served the clone);
4. starts it, streams its output into the job's log, and polls it every 15 seconds;
5. when it exits, copies a build's `./artifacts/**` out into the artifact store (a directory on
   the control plane, see "Artifacts" below), then removes the container.

A job whose VM stops answering for a minute is reported **lost**, and its timeout (124) and
cancellation work as for any other job. The job's log names the VM it ran in.

Every worker Mac needs **Softnet able to become root by itself**, because Orchard starts it with no
terminal to ask for a password. `grove setup` / `grove install --role worker` install it and print
the one-time `softnet-root` command for an admin to run; `grove doctor` shows a `softnet` row.
Redo that command after any `brew upgrade softnet`, which drops the SUID bit (VMs on that Mac
fail to start until you do).

Recycling a VM (`POST /api/v1/vms/{name}/recycle`, TTL, or drift) runs its shutdown script, which
marks the VM so no new job lands in it and waits for the running ones to finish.

Limits:

- The per-job `image` override is ignored, and no job gets the VM's Docker socket.
- A job still reaches anything on the public internet, your home's public IP included.
- (Linux) Job images must already be in the VM image or come from a public registry (the default runner
  image is on ghcr.io): the guest can't pull from a registry on your LAN or tailnet.
- A job can't call home while it runs (the grove API, your orchestrator, anything on your tailnet). Whatever
  it needs goes in with it; whatever it produces comes out as its log, exit code and artifacts.

### Sealed macOS pools

A macOS guest has no container runtime, so a macOS pool's jobs run differently in step 3 and
after step 5:

- **The job runs as the unprivileged `_grovejob` user**, in a fresh run directory under `/private/var/grove-job/runs/`, in its own process
  group; never as root and never as the VM's admin login. See docs/JOBS.md "Sealed pools".
- **A fresh VM for every job.** A sealed macOS VM runs one job at a time and only one job ever:
  once the job is over, grove marks the VM draining, deletes it through Orchard, and the fleet
  reconciler recreates it from the image on its next tick (every 30 seconds by default). Nothing a
  job leaves behind (files, processes, a changed system setting) can reach the next job. A VM
  that still holds an earlier job (its runner was stopped before it could clean up, e.g. by a
  `grove serve` restart) is deleted instead of being used.
- **Boot cost.** Each job waits for its VM to be recreated and boot first, typically a minute or
  two on top of the job itself, and longer for a large image on a busy Mac. A queue of jobs runs
  at the pace VMs come back.
- **Two VMs per Mac.** Apple's licence lets a Mac run at most two macOS VMs at once, and the
  virtualization framework enforces it. A macOS pool's `perWorker` is therefore 1 or 2, and
  counts against the same two VMs as any other macOS pool on that Mac; jobs per Mac at a time
  equal the pool's `perWorker`.
- **Sizing.** `jobCPU`/`jobMemory` and the runner image don't apply: the job has the whole VM, as
  sized by the pool's `cpu`/`memory`.
- **Timeouts** are enforced from the control plane (macOS has no `timeout(1)`): a job past its
  timeout is stopped (TERM, then KILL after 10 seconds) and reports 124 as usual.
- `hostXcode`: the host's shared Xcode is checked at boot before the VM takes a job.
- There is no in-guest packet fence like Linux's `grove-egress-fence`: Softnet's host block is
  what keeps the guest (and so the job) off home addresses.

## Log locations

| Component | macOS (launchd) | Linux (systemd --user) |
|---|---|---|
| Orchard worker | `~/Library/Logs/grove/orchard-worker.log` (+`.err.log`) | n/a (worker is macOS-only) |
| Orchard controller | `~/Library/Logs/grove/orchard-controller.log` | `journalctl --user -u grove-orchard-controller` |
| grove server | `~/Library/Logs/grove/server.log` | `journalctl --user -u grove-server` |

Job logs themselves (build/agent/shell) are files on the control plane,
`~/.local/state/grove/logs/<job id>.log` (0600, one JSON record per line: time, stream, line).
Read them with `grove dispatch`'s status/logs subcommands or `GET /api/v1/jobs/{id}/logs?follow=1`.
They expire with the job's artifacts: 15 minutes after a successful job, an hour after any other
outcome (the job record stays, with `logsExpiredAt` set).

## Upgrading

- **grove CLI**: re-run `scripts/install.sh` (or `brew upgrade grove` on macOS), then re-run
  `grove install --role <role>` to pick up any new config/unit templates — it's idempotent, so
  already-correct steps are skipped. Restarting `grove serve` marks the jobs it was running
  `lost` (see "Jobs, queues and restarts").
- **Orchard / Tart / Softnet**: `grove install` re-checks each binary; Homebrew formulae upgrade
  in place, and the Orchard fork is re-fetched from its release (or rebuilt) when it lacks a
  feature grove needs.
- **A Homebrew upgrade adds the third-party-tap trust gate**: `brew update && brew upgrade` on a Mac
  that already has `tart`/`softnet` installed can pick up a Homebrew version that now refuses
  to load formulae from an untrusted tap (`Error: Refusing to load formula … from untrusted tap …`)
  even though nothing about grove's own config changed. Re-running `grove install --role <role>`
  fixes it — it re-trusts `openai/tools`/`cirruslabs/cli` as needed
  before touching anything that installs from them — or run `grove doctor` first to see exactly
  which tap it is and the `brew trust <tap>` command to run by hand.
- **Restart a supervised service** after a config change: macOS —
  `launchctl kickstart -k gui/$(id -u)/com.grove.<name>`; Linux —
  `systemctl --user restart grove-<name>`.

## Artifacts

Build artifacts live in a plain directory on the control plane: `artifacts.dir` in `config.yaml`,
`~/.local/state/grove/artifacts` unless you changed it, laid out as `jobs/<job id>/<path>`
(directories 0700, files 0600). Nothing else serves or holds them: `grove serve` lists them,
streams them through `GET /api/v1/jobs/{id}/artifacts/{path}` and deletes them once their
retention runs out (docs/JOBS.md "Artifact retention"), and writes the ones it copies out of
sealed VMs.

- **Disk use**: at most 2 GiB per job, gone 15 min / 1 h after the job ends. `du -sh
  ~/.local/state/grove/artifacts` shows what's there now.
- **Moving it**: stop `grove serve`, move the directory, `grove config set artifacts.dir <new>`,
  start `grove serve` again. Jobs running at that moment lose their artifacts.
- **Upgrading from MinIO**: re-run `grove install --role control-plane`. It stops and removes the
  old `com.grove.minio` / `grove-minio` service and drops the S3 keys from `config.yaml`. Then
  delete what MinIO left behind yourself: `rm -rf ~/.config/grove/minio` (its data, root
  credential and policy file), plus `~/.local/bin/minio` and `~/.local/bin/grove-mc` on Linux, or
  `brew uninstall minio/stable/minio && brew untap minio/stable` on macOS. Artifacts stored in
  MinIO are not migrated; they were due to expire within the hour anyway.

## Re-checking health

`grove doctor` any time — it's read-only, 3s-timeout HTTP checks, and never hangs.

## Troubleshooting

**A job stays `pending`.** Its `pendingReason` says why: waiting for a free slot in one of its
pool's sealed VMs (are the pool's VMs running? `grove vm ls`), or queued behind other jobs of the
same pool. A pool with no running VM leaves jobs queued until one boots; a job that then can't find
any sealed VM of its pool for long enough reports `lost`. A request whose resources exceed the
pool's job default is refused with a 400 at submit time, never left pending — see docs/JOBS.md
"Per-job resource sizing".

**VMs of a pool never start after upgrading.** Every pool is sealed now, which needs Softnet able
to become root on each worker Mac (see "Sealed pools"); `grove doctor` on that Mac shows the
`softnet` row and the command to run.
