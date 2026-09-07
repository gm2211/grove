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
- **Orchard / Nomad / MinIO**: `grove install` re-checks each binary; bump the version pin in this
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
