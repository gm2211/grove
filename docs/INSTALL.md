# Installing grove

This doc is written so you can hand it to Claude Code on a fresh machine and say "install grove
as a worker" (or control-plane, or client) — it has everything Claude needs, including the failure
modes that come up on real Macs.

## Prerequisites

- **macOS** (Apple Silicon) for `worker`; **macOS or Linux** for `control-plane`; anything for
  `client`.
- **Tailscale**, standalone variant, already authenticated (`tailscale up` run at least once).
  Do **not** use the Mac App Store build — see [Failure modes](#failure-modes) below.
- Admin (sudo) access for the worker role's root steps, or an admin to paste one block for you.
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
- The Mac keeps the worker credential and the broker credential as 0600 files owned by the
  installing user, in `~/.config/grove/credentials/` (a 0700 directory), and the worker's wrapper
  feeds the first to Orchard over stdin. Neither appears in shell history, process arguments,
  `config.yaml`, a launchd plist or the wrapper script. They are files rather than Keychain items
  because the worker runs at boot with nobody logged in, and a login Keychain is locked, or doesn't
  exist, until someone logs in. A worker installed by an earlier grove keeps working: re-running
  the install copies its credentials out of the Keychain into files (the Keychain items are left
  alone). The control plane's dispatch credential for `grove ui` still goes in the Keychain; from
  a shell with no Keychain (`su -`), setup warns and skips it instead of failing.
- Re-run the same command any time — steps that are already satisfied are skipped.
- After upgrading to a control plane that issues per-worker Orchard credentials, workers with
  restricted credentials issued before worker binding need to enroll again with `grove setup` and
  be approved. This binds the credential to the worker's exact name so Grove can synchronize its
  assigned VMs and open watch connections. Registration remains allowed, and existing privileged
  legacy credentials remain compatible.
- The worker runs Tart only through `~/Applications/Grove Tart.app`: a copy of the genuine
  `tart.app` (Homebrew's, or grove's own install in `~/.local/lib/grove/tart.app`, or the one the
  `tart` on `PATH` belongs to; whichever it is, it is checked to be the genuine Cirrus Labs bundle:
  bundle id, strict code signature, team) with `LSUIElement` set, re-signed ad
  hoc with only the `com.apple.security.virtualization` entitlement, and put first on the
  worker's `PATH` by a shim in `~/.config/grove/tart-bin/`. Homebrew's own Tart is a regular
  foreground app, so every `tart run` by the daemon would flash "Tart" in the Dock of whoever is
  logged in on the Mac. The copy cannot do bridged networking (`com.apple.vm.networking` is
  restricted and does not survive an ad-hoc signature); grove never uses it. The wrapper refuses
  to start (exit 78) if the copy is missing; `grove install --role worker` re-makes it.
- Grove installs Orchard as `~/Applications/Grove Orchard Worker.app` and gives it a stable bundle
  identity. The worker and the credential broker run as **LaunchDaemons with `UserName` set to the
  user that ran the install** (`com.grove.orchard-worker`, `com.grove.broker`), so they start at
  boot and serve jobs with nobody logged in, and they and the VMs they boot are that user's
  processes, never root's. The install stages their plists under `~/.config/grove/launchd/`;
  installing them into `/Library/LaunchDaemons` takes root, so it is the `worker-daemons` root step
  (below). Run the install as an ordinary user: it refuses root, because
  a daemon without a `UserName` would run as root. The worker runs with `--guest-agent-transport`:
  it reaches each VM's SSH through `tart exec` and the Tart Guest Agent on the VM's control socket,
  never over the network, so it needs no macOS **Local Network** permission. Leave that permission
  off for **Grove Orchard Worker**. Every VM image must run the Tart Guest Agent (both of grove's
  images do).
- The **root steps** (`worker-daemons`, `disable-sleep`, `host-firewall`, and `softnet-root` with
  `--softnet`) run last, together, as ONE script through ONE `sudo`. If the user is an admin and on a
  terminal, grove shows the script, asks `[Y/n]`, and macOS asks for the password once (`--yes`
  skips the question; without a terminal it needs sudo credentials already cached). If not (a
  standard user such as `argos`, no terminal, a "no", or `--no-sudo`), grove prints the same
  script as one `sudo /bin/sh -e <<'GROVE_ROOT_STEPS'` block for an admin to paste; then re-run the
  same grove command, which skips what is done. (For a standard user, an admin can instead run
  `grove setup --user argos`, below: one password prompt, no block to paste.) The script uses only absolute system binaries, no
  secrets. Each plist the user staged is copied into `/Library/LaunchDaemons` as root's and checked
  by SHA-256 against the plist grove renders (removed if it differs) before it is loaded; the
  daemons always run as the worker user, never root. `worker-daemons` also removes the per-user
  LaunchAgents of an earlier grove (`retire-launchagents` removes any that remain);
  `disable-sleep` keeps the worker awake; `host-firewall` installs the `com.grove.pf-vm`
  LaunchDaemon, which keeps VMs off this Mac's ports and its networks except DHCP, DNS, the
  credential broker and Nomad RPC (docs/OPERATIONS.md "The worker Mac's own firewall"). Redo after a
  grove upgrade changes the plists. Grove does not add system-wide private-network allowlists and
  does not run Orchard or a companion as root.

#### A dedicated user for the worker (for example `argos`)

A worker does not need anyone logged in at the Mac to run Linux VMs, and the user it runs as does
not need to be an admin, so a dedicated standard user keeps the worker and its jobs apart from the
Mac's owner. **macOS VMs are the exception:** a macOS guest boots only while the worker user has a
login session, because Virtualization.framework signs with a Secure Enclave host key that only that
user's unlocked login keychain can use. Without one, every macOS VM fails with `VZErrorDomain -9 "The
virtual machine encountered a security error … Failed to get current host key"`. On a Mac someone
else uses, log the worker user in once with fast user switching and leave the session running (or
give it auto-login on a dedicated Mac). `grove doctor` warns under `login-session` when it is missing.

1. Create the user (System Settings -> Users & Groups -> **Standard**, or `sysadminctl`), once.
   Tailscale must be usable by it. Homebrew need not be: if it belongs to another user, `grove
   install` installs Tart itself from the pinned release tarball (SHA-256 checked, bundle id,
   signature and team verified) into `~/.local/lib/grove/tart.app`, linked at `~/.local/bin/tart`,
   and Orchard into the user's own home. A Tart that is already on `PATH` and not Homebrew's is
   used as it is.
2. From an admin account's Terminal (or ssh), run setup **for** it. No GUI session for `argos` is
   needed, and nobody has to `su`:

   ```bash
   grove setup --user argos --server http://<control-plane>:6130
   ```

   sudo asks for the admin's password once, before the join request; grove keeps that sudo
   session alive while you run `grove join approve <CODE>` on the control plane (up to 10
   minutes). Then the admin's grove runs the worker install as `argos` (`sudo -n -u argos -H grove
   setup --from-admin`; the join result and any `GROVE_REGISTRY_TOKEN` go to it on stdin, never on
   the command line), shows the root script, asks `[Y/n]`, runs it through the same sudo, and waits
   for the worker to come online. `--yes` skips the questions (without a terminal it needs sudo
   credentials already cached).

   `argos` is not trusted with root, so the admin's grove never runs anything `argos`'s process
   printed or wrote: it renders the root script itself from `argos`'s name and home, the grove
   binary, and the controller and server its own join returned. The plists `argos` staged are
   copied into `/Library/LaunchDaemons` as root's, private (0600) until their SHA-256 matches the
   plist the admin's grove renders, and removed instead of loaded if it doesn't.

   Re-running `grove setup --user argos` on a Mac where `argos`'s worker is already installed
   (both LaunchDaemons in `/Library/LaunchDaemons`, running as `argos`) does **not** join again: no
   approval, no new credentials, no worker restart. It re-checks every step, fixes the ones that
   no longer match (a plist changed by a grove upgrade, a daemon that isn't loaded, the firewall),
   and asks for sudo only to run root steps that fail their check. To start over with a fresh join
   (new credentials, or another control plane), pass `--rejoin`; `--server` naming a different
   control plane than the installed one is refused without it.

   If Homebrew belongs to another user (the Mac's owner), `argos` can read its taps but not change
   them: if the `brew-trust:<tap>` step fails, run the `brew tap X && brew trust X` it prints as
   Homebrew's owner, then re-run setup. A registry login also cannot reach `argos`'s Keychain; setup
   warns and keeps the login in `credentials/registry`, which is all the worker needs.

   Without an admin at hand you can still do it the long way: `su - argos`, `grove setup --server
   ...`; `argos` cannot use `sudo`, so setup prints the root steps as one block for an admin to
   paste, and then `grove setup` runs again as `argos`.
3. Setup does not store this device's dispatch credential for `argos` (there is no Keychain to put
   it in from `sudo -u` or `su -`). That is expected; the worker is unaffected. Run `grove setup`
   once from a login session of that user if you also want to use `grove ui` or dispatch jobs as
   it.

#### A worker with no root (`--rootless`)

If the worker's user has no sudo and no admin is at hand, install it rootless: the worker installs
and runs entirely as that unprivileged user, and there is no block for an admin to paste. Run, as
that user (for example `argos`, logged in at the Mac or over ssh):

```bash
grove setup --rootless --server http://<control-plane>:6130    # or: grove install --role worker --rootless ...
```

It is chosen automatically when the user running the install is not an admin (and the worker is
not already installed as LaunchDaemons); the install then prints what it gives up. `--rootless`
asks for it explicitly, and `worker.rootless: true` in `config.yaml`, which the install writes, keeps
re-runs rootless. `--rootless=false` keeps the root-based install, and clears that setting.

| Root-based install | Rootless install |
|---|---|
| `worker-daemons`: worker and broker LaunchDaemons with `UserName` | `worker-agents`: the same two jobs (same program, wrapper, arguments, KeepAlive + RunAtLoad, log paths) as LaunchAgents in `~/Library/LaunchAgents`, loaded with `launchctl bootout` then `launchctl bootstrap gui/<uid>`. An agent already there under the same label, of an older shape, is replaced in place (backed up first); an agent already right is left running. The step fails unless both jobs are running afterwards |
| `disable-sleep`: `pmset -a disablesleep 1` | `keep-awake`: a `com.grove.keepawake` LaunchAgent running `/usr/bin/caffeinate -ims` (KeepAlive). An existing caffeinate LaunchAgent of the user's (any label, loaded, running `caffeinate` with `-i`, `-s` or `-d`) counts as done, so a Mac that already has one gets no duplicate |
| `host-firewall`: the `com.grove.pf-vm` pf anchor | skipped (`[skip]`): pf needs root, so **VMs on this Mac are not fenced off the host's other ports and your LAN**. `grove doctor` shows it as a warning, not a failure. **Add `--firewall`** to keep it anyway: its one root step is printed for an admin (or run through sudo for one), the worker stays LaunchAgents, and once installed later runs keep it current without the flag |
| `softnet-root` (with `--softnet`) | skipped: Softnet can't be made setuid root, so `network: isolated` pools can't run on this Mac |
| the root script, one `sudo` | none: no sudo, no block, exit status 0 when everything succeeded |

`retire-launchagents` still removes the pieces of an earlier install (`run-worker.sh`,
`worker-bootstrap`, extra plists) once the new agent runs, but never the agents this mode installed.
`grove doctor` checks `worker-agent` and `keep-awake` instead of `worker-daemon`. Every plist uses
absolute program paths, because launchd starts jobs with a minimal `PATH`.

**The trade-off: a LaunchAgent only runs while its user has a GUI login session.** A LaunchDaemon
needed nobody logged in; a rootless worker does not start at boot until its user has logged in at the
Mac, and stops when that user logs out. So the Mac needs that user logged in: turn on automatic login
for it (System Settings -> Users & Groups -> Automatically log in as), or, with FileVault on (which
disables automatic login), unlock the disk and log in once after each reboot. Switch back to
LaunchDaemons any time: re-run the install as the user with `--rootless=false` and run the block it
prints; the root script boots the agents out. Other accounts' superseded worker stacks are not this
user's to remove: the doctor reports them (`worker-shape`).

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
   stdin (never in argv, never logged), so Tart keeps it in that user's keychain. The worker
   daemon starts before anyone logs in and can't read that keychain, so grove also keeps the login
   in `~/.config/grove/credentials/registry` (0600, two lines: user, token). The worker's wrapper
   reads it into `TART_REGISTRY_USERNAME` and `TART_REGISTRY_PASSWORD`, which Tart honours, so the
   token is never in a plist, argv or the wrapper's text; a running worker is restarted onto it.
   It also records a marker file (`~/.config/grove/registry-login.<registry>`, containing only the
   username and registry, never the token) that `grove doctor` checks. `--registry` defaults to `ghcr.io` and
   rarely needs overriding. Without a token, `grove install`/`grove setup` skip this and print a
   NOTE that the images must be public instead.

#### Repairing a worker: re-run the install

`grove install --role worker` is the supported repair for any installed worker, whatever shape it
was installed in: it heals the worker in place rather than orphaning it. It never needs
`--controller`/`--token` for a worker that is already installed (it finds them), and it:

1. **Keeps the worker's identity.** The Orchard name and labels are reused from, in order: the
   record this installer keeps (`~/.config/grove/worker-identity.json`), grove's wrapper, a
   hand-written `~/.config/grove/run-worker.sh`, a per-user LaunchAgent's arguments (following it
   into the script it runs). A Mac with no worker gets its hostname and `host=<name>,arch=arm64`.
   So a worker that runs as `--name studio --labels host=studio` stays that, and the pools whose
   `workerSelector` is `host: studio` keep their VMs. `--name` and `--labels` override.
2. **Renders the current launch shape over whatever is there:** the wrapper with
   `--guest-agent-transport`, the worker and broker LaunchDaemons (absolute program paths only; a
   bare `nomad`/`grove` makes launchd exit 78), and the Orchard app at the pinned commit. A
   running worker is restarted onto it (kill and recreate; its VMs reboot).
3. **Removes what the new shape supersedes**, after the new LaunchDaemon is loaded: the old
   per-user LaunchAgent (in the installing user's session, or another account's, such as a
   MacBook's earlier `gmecocci` stack), `run-worker.sh`, and `worker-bootstrap` once its token is in
   `credentials/orchard-worker-bootstrap`. Everything it rewrites or removes is first copied to
   `~/.config/grove/backups/<timestamp>/` (0700), and each action is printed.
4. **Is idempotent:** with nothing to change it says so and touches and restarts nothing.

**Renaming** (`--name` or `--labels` differing from the installed identity) needs a bootstrap
credential minted for the new name, so pass `--token`. On the control plane host (a `config.yaml`
that names a local `fleet.yaml`) the install also rewrites the `workerSelector` entries that
matched the old labels (comments kept, `fleet.yaml` backed up) and deletes the old Orchard worker
record, once the renamed worker is loaded; an interrupted rename is retried by the next run. On
any other host it refuses and prints the exact `fleet.yaml` change and the record to delete; make
that change on the control plane, then re-run with `--force`.

`grove doctor` has a `worker-shape` check that flags a legacy or drifted worker (no guest-agent
transport, bare program paths, a leftover LaunchAgent or `run-worker.sh`, a second account's
stack, a pool whose `workerSelector` matches no worker) and tells you to re-run this command.

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
| `--name` | the worker's Orchard name, which fleet.yaml's `workerSelector` matches (worker role). Default: the name this Mac's worker is already installed under (its recorded identity, grove's wrapper, `run-worker.sh`, or an older `com.grove.orchard-worker` LaunchAgent), else the hostname |
| `--labels` | the worker's Orchard labels, `k=v,k=v` (worker role). Default: the labels it is already installed with, else `host=<name>,arch=arm64` |
| `--force` | rename a worker on a host that is not the control plane: print the `fleet.yaml` change it needs instead of refusing (worker role) |
| `--softnet` | also install Softnet and add its one-time root step (worker role; only for Macs that run `network: isolated` pools, see [OPERATIONS.md](OPERATIONS.md#isolating-a-pools-network)) |
| `--yes` | run the root steps through sudo without asking (needs a terminal, or cached sudo credentials); as root, apply them directly |
| `--no-sudo` | never run sudo; print the root steps as one block for an admin |
| `--firewall` | with a rootless worker, still install the VM firewall (`host-firewall`): one root step for an admin, the worker stays LaunchAgents |
| `--rootless` | install the worker with no root at all: per-user LaunchAgents, a caffeinate keep-awake agent, no VM firewall (worker role; chosen automatically for a user who isn't an admin; `--rootless=false` keeps the root-based install; see [A worker with no root](#a-worker-with-no-root---rootless)) |
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
  runs a wrapper (or a hand-written `run-worker.sh`) from before `--guest-agent-transport`: re-run
  `grove install --role worker` (it keeps the worker's name and labels, rebuilds Orchard at the
  pinned commit, rewrites the wrapper and removes the old launcher; see "Repairing a worker"). Grove deliberately never asks
  for that permission, never writes `AllowedEthernetLocalNetworkAddresses` /
  `AllowedWiFiLocalNetworkAddresses` (they exempt matching networks for every process on the Mac)
  and never uses the privileged Orchard helper.
- **The worker log says `guest agent transport to <vm> ...`**: the VM's image doesn't run the Tart
  Guest Agent (`tart exec <vm> true` on the worker fails the same way). Use one of grove's images,
  or a Cirrus Labs base image, all of which ship it.
- **FileVault and reboots**: the worker and broker are LaunchDaemons, so a worker Mac needs no
  login or auto-login to rejoin the fleet after a reboot. What FileVault still needs is the disk
  unlocked: after a power loss or a macOS update, a FileVault Mac waits at the pre-boot unlock
  screen for a password, and nothing runs until someone types it. Either leave FileVault off on
  worker Macs, or use `sudo fdesetup authrestart` for planned restarts (it unlocks the disk for
  one reboot); an unplanned power loss still needs a person at the Mac.
- **`grove doctor` says `worker-daemon` is not installed, or a LaunchAgent remains**: the
  `worker-daemons` root step has not been run (or is stale after an
  upgrade). Re-run `grove setup` (or `grove install --role worker`) as an admin, or paste the block it prints.
- **Mac App Store Tailscale**: its CLI doesn't run before login, so a headless worker Mac never
  reconnects to the tailnet unattended. `grove install` detects this (App Store bundle present,
  no working CLI) and refuses with an explanation instead of half-configuring things. Fix: install
  the standalone build from https://tailscale.com/download/macos and delete the App Store app.
- **2 macOS VM cap per host**: Apple's Virtualization.framework allows at most 2 concurrent macOS
  guests per physical Mac (Linux guests aren't capped). `grove doctor` reports current usage;
  `fleet.yaml`'s `perWorker` for the `macos` pool should respect this.
- **MacBook lid / thermal notes**: a worker Mac needs to stay awake with the lid open (or on a
  stand that keeps it from sleeping) — `sudo pmset -a disablesleep 1` (one of the root steps) stops the OS from sleeping, but a clamshell Mac without an external display can still
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
- **`grove install` can't get Orchard** (`install the prebuilt Orchard from grove vX … no Go toolchain`):
  the installer downloads `orchard_<os>_<arch>.tar.gz` from its own release
  (`github.com/gm2211/grove/releases/download/v<version>/`) and checks it against that release's
  `checksums.txt`. It builds `./cmd/orchard` from source (needs Go; a minute or two) only when
  that asset is missing, which is the case for a dev build or a release older than the assets, and
  Go is on `PATH`. Fix: install a released grove (`brew upgrade grove`) or install Go and re-run.
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
  grove treats that as nothing to do — there's no trust gate to satisfy. When Tart does not come
  from Homebrew (a non-Homebrew `tart` is already on `PATH`, or Homebrew is another user's and
  grove installs the release tarball), the `openai/tools` trust step has nothing to do and passes.

## See also

- [`docs/OPERATIONS.md`](OPERATIONS.md) — recycling, rolling new images, pausing a Mac, logs,
  upgrading.
- [`ARCHITECTURE.md`](../ARCHITECTURE.md) — the full design.
