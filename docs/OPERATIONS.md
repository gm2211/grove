# Operating grove

## Recycling / hygiene

Nothing long-lived is cleaned in place — it's replaced. See ARCHITECTURE.md's "Recycling /
hygiene" section for the full mechanism; the short version:

1. Every worker VM has an Orchard **TTL** (fork feature, e.g. `ttl: 12h` in `fleet.yaml`).
2. When it expires, Orchard deletes the VM, running its **ShutdownScript** (fork feature) inside
   the guest first: `nomad node drain -self -enable -deadline 2h`, then it waits for zero
   allocations before letting the delete proceed. The guest drains itself — Orchard never learns
   Nomad exists, and Nomad never learns about VM lifecycle.
3. grove's fleet reconciler notices the VM is gone (or spec drifted) and recreates it from the
   pool's `image`.

To force a recycle right now (bad VM, want fresh image): `POST /api/v1/vms/{name}/recycle` (drains
via Nomad, then deletes; the reconciler recreates it), or just `orchard delete <name>` directly if
you don't need the graceful drain.

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

## Isolating a pool's network

By default a worker VM shares its host Mac's network through Tart's NAT, so a job can reach
whatever that Mac reaches: your LAN, services the Mac itself runs, and every device on your
tailnet, through the Mac's own Tailscale login. `network: isolated` closes that for a pool:

```yaml
pools:
  - name: linux
    network: isolated
```

What changes for that pool's VMs:

- **Softnet, host blocked.** Each VM runs on Softnet instead of Tart's NAT, with
  `--net-softnet-block "out @host"`. The guest reaches public internet addresses only: not your
  LAN, not tailnet (100.64.0.0/10) addresses, and not its own host Mac. The host can still open
  connections into the guest, which is how Orchard runs the startup script.
- **Public DNS.** The guest's `/etc/resolv.conf` points at 1.1.1.1 and 8.8.8.8, because the
  resolver DHCP hands out is the blocked host.
- **Its own tailnet login.** The guest joins the tailnet with `tailscale.authKey` as an ephemeral
  device tagged with `tailscale.tags` (default `tag:grove-vm`), with no Tailscale SSH, no subnet
  routes, no tailnet DNS and no inbound connections. Its Nomad client dials the servers' tailnet
  IP, taken from `nomad.url`. Your tailnet policy decides what the tag can reach: give it the
  Nomad RPC port and nothing else (step 2 below).
- **A fence for job containers.** `/usr/local/sbin/grove-egress-fence`, run before every Docker
  start and once by the startup script, keeps job containers off private, tailnet and link-local
  addresses, off the guest's tailnet link, and off the guest itself. A job can't borrow the guest's
  tailnet login; only the guest's own Nomad client uses it.

Before you switch a pool over:

1. **Softnet on every worker Mac that runs the pool**, able to become root by itself, because
   Orchard starts it with no terminal to ask for a password. `grove install --role worker
   --softnet` installs it and prints the one-time `softnet-root` command for an admin to run;
   `grove doctor` shows a `softnet` row. Redo that command after any `brew upgrade softnet`,
   which drops the SUID bit (isolated VMs on that Mac fail to start until you do).
2. **A tailnet policy for the tag.** In the Tailscale admin console's access controls, make the tag
   one you own and let it reach only the Nomad server's RPC port. A rule whose source is `"*"`
   covers tagged devices too, so narrow those to `autogroup:member` (plus any other tags you use)
   at the same time. For example, with your control plane's tailnet IP:

   ```jsonc
   "tagOwners": { "tag:grove-vm": ["autogroup:admin"] },
   "acls": [
     { "action": "accept", "src": ["autogroup:member"], "dst": ["*:*"] },
     { "action": "accept", "src": ["tag:grove-vm"], "dst": ["100.x.y.z:4647"] }
   ]
   ```

3. **A key for the tag.** Either an OAuth client secret (`auth_keys` scope, tagged
   `tag:grove-vm`), which doesn't expire and to which grove adds
   `?ephemeral=true&preauthorized=true` itself, or a reusable, ephemeral auth key tagged
   `tag:grove-vm`, which expires within 90 days. Store it from stdin so it stays out of your shell
   history and the process list:

   ```bash
   grove config set tailscale.authKey - < key.txt
   grove config set tailscale.tags tag:grove-vm      # only needed for a different tag
   ```

4. **`nomad.url` on the control plane's tailnet IP**, not a hostname: isolated guests don't use
   MagicDNS.

Then set `network: isolated` and let the reconciler recreate the pool's VMs (`grove fleet plan`
shows the change first). If step 3 or 4 is missing, `grove fleet plan` prints
`! blocked pool=… : <reason>` and the reconciler refuses to build that pool's VMs: it deletes the
pool's unisolated VMs, keeps any already-isolated ones, and shows the reason as the last reconcile
error in `grove fleet status` until it's fixed.

Limits:

- Linux pools only for now. macOS jobs run as root in the guest with no container boundary, so
  they could use the guest's tailnet login; isolating them needs jobs to run as a normal user first.
- `allowDockerSocket` can't be combined with it, because the socket gives every job root on the
  guest.
- A job still reaches anything on the public internet, your home's public IP included.
- Job images must already be in the VM image or come from a public registry (the default runner
  image is on ghcr.io): the guest can't pull from a registry on your LAN or tailnet.
- Jobs can't reach anything on your tailnet, so `ARTIFACT_*` uploads to a tailnet-only artifact
  store fail from an isolated pool.

## Log locations

| Component | macOS (launchd) | Linux (systemd --user) |
|---|---|---|
| Orchard worker | `~/Library/Logs/grove/orchard-worker.log` (+`.err.log`) | n/a (worker is macOS-only) |
| Orchard controller | `~/Library/Logs/grove/orchard-controller.log` | `journalctl --user -u grove-orchard-controller` |
| Nomad server | `~/Library/Logs/grove/nomad.log` | `journalctl --user -u grove-nomad` |
| MinIO | `~/Library/Logs/grove/minio.log` | `journalctl --user -u grove-minio` |
| grove server | `~/Library/Logs/grove/server.log` | `journalctl --user -u grove-server` |

Job logs themselves (build/agent/shell) come from Nomad — `grove dispatch`'s status/logs
subcommands, or `GET /api/v1/jobs/{id}/logs?follow=1`.

## Upgrading

- **grove CLI**: re-run `scripts/install.sh` (or `brew upgrade grove` on macOS), then re-run
  `grove install --role <role>` to pick up any new config/unit templates — it's idempotent, so
  already-correct steps are skipped.
- **Orchard / Nomad / MinIO**: `grove install` re-checks each binary; bump the version pin (and
  every per-platform SHA-256 beside it, in `internal/install/download.go`) in this
  repo's install steps and re-run to pick up the change (Homebrew formulae upgrade in place;
  Linux downloads re-fetch the pinned version).
- **A Homebrew upgrade adds the third-party-tap trust gate**: `brew update && brew upgrade` on a Mac
  that already has `tart`/`nomad`/`minio` installed can pick up a Homebrew version that now refuses
  to load formulae from an untrusted tap (`Error: Refusing to load formula … from untrusted tap …`)
  even though nothing about grove's own config changed. Re-running `grove install --role <role>`
  fixes it — it re-trusts `openai/tools`/`cirruslabs/cli`/`hashicorp/tap`/`minio/stable` as needed
  before touching anything that installs from them — or run `grove doctor` first to see exactly
  which tap it is and the `brew trust <tap>` command to run by hand.
- **Restart a supervised service** after a config change: macOS —
  `launchctl kickstart -k gui/$(id -u)/com.grove.<name>`; Linux —
  `systemctl --user restart grove-<name>`.

## Re-checking health

`grove doctor` any time — it's read-only, 3s-timeout HTTP checks, and never hangs.

## Troubleshooting

**Jobs pending with `DimensionExhausted cpu` on macOS.** Nomad's stock CPU fingerprinter badly
under-reports on Apple Silicon (a real M5 Max worker fingerprinted `cpu.totalcompute=24`), which
fails placement for any job whose `resources.cpu` request is a realistic MHz value — every
build/agent/shell job on that node stays `pending` forever. `internal/fleet/scripts.go`'s
startup script is supposed to fix this automatically by computing `cpu_total_compute` from the
guest's actual core count (`ncpu * 2000` MHz) and writing it into the dynamic `grove-meta.hcl` (see
docs/IMAGES.md's two-file config contract) — if you're hitting this anyway:

1. Run `grove doctor` — it warns (`macos-cpu-fingerprint`) when any `macos`-class Nomad node
   reports a fingerprinted CPU under 1000 MHz.
2. On the affected Mac, check `/usr/local/etc/nomad.d/grove-meta.hcl` for a `cpu_total_compute`
   line inside its `client {}` block. Missing entirely usually means the startup script hasn't run
   since this fix shipped — recycle the VM (`grove` fleet reconciler recreates it, re-running the
   startup script) rather than editing the file by hand.
3. Present but the Nomad client hasn't picked it up — a `grove-meta.hcl` edit only takes effect on
   the next Nomad client restart: `sudo launchctl kickstart -k system/com.grove.nomad`, then
   `nomad node status -self -verbose | grep cpu.totalcompute` to confirm.
4. Per-job sizing (`JobRequest.Resources` / fleet.yaml's `pools[].jobCPU`/`jobMemory`) is unrelated
   to this — see docs/JOBS.md "Per-job resource sizing" — a request rejected there is a 400 with an
   explicit "exceeds pool's job default" message, not a silently-pending job.
