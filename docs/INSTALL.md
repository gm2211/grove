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

grove itself installs everything else (Homebrew, Tart, Nomad, MinIO, Orchard) — including trusting
the third-party Homebrew taps those come from, which recent Homebrew otherwise refuses to load from
(see [Failure modes](#failure-modes)).

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

#### Registry access for private worker images

grove's own worker images (`ghcr.io/gm2211/grove-{linux,macos}-worker:latest`) are pushed to GHCR,
which creates packages **private by default** — and GitHub has no API to change that, only the
web UI. A worker that can't authenticate to a private registry fails to pull the image (see
[Failure modes](#failure-modes) below), so before running fleet workloads, pick one of:

1. **Make the packages public** (simplest, no per-worker credential to manage): on GitHub, go to
   the organization/user that owns the package, **Packages -> `grove-macos-worker`** (and
   `grove-linux-worker`) **-> Package settings -> Change visibility -> Public**. Do this once per
   package; every worker can then pull without logging in.
2. **Log each worker in to the registry**, via `grove install`:

   ```bash
   grove install --role worker --controller https://<control-plane-host>:6120 --token <bootstrap-token> \
     --registry-user <github-username> --registry-token <PAT>
   ```

   The token needs at minimum the `read:packages` scope (a fine-grained PAT scoped to just the
   package, or a classic PAT with `read:packages`, both work). It's read from `--registry-token`,
   or — to avoid it ever landing in shell history — the `GROVE_REGISTRY_TOKEN` environment
   variable:

   ```bash
   GROVE_REGISTRY_TOKEN=<PAT> grove install --role worker --controller ... --registry-user <github-username>
   ```

   `--registry` defaults to `ghcr.io` and rarely needs overriding. This runs an idempotent
   "registry-login" step (`tart login <registry> --username <user> --password-stdin`, token piped
   over stdin — never in argv, never logged) and records a marker file
   (`~/.config/grove/registry-login.<registry>`, containing only the username and registry, never
   the token) so re-running `grove install` doesn't log in again. Omitting `--registry-token`
   entirely skips this step and prints a NOTE that the images must be public instead.

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
| `--registry` | container registry a worker authenticates to (worker role; default `ghcr.io`) |
| `--registry-user` | registry username for `tart login` (worker role) |
| `--registry-token` | registry password/PAT for `tart login` (worker role); also read from `$GROVE_REGISTRY_TOKEN` |
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
Apple's Virtualization.framework), disk free under `~/.tart`, and — for any third-party Homebrew tap
that's actually tapped on this machine (`openai/tools`, `cirruslabs/cli`, `hashicorp/tap`,
`minio/stable`) — whether it's trusted, with the exact `brew trust <tap>` remediation if not. Non-
zero exit if anything's red.

If `fleet.yaml` has any pool pulling a `ghcr.io` image and this machine has no recorded `tart login
ghcr.io` (see "Registry access for private worker images" above), doctor also warns with a
`registry-login` check: make the packages public, or re-run `grove install --role worker
--registry-token …`.

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
- **A worker's VM never starts / `grove vm ls` shows an error status referencing the image**: the
  fleet reconciler's VM create sits `pending` (or fails outright) with an auth error when the
  pool's image is a private `ghcr.io` package and the worker isn't logged in. Check `grove vm ls`'s
  STATUS column and the Orchard worker's own log (`~/Library/Logs/grove/orchard-worker.err.log` —
  see docs/OPERATIONS.md's Log locations table) for a `401`/`403`/"unauthorized" pulling the image.
  Fix by either making the package public or re-running `grove install --role worker
  --registry-token …` (see "Registry access for private worker images" above); `grove doctor`'s
  `registry-login` check flags this proactively.
- **First `grove install --role worker` run is slow**: if no `gm2211/orchard` release asset
  matches your OS/arch yet, it falls back to `git clone` + `go build ./cmd/orchard`, which needs a
  working Go toolchain and takes a minute or two the first time.
- **`Refusing to load formula … from untrusted tap …`**: recent Homebrew (see `brew trust --help`)
  refuses to load any formula from a non-official tap until that tap has been explicitly trusted,
  e.g.:

  ```
  Error: Refusing to load formula openai/tools/softnet from untrusted tap openai/tools.
  Run `brew trust --formula openai/tools/softnet` or `brew trust openai/tools` to trust it.
  ```

  `grove install` handles this itself: before installing from `openai/tools` (Tart; falls back to
  `cirruslabs/cli` if that tap doesn't have it), `hashicorp/tap` (Nomad, control-plane role only) or
  `minio/stable` (MinIO, control-plane role only), it runs `brew tap <tap>` (if not already tapped)
  then `brew trust <tap>`, printing why before it does. If you still hit this error — e.g. you ran
  `brew install` yourself outside of `grove install` — the fix is the command Homebrew already
  printed: `brew trust openai/tools` (or `hashicorp/tap` / `minio/stable`, matching whichever tap
  the error names). `grove doctor` also reports any required tap that's tapped but not trusted,
  with this exact remediation command. On a Homebrew old enough to not have `brew trust` at all,
  grove treats that as nothing to do — there's no trust gate to satisfy.

## See also

- [`docs/OPERATIONS.md`](OPERATIONS.md) — recycling, rolling new images, pausing a Mac, logs,
  upgrading.
- [`ARCHITECTURE.md`](../ARCHITECTURE.md) — the full design.
