# grove — VM images

grove's fleet is built from two Tart VM images (`images/macos-worker`, `images/linux-worker`) plus
one plain container image (`images/runner`, the default image for linux jobs). This doc covers
building and pushing them, why they're shaped the way they are, and the contract between an image
and the fleet reconciler / startup-shutdown scripts (`internal/fleet`).

> **Disk size:** the Xcode base image (`ghcr.io/cirruslabs/macos-sequoia-xcode`) is already 140 GB, and Tart
> can only grow a disk, never shrink it — so `disk_size_gb` must be ≥ the base image's size (default 150).
> A build that fails with `new disk size of N GB should be larger than the current disk size of 140 GB`
> means the variable is too small.

## Prerequisites (build machine — must be a Mac)

Tart only runs on Apple Silicon Macs (it's built on `Virtualization.framework`), so both worker
images can only be built locally on a Mac, never in GitHub-hosted CI. See `.github/workflows/images.yml`
for what *is* automated (the `images/runner` container) and the `make images` target below for the
manual path.

```console
$ brew install openai/tools/tart
$ brew tap hashicorp/tap && brew install hashicorp/tap/packer   # or `brew install packer` if your
                                                                  # Homebrew allows the hashicorp tap
$ cd images/macos-worker && packer init . && packer validate . && packer build .
$ cd images/linux-worker && packer init . && packer validate . && packer build .
```

`make images` (repo root) runs both builds in sequence and prints the resulting local Tart VM
names.

## Pushing to GHCR

Packer's `tart-cli` builder produces a local VM (`~/.tart/vms/<vm_name>`); pushing it is a
separate, manual `tart` command — the plugin has no push post-processor:

```console
$ tart login ghcr.io -u <github-username>          # prompts for a PAT with write:packages scope
$ tart push grove-macos-worker ghcr.io/gm2211/grove-macos-worker:latest
$ tart push grove-linux-worker ghcr.io/gm2211/grove-linux-worker:latest
```

Tag with a date or git SHA too (`:2026-09-06`) before overwriting `:latest` if you want the fleet
reconciler's old VMs to keep running their current image until you deliberately bump
`fleet.yaml`'s `pools[].image`.

## Base image choice

| Image | Base | Why |
|---|---|---|
| `macos-worker` | `ghcr.io/cirruslabs/macos-sequoia-xcode:latest` | Only Cirrus Labs base with Xcode pre-installed and licensed; building Xcode from scratch on every image build would dwarf everything else in build time. Ships auto-login for `admin` and passwordless sudo already — the packer template only *verifies* both survived, it doesn't set them up (see `macos-worker.pkr.hcl`'s first provisioner). |
| `linux-worker` | `ghcr.io/cirruslabs/ubuntu:latest` (arm64) | Cirrus Labs' maintained arm64 Ubuntu Tart image; matches the arm64 host architecture (Apple Silicon), so no CPU emulation for the guest OS itself — only for the *containers it runs* (see Rosetta below). |
| `runner` | `ubuntu:24.04` | Plain container, not a Tart VM — this is what actually executes `build`/`agent`/`shell` job scripts on the linux pool (see docs/JOBS.md). Built for both `linux/amd64` and `linux/arm64` by CI. |

## Size expectations

Tart images are full macOS/Linux disk images, not layered containers — expect them to be large
and slow to transfer:

- `grove-macos-worker`: ~90-110 GiB (Xcode alone is ~40 GiB; `disk_size_gb = 100` in the template
  leaves headroom for derived data / SPM caches a build job creates). First `tart pull` on a new
  Mac takes a while even on a fast link; budget for it when adding a worker.
- `grove-linux-worker`: ~8-15 GiB (Ubuntu + Docker + build-essential + Nomad).
- `grove-runner` (container, not a Tart VM): a few hundred MiB, layered/cached normally via GHCR.

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

Cirrus Labs updates `macos-sequoia-xcode` and `ubuntu` periodically (new Xcode point releases,
security patches). Bump `var.base_image`'s default in the relevant `.pkr.hcl`, rebuild, retag, and
push — the fleet reconciler picks up the new image the next time it recreates a VM (on its next
TTL expiry, or immediately for VMs you recycle with `POST /vms/{name}/recycle`).
