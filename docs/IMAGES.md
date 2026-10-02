# grove — VM images

grove's fleet is built from two Tart VM templates (`images/macos-worker`, `images/linux-worker`) plus
one plain container image (`images/runner`, the default image for linux jobs). This doc covers
building and pushing them, why they're shaped the way they are, and the contract between an image
and the fleet reconciler / startup-shutdown scripts (`internal/fleet`).

> **macOS profiles:** the default worker uses minimal vanilla-derived Tahoe with Command Line Tools and no full Xcode.
> Apple app builds must explicitly select `xcode.pkrvars.hcl` or configure host Xcode sharing below. The profiles use separate local VM
> names so building one does not replace the other.
>
> **Disk capacity is not disk usage.** The lean profile allows 60 GB; Xcode allows 150 GB.
> Upstream base images currently declare 50 GB and 140 GB respectively. Tart cannot shrink a
> base disk: custom bases may require a larger `disk_size_gb`. Lowering this number does not
> remove installed files or shrink an existing VM.

## Prerequisites (build machine — must be a Mac)

Tart only runs on Apple Silicon Macs (it's built on `Virtualization.framework`), so both worker
images can only be built locally on a Mac, never in GitHub-hosted CI. See `.github/workflows/images.yml`
for what *is* automated (the `images/runner` container) and the `make images` target below for the
manual path. The native Nomad build currently requires Go 1.27.1 and an Xcode-selected build
host; the tested host used Xcode 27.0. These build tools are not copied into the worker.

```console
$ brew install openai/tools/tart
$ brew tap hashicorp/tap && brew install hashicorp/tap/packer   # or `brew install packer` if your
                                                                  # Homebrew allows the hashicorp tap
$ make images-macos
$ cd images/linux-worker && packer init . && packer validate . && packer build .
```

`make images` builds lean macOS and Linux workers. `make images-macos` builds only lean macOS.
`make images-validate` checks all macOS profiles without pulling or starting a VM. It first builds (or verifies a cached copy of) the pinned native Nomad artifact on the build host.

For iOS/macOS app builds that need full Xcode and Apple SDKs:

```console
$ make images-macos-xcode
# Equivalent, from images/macos-worker:
$ packer build -var-file=xcode.pkrvars.hcl .
```

This creates `grove-macos-xcode-worker`; the default creates `grove-macos-worker`.

## Optional host Xcode sharing

A `macos` pool can share one host-installed Xcode app read-only instead of storing a separate
copy inside each guest. This is opt-in; omit `hostXcode` to retain the existing behavior.
The default and host-sharing profiles use the same minimal Tahoe base and provisioning; only the local output name differs. Publish one `lean-<git-sha>` image and reuse it for both ordinary CLT workers and host-sharing workers; there is no need to build or download two image variants. Sharing needs **both** an Xcode-free image and `hostXcode` in the pool. Enabling `hostXcode` does not strip Xcode from an already bundled image or change the pool image automatically. Build a lean guest with a compatible macOS version:

```console
$ make images-macos-host-xcode
# Produces grove-macos-host-xcode-worker from vanilla Tahoe, without full Xcode.
$ tart push grove-macos-host-xcode-worker ghcr.io/gm2211/grove-macos-worker:lean-<git-sha>
```

Add this to the existing `macos` pool, retaining its other settings:

```yaml
name: macos
image: ghcr.io/gm2211/grove-macos-worker:lean-<git-sha>
workerSelector:
  host: your-xcode-host
hostXcode:
  path: /Applications/Xcode.app
  buildVersion: 27A266a # Example: use the installed app's ProductBuildVersion.
```

Find the build pin and required guest OS on the host:

```console
$ plutil -extract ProductBuildVersion raw /Applications/Xcode.app/Contents/version.plist
$ plutil -extract LSMinimumSystemVersion raw /Applications/Xcode.app/Contents/Info.plist
```

Use actual Orchard worker labels for `workerSelector`. Every matching host must provide the same
path and pinned Xcode build. Keep the pool name `macos`: this option does not add job routing or
an additional macOS pool class. A build pin change triggers the normal drain/recycle flow.

Orchard disables host sharing by default. An administrator must explicitly add
`--insecure-allow-host-dirs` to the controller's `controller run` service arguments and restart
that controller. Grove's installer does not enable this flag. Configure a narrow read-only
allowlist through the existing authenticated Orchard CLI:

```console
$ orchard get cluster-settings
$ orchard set cluster-settings --host-dir-policies=/Applications/Xcode.app:ro
```

The `set` command appends a policy. Policies are alternatives: a broader writable policy such as
`/Applications` still permits writable requests even after adding this narrower read-only policy.
Ensure no existing writable policy covers the app before enabling host sharing.
Grove always requests only this app as read-only, under the fixed share name `grove-xcode.app`.
No worker-side flag is required. The host Xcode app must remain installed and unchanged while
workers use it; drain those workers before replacing or updating the host app.

The guest selects `/Volumes/My Shared Files/grove-xcode.app/Contents/Developer`. Startup checks
the pinned build and Xcode's minimum macOS version, then performs guest first-launch setup
before starting Nomad. Missing shares, changed builds, incompatible guest OS versions, or failed
Xcode setup stop startup. For example, an Xcode build requiring macOS 26.6 cannot run in the
Sequoia guest. Override the base image if the Tahoe profile does not meet your selected Xcode's
minimum version.

Only the Xcode app is shared. DerivedData, package caches, simulator runtimes, signing identities,
and job outputs remain guest-local and can still consume disk. Host Keychain and developer home
are not shared. This saves the app's duplicated storage; it does not remove the guest OS or its
build data. Use the full-Xcode image when workers need an independent toolchain lifecycle.

## Pushing to GHCR

Packer's `tart-cli` builder produces a local VM (`~/.tart/vms/<vm_name>`); pushing it is a
separate, manual `tart` command — the plugin has no push post-processor:

```console
$ tart login ghcr.io -u <github-username>          # prompts for a PAT with write:packages scope
$ tart push grove-macos-worker ghcr.io/gm2211/grove-macos-worker:base-<git-sha>
$ tart push grove-macos-xcode-worker ghcr.io/gm2211/grove-macos-worker:xcode-<git-sha>
$ tart push grove-linux-worker ghcr.io/gm2211/grove-linux-worker:latest
```

Replace `<git-sha>` with the source revision. Push only the profiles you built and validated.
Use immutable profile tags in `fleet.yaml`'s `pools[].image`, then deliberately recycle workers.
Existing macOS fleets using `:latest` may depend on Xcode: do not overwrite that tag with a lean
image as part of this migration. Select an `xcode-<git-sha>` tag for those fleets; use a
`base-<git-sha>` tag only where jobs do not require full Xcode. Both profiles retain the `macos`
pool and Nomad `raw_exec` contract; this change does not add automatic toolchain routing or a
new pool name. Prefer the existing Linux pool for jobs that do not require macOS.

### Packages are private by default — workers need access too

`tart push` creates a GHCR **package** the first time it runs, and GitHub Container Registry makes
every new package **private by default**. Unlike a repo's visibility, there is **no GitHub API to
change a package's visibility** — it's a UI-only setting. Every worker that pulls the image (via
`tart` from `internal/fleet`'s reconciler, at VM create time) needs either:

1. **The packages made public** (one-time, per package, simplest for a fleet with several
   workers): on GitHub, go to **Packages -> `grove-macos-worker`** (and `grove-linux-worker`
   separately) **-> Package settings -> Change visibility -> Public**.
2. **Each worker logged in to the registry**: `grove install --role worker --registry-user
   <github-username> --registry-token <PAT>` (PAT needs at least `read:packages`; also readable
   from `$GROVE_REGISTRY_TOKEN` so it doesn't have to sit in shell history) — see docs/INSTALL.md's
   "Registry access for private worker images". This runs `tart login <registry>
   --password-stdin`, caching the credential in the worker's own keychain the same way a manual
   `tart login` would, and records a marker (`~/.config/grove/registry-login.<registry>`, never the
   token) so it's idempotent across re-runs.

If neither is done, the fleet reconciler still *creates* the VM record in Orchard, but the actual
`tart pull`/run on the worker fails with an auth error — the VM sits `pending` or shows an error
status. That surfaces in two places: `grove vm ls`'s STATUS column, and the Orchard worker's own
log (`~/Library/Logs/grove/orchard-worker.err.log` on the worker — see docs/OPERATIONS.md's Log
locations table). `grove doctor`'s `registry-login` check flags this proactively whenever
`fleet.yaml` references a `ghcr.io` image and no login marker is present on the machine `grove
doctor` runs on.

## Base image choice

| Image | Base | Why |
|---|---|---|
| `macos-worker` (default) | `ghcr.io/cirruslabs/macos-tahoe-vanilla` (pinned digest in the template) | Minimal macOS, with only CLT, Homebrew, Tart guest transport and Grove job prerequisites added. No inherited CI runners, Ruby installations, GCC or AWS CLI. Inherited auto-login and passwordless sudo are verified during provisioning. |
| `macos-xcode-worker` (explicit profile) | `ghcr.io/cirruslabs/macos-sequoia-xcode:latest` | Full Xcode for Apple app builds. The profile verifies Xcode and runs license/first-launch setup. |
| `macos-host-xcode-worker` (explicit profile) | `ghcr.io/cirruslabs/macos-tahoe-vanilla` (same pinned digest) | Same lean guest; compatible host Xcode is mounted read-only at startup through `hostXcode`. |
| `linux-worker` | `ghcr.io/cirruslabs/ubuntu:latest` (arm64) | Cirrus Labs' maintained arm64 Ubuntu Tart image; matches the arm64 host architecture (Apple Silicon), so no CPU emulation for the guest OS itself — only for the *containers it runs* (see Rosetta below). |
| `runner` | `ubuntu:24.04` | Plain container, not a Tart VM — this is what actually executes `build`/`agent`/`shell` job scripts on the linux pool (see docs/JOBS.md). Built for both `linux/amd64` and `linux/arm64` by CI. |

## Runners without artifact storage

For a pool with no artifact store configured, the optional MinIO client can be omitted when
building a local runner. The default build retains the client using a pinned, checksum-verified
GitHub release rather than the retired moving download URL:

```bash
docker build --build-arg INSTALL_MINIO_CLIENT=false -t grove-runner:studio-v1 images/runner
```

Set that pool's `runnerImage` to the same pinned tag. Do not use `latest` for an image that exists
only inside the worker VM: Nomad always attempts to pull that tag. Omitting the client means the
image cannot upload build artifacts; leave artifact storage unconfigured for this pool.

## Native Nomad dependency and release acceptance

The official Nomad 2.0.7 ARM64 binary panics in Tart guests exposing only one CPU performance
level: the missing efficiency-core count becomes -1. Grove carries a narrow Darwin topology
patch against the exact upstream source revision. `make images-nomad` builds
`2.0.7+grove.1` on the host with pinned source/toolchain inputs and runs the scanner regression
checks. The worker contains only the binary and `/usr/local/share/grove/nomad-build.json`, not
the Go compiler, source or build caches. Do not replace it with an unverified Homebrew upgrade.
Remove the patch once an official ARM64 release passes the same guest tests.

Every Packer profile now boots an isolated local Nomad server/client and requires a completed
`raw_exec` allocation before the image build succeeds. Temporary server state is removed.
Fleet startup also checks local Nomad API readiness, so a crash-looping launchd service is not
reported as a successfully started worker. These checks complement the Grove dispatch matrix;
they do not establish simulator or signing support.

Build and publish a new immutable image tag, canary it, then drain/recycle affected workers.
The image build remains a local Apple Silicon workflow; CI does not automatically publish Tart
VMs. Review OS/tool updates and re-run acceptance before rebuilding. A pinned source checksum
and build manifest are provenance controls, not a vulnerability scan or signed release.

## Size expectations

Tart images are full macOS/Linux disk images, not layered containers — expect them to be large
and slow to transfer:

- `grove-macos-worker`: 60 GB virtual capacity. Actual host usage depends on the base revision
  and job data; measure the finished image rather than treating capacity as allocated space.
- `grove-macos-xcode-worker`: 150 GB virtual capacity. Full macOS/Xcode images can occupy tens
  of GiB before any Grove job runs. Keep this profile only on hosts that need Apple builds.
- `grove-linux-worker`: ~8-15 GiB (Ubuntu + Docker + build-essential + Nomad).
- `grove-runner` (container, not a Tart VM): a few hundred MiB, layered/cached normally via GHCR.

Use `tart list` and `du -sh ~/.tart/vms/<vm_name>` to inspect local usage. Cached bases and APFS
clones can share blocks, so directory sizes do not necessarily sum to unique physical usage.
After builds, `tart prune --entries caches --older-than 7` removes old downloadable cache entries
without deleting local workers. Live workers still need the fleet's normal drain/recycle flow;
never delete their disk files directly. Template changes affect newly built images only.

## The two-file Nomad client config contract

Both worker images bake in a Nomad client config split across two files, because the parts that
never change (data dir, driver plugins, `node_class`) are baked into the image, while the parts
that are only known at boot (which control-plane to dial, which pool/host this particular VM is)
are written by the fleet's VM **startup script** (see `internal/fleet`, `Pool.StartupScript` in
ARCHITECTURE.md's fleet spec) every time a VM starts:

| | macOS (`images/macos-worker`) | Linux (`images/linux-worker`) |
|---|---|---|
| Nomad config dir | `/usr/local/etc/nomad.d` | `/etc/nomad.d` |
| Static file (baked into image) | `client.hcl` — `data_dir`, `client { enabled = true, node_class = "macos" }`, `raw_exec` plugin enabled | `client.hcl` — `data_dir`, `client { enabled = true, node_class = "linux" }`, `docker` (with `volumes.enabled = true`) + `raw_exec` plugins enabled |
| Dynamic file (overwritten by the startup script every boot) | `grove-meta.hcl` — `client { servers = [...] meta { pool = "macos" host = "<worker>" } cpu_total_compute = <ncpu*2000> }` | same shape, `pool = "linux"`, no `cpu_total_compute` override |
| Supervisor | `launchd` — `/Library/LaunchDaemons/com.grove.nomad.plist` (`KeepAlive`, runs `nomad agent -config /usr/local/etc/nomad.d`) | `systemd` — `nomad.service` (`Restart=on-failure`, `nomad agent -config /etc/nomad.d`) |

**macOS-only: `cpu_total_compute`.** Nomad's stock CPU fingerprinter badly under-reports on Apple
Silicon — a real M5 Max worker fingerprinted `cpu.totalcompute=24` (`cpu.frequency=4`,
`cpu.numcores=18`; it's effectively treating GHz as MHz-per-core instead of deriving a usable
total). Since every job's `resources.cpu` is specified in MHz (see docs/JOBS.md), a node
fingerprinting single-digit MHz fails placement for *any* real job with `DimensionExhausted cpu` —
see docs/OPERATIONS.md's troubleshooting entry. Because `images/macos-worker` is one generic image
run on whatever Mac model a given worker happens to be, this can't be a fixed value baked into the
static `client.hcl` — it's computed per boot instead, in the startup script
(`internal/fleet/scripts.go`'s `nomadMetaScript`, only on the `Darwin` branch) as
`$(sysctl -n hw.ncpu) * 2000` MHz/core, and written into the dynamic `grove-meta.hcl`'s `client {}`
block alongside `meta`. `grove doctor` also checks for this (see docs/OPERATIONS.md) by listing
Nomad nodes and warning when a `macos`-class node's fingerprinted CPU is implausibly low.

`nomad agent -config <dir>` merges every `*.hcl` file in the directory, so shipping the static half
in the image and letting the startup script drop in the dynamic half means: (a) the image never
has to know the control-plane's address at build time, and (b) a VM recreated on a different host,
or with a fleet.yaml pointing at a new control-plane address, picks up the right config on its
very next boot with zero image changes — see ARCHITECTURE.md's "Recycling / hygiene" section for
why VMs get recreated in the first place (TTL expiry → `ShutdownScript` drains Nomad → Orchard
deletes → reconciler recreates from the image).

The **shutdown script** (also fleet-owned, runs inside the guest before Orchard deletes the VM)
is what actually calls `nomad node drain -self -enable -deadline 2h` — the image itself doesn't
need to do anything special to support draining; it only needs a Nomad client that's already
correctly configured, which is exactly what these two files guarantee.

## Tailscale

Both images install the **standalone** (non–App-Store) Tailscale build:

- macOS: `brew install tailscale` — the plain formula, which ships `tailscaled` as a LaunchDaemon
  that behaves like the Linux daemon and starts headless before any user logs in. The Homebrew
  *cask* (`tailscale-app`) is the sandboxed Mac-App-Store-equivalent GUI build, which only starts
  once someone is logged into a GUI session — unusable for a worker VM that reboots unattended.
  See ARCHITECTURE.md: "Tailscale standalone variant (not App Store — that one doesn't start
  before login)".
- Linux: the official `tailscale.com/install.sh` script (adds HashiCorp-adjacent apt repo,
  installs the same standalone daemon).

Neither image joins the tailnet (`tailscale up`) — that's the fleet startup script's job, using an
**ephemeral tagged auth key** per VM (see ARCHITECTURE.md: "Tailscale inside guests uses ephemeral
tagged auth keys, so recycled VMs vanish from the tailnet"). Baking a real auth key into an image
would mean every VM cloned from it shares one identity and one key that never expires.

## Rosetta (linux-worker only)

Apple Silicon Macs run **arm64** Linux guests, but plenty of tools (and Docker images) are only
published for `amd64`. `linux-worker.pkr.hcl` sets `rosetta = "rosetta"` on the Tart source, which
shares the host's Rosetta 2 translator into the guest as a virtiofs mount tagged `rosetta` —
*provided* whoever runs the VM also passes `tart run --rosetta rosetta <vm>` (the fleet startup
script does). `images/linux-worker/files/rosetta-binfmt.sh` (run at boot by a `rosetta-binfmt.service`
oneshot unit, before `docker.service`) mounts that share at `/mnt/rosetta` and registers it with
the kernel's `binfmt_misc`, so an `amd64` container's ELF binaries transparently execute through
Rosetta instead of failing or falling back to slow QEMU emulation. This is best-effort: if the VM
wasn't started with `--rosetta rosetta`, the script logs and exits 0 rather than failing the boot.

## Why the default `build`/`agent`/`shell` container isn't dynamically chosen

See docs/JOBS.md's "Why the image isn't just `image = "${NOMAD_META_image}"`" section — the short
version is that Nomad's docker driver can't interpolate its `image` field from dispatch-time meta,
so `images/runner` ships the Docker CLI specifically so a job that wants a different image gets it
via a nested `docker run` against the mounted host socket, rather than by changing the outer
container. That mechanism is now gated behind `fleet.yaml`'s `pools[].allowDockerSocket` (default
`false`) — see docs/JOBS.md's "The socket mount is opt-in per pool, off by default" for why it
defaults off and what happens on each side of the flag.

## Rebuilding after a base image update

Cirrus Labs updates `macos-sequoia-base`, `macos-sequoia-xcode`, and `ubuntu` periodically.
Update the base reference in the relevant template or Xcode variable file, rebuild, retag, and
push — the fleet reconciler picks up the new image the next time it recreates a VM (on its next
TTL expiry, or immediately for VMs you recycle with `POST /vms/{name}/recycle`).
