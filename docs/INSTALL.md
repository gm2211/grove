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

grove itself installs everything else (Homebrew, Tart, Nomad, Orchard) — including trusting
the third-party Homebrew taps those come from, which recent Homebrew otherwise refuses to load from
(see [Failure modes](#failure-modes)).

## 1. Install the `grove` binary

```bash
curl -fsSL https://raw.githubusercontent.com/gm2211/grove/main/scripts/install.sh | sh
```

This installs the CLI. On a worker Mac, `grove setup` handles role selection, control-plane
discovery, enrollment, worker installation, and verification.

On macOS this is equivalent to `brew tap gm2211/grove https://github.com/gm2211/grove && brew
install gm2211/grove/grove` — the grove repo doubles as its own Homebrew tap (no separate
`homebrew-tap` repo, no personal access token). Once installed, upgrade in place any time with
`brew upgrade grove`.

On Linux the script resolves the latest release to a tag (or uses `GROVE_VERSION=vX.Y.Z` if set),
downloads that release's tarball **and** its `checksums.txt`, and refuses to install unless the
tarball's SHA-256 matches. When it has to install Homebrew first, it fetches Homebrew's installer
at a pinned commit and checks it against a pinned SHA-256 before running it.

### Verified downloads

Nothing `grove install` downloads with `curl` is installed unverified. Each artifact is fetched at
an explicit version into `~/.cache/grove/downloads/`, hashed, compared against the SHA-256 grove
embeds for that version and platform (`internal/install/download.go`), and installed only on a
match; a mismatch deletes the download and fails the step.

| What | Pinned version | Where |
|---|---|---|
| Nomad (Linux control plane) | 2.0.7 | `releases.hashicorp.com` zip |
| Homebrew installer (worker, when `brew` is missing) | `Homebrew/install@35da6871c4be` | `raw.githubusercontent.com` |

On macOS, Nomad still comes from its Homebrew tap (`hashicorp/tap`); Homebrew verifies the
formula's own pinned sha256. The Homebrew installer, once verified, clones
Homebrew itself from git as it always does. Bumping a pin means changing the version and every
per-platform hash together — the file's header says where each upstream digest comes from.

## 2. Set up this machine

### Worker (every Mac that runs jobs)

```bash
grove setup
```

- If you are standing at the new Mac and do not remember any of this, open
  `http://<control-plane>:6130/join` in its browser: the page carries these same commands and a
  QR code of its own address, so you can send the link to another device by scanning it. It needs
  no credential — a Mac that has not joined yet does not have one.
- `grove setup` discovers Grove peers through Tailscale and creates a ten-minute enrollment
  request. It prints an approval code and waits.
- Discovery is only "a tailnet peer answering on :6130", so setup never trusts it silently: it
  shows the address and Tailscale name it found and asks you to confirm. If several peers answer,
  it lists them and asks which one. Unattended, pass `--server http://<control-plane>:6130`
  (always works, no prompt) or `--yes` (accepts the discovered control plane only when exactly one
  answers).
- Approve it. The Fleet page in `grove ui` lists every Mac that is waiting, with its name,
  tailnet address and code, and an **Approve** button beside each one — so you can do this from
  whatever device already has the UI open. On a terminal instead, run `grove join approve <CODE>`
  on the control-plane machine. Either way, Grove then creates two unique,
  revocable credentials: an Orchard credential limited to worker registration/connect rights,
  plus a Grove `read + build + agent` credential for this device. Raw shell jobs (operator or an
  explicit `dispatch:shell` credential) and named secrets remain privileged.
- The Mac stores both credentials in macOS Keychain and feeds worker credential to Orchard over
  stdin. Neither appears in shell history, process arguments, `config.yaml`, or LaunchAgent plist.
- Re-run the same command any time — steps that are already satisfied are skipped.
- After upgrading to a control plane that issues per-worker Orchard credentials, workers with
  restricted credentials issued before worker binding need to enroll again with `grove setup` and
  be approved. This binds the credential to the worker's exact name so Grove can synchronize its
  assigned VMs and open watch connections. Registration remains allowed, and existing privileged
  legacy credentials remain compatible.
- Grove installs Orchard as `~/Applications/Grove Orchard Worker.app`, gives it a stable bundle
  identity, and associates the per-user LaunchAgent with that identity. The LaunchAgent is loaded
  automatically. The worker runs with `--guest-agent-transport`: it reaches each VM's SSH through
  `tart exec` and the Tart Guest Agent on the VM's control socket, never over the network, so it
  needs no macOS **Local Network** permission. Leave that permission off for **Grove Orchard
  Worker**. Every VM image must run the Tart Guest Agent (both of grove's images do).
- The remaining **privileged checklist** has `sudo pmset`, which keeps the worker from sleeping,
  and the `host-firewall` step. That step installs the `com.grove.pf-vm` LaunchDaemon, which keeps
  VMs off this Mac's ports and its networks except DHCP, DNS, the credential broker and Nomad RPC
  (docs/OPERATIONS.md "The worker Mac's own firewall"). Grove does not add system-wide
  private-network allowlists and does not run Orchard or a companion as root.

#### Registry access for private worker images

grove's own worker images (`ghcr.io/gm2211/grove-{linux,macos}-worker:latest`) are pushed to GHCR,
which creates packages **private by default** — and GitHub has no API to change that, only the
web UI. A worker that can't authenticate to a private registry fails to pull the image (see
[Failure modes](#failure-modes) below), so before running fleet workloads, pick one of:

1. **Make the packages public** (simplest, no per-worker credential to manage): on GitHub, go to
   the organization/user that owns the package, **Packages -> `grove-macos-worker`** (and
   `grove-linux-worker`) **-> Package settings -> Change visibility -> Public**. Do this once per
   package; every worker can then pull without logging in.
2. **Log each worker in to the registry.** Make a classic personal access token with only the
   `read:packages` scope, copy it, and on each worker, as the macOS user the worker runs as:

   ```bash
   pbpaste | grove registry login --user <github-username>
   ```

   A Mac that hasn't joined yet can do it while joining instead:
   `GROVE_REGISTRY_TOKEN=<PAT> grove setup --registry-user <github-username>`.

   Either runs `tart login <registry> --username <user> --password-stdin` with the token over
   stdin (never in argv, never logged), so Tart keeps it in that user's keychain, and records a
   marker file (`~/.config/grove/registry-login.<registry>`, containing only the username and
   registry, never the token) that `grove doctor` checks. `--registry` defaults to `ghcr.io` and
   rarely needs overriding. Without a token, `grove install`/`grove setup` skip this and print a
   NOTE that the images must be public instead.

### Control plane (the always-on machine, Linux or a Mac)

```bash
grove install --role control-plane
```

Renders and starts Orchard controller, Nomad server and `grove serve`, all supervised (launchd on
macOS, `systemd --user` on Linux), plus `~/.config/grove/config.yaml` and a starter `fleet.yaml`.
No privileged steps — everything here runs as your own user.

Build artifacts are kept in a plain directory on this machine, `~/.local/state/grove/artifacts`
(0700, next to the job history in `jobs.json`), recorded in `config.yaml` as `artifacts.dir`. There
is no artifact service, port or credential: only the control plane reads and writes it (`grove
serve` serves and expires artifacts, `grove sandbox run` stores the ones it copies out of sealed
VMs). To keep them elsewhere, `grove config set artifacts.dir /path` and restart `grove serve`.

Upgrading from a release that ran MinIO: re-running `grove install --role control-plane` stops and
removes the `com.grove.minio` LaunchAgent / `grove-minio` systemd unit, drops the old
`artifacts.endpoint`/`bucket`/`accessKey`/`secretKey` keys from `config.yaml` (an older
`config.yaml` still loads before that; `grove serve` logs that it ignores them) and sets
`artifacts.dir`. It leaves `~/.config/grove/minio` (MinIO's data and root credential) for you to
delete — artifacts there are past their retention anyway; see docs/OPERATIONS.md.

### Client (your laptop, or any machine that just talks to the fleet)

Run `grove setup` on a Mac. Enrollment gives that device its own revocable dispatcher credential,
stored in Keychain, and also enables it as a worker. `grove ui` opens Map through a localhost
bridge so the credential never needs to be copied into browser storage. Client-only enrollment
for non-worker and non-macOS machines is planned; legacy `grove install --role client` remains for
manually provisioned environments.

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
| `--name` | the worker's Orchard name, which fleet.yaml's `workerSelector` matches (worker role). Default: the name this Mac's worker is already installed under (grove's wrapper, or an older `com.grove.orchard-worker` LaunchAgent), else the hostname |
| `--softnet` | also install Softnet and print its one-time root step (worker role; only for Macs that run `network: isolated` pools, see [OPERATIONS.md](OPERATIONS.md#isolating-a-pools-network)) |
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
that's actually tapped on this machine (`openai/tools`, `cirruslabs/cli`, `hashicorp/tap`) —
whether it's trusted, with the exact `brew trust <tap>` remediation if not. Non-
zero exit if anything's red.

If `fleet.yaml` has any pool pulling a `ghcr.io` image and this machine has no recorded `tart login
ghcr.io` (see "Registry access for private worker images" above), doctor also warns with a
`registry-login` check: make the packages public, or run `grove registry login` on each worker.

## Failure modes

- **A VM restarts every few minutes and the worker log says `no route to host`**: the worker is
  dialing the VM over the network, which macOS 15+ refuses without Local Network permission. It
  runs a wrapper from before `--guest-agent-transport`: re-run `grove install --role worker` (it
  rebuilds Orchard at the pinned commit and rewrites the wrapper). Grove deliberately never asks
  for that permission, never writes `AllowedEthernetLocalNetworkAddresses` /
  `AllowedWiFiLocalNetworkAddresses` (they exempt matching networks for every process on the Mac)
  and never uses the privileged Orchard helper.
- **The worker log says `guest agent transport to <vm> ...`**: the VM's image doesn't run the Tart
  Guest Agent (`tart exec <vm> true` on the worker fails the same way). Use one of grove's images,
  or a Cirrus Labs base image, all of which ship it.
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
  Fix by either making the package public or running `grove registry login` on the worker
  (see "Registry access for private worker images" above); `grove doctor`'s
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
  `cirruslabs/cli` if that tap doesn't have it) or `hashicorp/tap` (Nomad, control-plane role
  only), it runs `brew tap <tap>` (if not already tapped)
  then `brew trust <tap>`, printing why before it does. If you still hit this error — e.g. you ran
  `brew install` yourself outside of `grove install` — the fix is the command Homebrew already
  printed: `brew trust openai/tools` (or `hashicorp/tap`, matching whichever tap
  the error names). `grove doctor` also reports any required tap that's tapped but not trusted,
  with this exact remediation command. On a Homebrew old enough to not have `brew trust` at all,
  grove treats that as nothing to do — there's no trust gate to satisfy.

## See also

- [`docs/OPERATIONS.md`](OPERATIONS.md) — recycling, rolling new images, pausing a Mac, logs,
  upgrading.
- [`ARCHITECTURE.md`](../ARCHITECTURE.md) — the full design.
