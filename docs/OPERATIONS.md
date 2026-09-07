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
- **Restart a supervised service** after a config change: macOS —
  `launchctl kickstart -k gui/$(id -u)/com.grove.<name>`; Linux —
  `systemctl --user restart grove-<name>`.

## Re-checking health

`grove doctor` any time — it's read-only, 3s-timeout HTTP checks, and never hangs.
