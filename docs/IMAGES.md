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
manual path.

```console
$ brew install openai/tools/tart
$ brew tap hashicorp/tap && brew install hashicorp/tap/packer   # or `brew install packer` if your
                                                                  # Homebrew allows the hashicorp tap
$ make images-macos
$ cd images/linux-worker && packer init . && packer validate . && packer build .
```

`make images` builds lean macOS and Linux workers. `make images-macos` builds only lean macOS.
`make images-validate` checks all macOS profiles without pulling or starting a VM.

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
before the VM takes a job. Missing shares, changed builds, incompatible guest OS versions, or failed
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
pool contract; this change does not add automatic toolchain routing or a new pool name. Prefer the existing Linux pool for jobs that do not require macOS.

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

## Release acceptance

Images carry no scheduler agent: `grove serve` drives each job through Orchard exec, so an image
only needs the toolchain, Docker (Linux), the Tart guest agent and, on macOS, a working `sudo` for
the admin account. Build and publish a new immutable image tag, canary it (dispatch a `shell` and
a `build` job to a pool using it), then recycle affected workers. The image build remains a local
Apple Silicon workflow; CI does not automatically publish Tart VMs. Review OS/tool updates before
rebuilding. A pinned base digest is a provenance control, not a vulnerability scan or signed
release.

## Size expectations

Tart images are full macOS/Linux disk images, not layered containers — expect them to be large
and slow to transfer:

- `grove-macos-worker`: 60 GB virtual capacity. Actual host usage depends on the base revision
  and job data; measure the finished image rather than treating capacity as allocated space.
- `grove-macos-xcode-worker`: 150 GB virtual capacity. Full macOS/Xcode images can occupy tens
  of GiB before any Grove job runs. Keep this profile only on hosts that need Apple builds.
- `grove-linux-worker`: ~8-15 GiB (Ubuntu + Docker + build-essential).
- `grove-runner` (container, not a Tart VM): a few hundred MiB, layered/cached normally via GHCR.

Use `tart list` and `du -sh ~/.tart/vms/<vm_name>` to inspect local usage. Cached bases and APFS
clones can share blocks, so directory sizes do not necessarily sum to unique physical usage.
After builds, `tart prune --entries caches --older-than 7` removes old downloadable cache entries
without deleting local workers. Live workers still need the fleet's normal drain/recycle flow;
never delete their disk files directly. Template changes affect newly built images only.

## What the fleet scripts expect from an image

Every VM is sealed (Softnet, host blocked). The fleet's **startup script** (`internal/fleet`,
generated per pool) runs as the image's `admin` user with passwordless `sudo` and, on Linux, sets
public DNS, installs the job-container egress fence (run before every Docker start) and clears
any draining marker; on macOS it sets public
DNS on every network service, prepares the unprivileged `_grovejob` user and, with `hostXcode`,
checks the shared Xcode. It also switches off any Nomad client or Tailscale an image built by an
older grove still carries, so those images keep working sealed.

The **shutdown script** (also fleet-owned, runs inside the guest before Orchard deletes the VM)
marks the VM draining and waits for its running jobs (Linux: grove's job containers; macOS: the
job user's processes) to finish. The image needs nothing special for either.

Neither image installs Tailscale or joins any network of its own: a sealed VM reaches the public
internet only.

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

## The runner image

Linux jobs run in a fresh container of the pool's `runnerImage` (default
`ghcr.io/gm2211/grove-runner:latest`, built from `images/runner`). A per-job `image` override is
ignored and no job gets the VM's Docker socket — see docs/JOBS.md "Inside the sealed VM". Set
`pools[].runnerImage` in `fleet.yaml` to use a prebuilt runner with your toolchain.

## Rebuilding after a base image update

Cirrus Labs updates `macos-sequoia-base`, `macos-sequoia-xcode`, and `ubuntu` periodically.
Update the base reference in the relevant template or Xcode variable file, rebuild, retag, and
push — the fleet reconciler picks up the new image the next time it recreates a VM (on its next
TTL expiry, or immediately for VMs you recycle with `POST /vms/{name}/recycle`).
