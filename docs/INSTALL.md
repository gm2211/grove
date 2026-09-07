# Installing grove

This doc is written so you can hand it to Claude Code on a fresh machine and say "install grove
as a worker" (or control-plane, or client) — it has everything Claude needs, including the failure
modes that come up on real Macs.

## Prerequisites

- **macOS** (Apple Silicon) for `worker`; **macOS or Linux** for `control-plane`; anything for
  `client`.
- **Tailscale**, standalone variant, already authenticated (`tailscale up` run at least once).
  Do **not** use the Mac App Store build — see [Failure modes](#failure-modes) below.
- Admin (sudo) access, for the worker role's privileged checklist.
- Go 1.21+ only if you expect `grove install` to fall back to building the Orchard fork from
  source (it tries a prebuilt release asset first).

grove itself installs everything else (Homebrew, Tart, Nomad, MinIO, Orchard).

## 1. Install the `grove` binary

```bash
curl -fsSL https://raw.githubusercontent.com/gm2211/grove/main/scripts/install.sh | sh
```

This installs the CLI only (via Homebrew on macOS, a direct binary download on Linux) — it never
runs `grove install` for you, since that needs a `--role` and touches system settings.

On macOS this is equivalent to `brew tap gm2211/grove https://github.com/gm2211/grove && brew
install gm2211/grove/grove` — the grove repo doubles as its own Homebrew tap (no separate
`homebrew-tap` repo, no personal access token). Once installed, upgrade in place any time with
`brew upgrade grove`.

## 2. Pick a role and run `grove install`

### Worker (every Mac that runs jobs)

```bash
grove install --role worker --controller https://<control-plane-host>:6120 --token <bootstrap-token>
```

- **Finding the controller URL**: on the control-plane machine, run `grove doctor` (or read
  `~/.config/grove/config.yaml`'s `orchard.url`) — it's `http://<tailnet-ip-or-MagicDNS-name>:6120`
  by default. MagicDNS names look like `grove-cp.tailnetname.ts.net`.
- **The bootstrap token** is whatever the control-plane operator hands out; it becomes the
  `--token` baked into the worker's LaunchAgent.
- Re-run the same command any time — steps that are already satisfied are skipped.
- At the end, `grove install` prints a **privileged checklist**: `sudo pmset`, two
  `sudo defaults write com.apple.network.local-network …` commands (macOS 15+ Local Network
  permission), and a `launchctl bootstrap` to load the worker's LaunchAgent. Run those yourself
  (or ask Claude Code to run them), then reboot if the local-network commands changed anything.

### Control plane (the always-on machine, Linux or a Mac)

```bash
grove install --role control-plane
```

Renders and starts Orchard controller, Nomad server, MinIO, and `grove serve`, all supervised
(launchd on macOS, `systemd --user` on Linux), plus `~/.config/grove/config.yaml` and a starter
`fleet.yaml`. No privileged steps — everything here runs as your own user.

### Client (your laptop, or any machine that just talks to the fleet)

```bash
grove install --role client --server https://<control-plane-host>:6130 --token <server-token>
```

Just writes `~/.config/grove/config.yaml` with the server URL/token. No local services.

### Flags

| Flag | Meaning |
|---|---|
| `--role` | `worker` \| `control-plane` \| `client` (required) |
| `--controller` | Orchard controller URL |
| `--nomad` | Nomad HTTP API URL |
| `--server` | grove server URL |
| `--token` | bootstrap/server token (meaning depends on `--role`) |
| `--yes` | apply privileged steps too, **only takes effect when also running as root** |
| `--dry-run` | print the whole plan, change nothing |

Always safe to preview first:

```bash
grove install --role worker --controller https://... --dry-run
```

## 3. Verify

```bash
grove doctor
```

Prints a ✓/✗ table: Tailscale up, Orchard controller reachable, Nomad reachable, grove server
`/api/v1/healthz`, launchd/systemd units present, Tart installed, macOS VM slot usage (max 2 per
Apple's Virtualization.framework), and disk free under `~/.tart`. Non-zero exit if anything's red.

## Failure modes

- **"Local Network" permission popup blocks the worker (macOS 15+)**: `grove install --role
  worker` prints two `sudo defaults write com.apple.network.local-network …` commands as part of
  its privileged checklist. Run them, then **reboot** — they don't take effect until then.
  Alternative: run the Orchard worker as root with `--user <you>` (see Orchard's README);
  `grove install` doesn't automate that path.
- **FileVault blocks auto-login**: a worker Mac that reboots (power blip, macOS update) needs to
  log in before launchd can start user LaunchAgents. If FileVault is on, either disable it on
  worker Macs or enable automatic login for the worker's user account — otherwise the worker
  silently stops rejoining the fleet after a reboot.
- **Mac App Store Tailscale**: its CLI doesn't run before login, so a headless worker Mac never
  reconnects to the tailnet unattended. `grove install` detects this (App Store bundle present,
  no working CLI) and refuses with an explanation instead of half-configuring things. Fix: install
  the standalone build from https://tailscale.com/download/macos and delete the App Store app.
- **2 macOS VM cap per host**: Apple's Virtualization.framework allows at most 2 concurrent macOS
  guests per physical Mac (Linux guests aren't capped). `grove doctor` reports current usage;
  `fleet.yaml`'s `perWorker` for the `macos` pool should respect this.
- **MacBook lid / thermal notes**: a worker Mac needs to stay awake with the lid open (or on a
  stand that keeps it from sleeping) — `sudo pmset -a disablesleep 1` (in the privileged
  checklist) stops the OS from sleeping, but a clamshell Mac without an external display can still
  throttle or the lid-closed sleep policy can override it on some macOS versions. Prefer running
  worker Macs with the lid open or on a dock, and keep an eye on thermals if it's doing sustained
  macOS VM builds (fanless MacBooks throttle hard).
- **First `grove install --role worker` run is slow**: if no `gm2211/orchard` release asset
  matches your OS/arch yet, it falls back to `git clone` + `go build ./cmd/orchard`, which needs a
  working Go toolchain and takes a minute or two the first time.

## See also

- [`docs/OPERATIONS.md`](OPERATIONS.md) — recycling, rolling new images, pausing a Mac, logs,
  upgrading.
- [`ARCHITECTURE.md`](../ARCHITECTURE.md) — the full design.
