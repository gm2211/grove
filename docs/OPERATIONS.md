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

## Nomad ACLs and node tokens

`grove install --role control-plane` runs Nomad with ACLs on. It bootstraps them once and keeps
the management token in `config.yaml` (`nomad.token`, file mode 0600); `grove serve` and the CLI
send it on every Nomad call. A request without a token (from the tailnet, or from inside a job)
gets nothing. An install made while ACLs were off is migrated by re-running the installer: it
re-renders `server.hcl`, restarts Nomad once, and bootstraps. If Nomad was already bootstrapped by
hand, set the existing management token with `grove config set nomad.token <secret>`.

With `nomad.token` set, the fleet reconciler also gives each VM it creates:

- **its own node token** (Nomad policy `grove-node`: `node` write and `agent` read, no namespace access, named
  `grove-vm/<vm>`). It sits only in the VM's shutdown script environment, never on the VM's disk,
  so `nomad node drain -self` works on recycle and jobs never see it. The reconciler revokes a
  VM's token once Orchard has actually removed the VM (`nomad acl token list` shows the live ones).
- **ACL enforcement in the VM's own Nomad client**, so a job can't read other allocations' files
  or logs through `127.0.0.1:4646`.

VMs created before this keep running unchanged and pick both up on their next recycle. To apply
it now, recycle them with `POST /api/v1/vms/{name}/recycle`, which drains through grove's own token
first (their old shutdown script has no token, so its own drain step fails).

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

## Credentials for orchestrators and scripts

An orchestrator (an agent runner, a CI script) should not hold the operator token: it can approve devices,
recycle VMs and arm private-repo sourcing. Mint it its own revocable, scoped credential instead:

```bash
# on any machine with the operator credential; the raw token is printed to stdout once
grove access issue --name ci-bot --scope read --scope dispatch > ci-bot.token
grove access list                       # shows the new device id and its scopes
grove access revoke DEVICE_ID           # when it's no longer needed
```

Scopes: `read` (fleet, and the jobs this credential submitted — status, logs, artifacts),
`dispatch` (build + agent jobs), `dispatch:build`, `dispatch:agent`, `dispatch:shell` (raw `shell`
jobs; never implied by `dispatch`), `operator` (everything). A non-operator credential only ever
sees and cancels its own jobs, and job `env` values come back `[redacted]` to everyone. The same
thing over HTTP is `POST /api/v1/access/devices` with `{"name": ..., "scopes": [...]}`.

Re-running `grove setup` on a Mac gives it a new credential. Once that Mac is back online, grove
revokes the Mac's earlier setup credentials (same name, read/dispatch scopes only), so
`grove access list` holds one live credential per Mac. Credentials issued with `grove access issue`
are never revoked this way. Stale ones left from before this change go with the Mac's next
`grove setup`, or `grove access revoke DEVICE_ID` by hand.

## Tailscale identity instead of tokens

A machine on the tailnet can call the API with no grove token at all: `grove serve` looks up who
the caller is with `tailscale whois` on the connection's peer address (the address WireGuard
already authenticated) and gives it the scopes you granted that identity. Nothing is minted,
stored or rotated, and removing a node from the tailnet (or a tag from a node) removes its access
within 30 seconds. Grant scopes in the tailnet policy file, next to the rest of your ACLs:

```jsonc
"grants": [
  {
    "src": ["tag:argos"],
    "dst": ["tag:grove"],          // the control plane's tag
    "ip":  ["tcp:6120"],
    "app": {"github.com/gm2211/grove": [{"scopes": ["read", "dispatch"]}]}
  }
]
```

or, without touching the policy file, in the control plane's `config.yaml`:

```yaml
server:
  tailscale:
    grants:
      - match: tag:argos              # a node tag
        scopes: [read, dispatch]
      - match: you@example.com        # a user login (untagged nodes only)
        scopes: [operator]
```

The scopes are the same as for device credentials (below); unknown ones are ignored. Both sources
add up. A peer that nothing grants gets a 401, as does a bad `Authorization` header from any peer
(a token, when sent, is always the identity). To check what a machine gets, `curl` the
control plane's `/api/v1/whoami` from it: a Tailscale principal's id is `tailscale:<node stable
id>`, with the node's tags. It needs the `tailscale` CLI on the
control plane, is on whenever one is found, and `server.tailscale.disabled: true` turns it off.
Requests that reach grove through a local proxy (`tailscale serve`, nginx) come from loopback and
are never authenticated this way.

Bearer tokens keep working alongside it, so moving an orchestrator over is: grant its tag, stop
sending the token, then `grove access revoke` its device credential.

## Named secrets for jobs

A job that needs a credential (a Claude or Codex sign-in, an API key) names it instead of carrying
it: `"secrets": ["CLAUDE_CODE_OAUTH_TOKEN"]`. The value lives on the control plane
(`secrets.json` next to `config.yaml`, mode 0600) and is exported into the job under that name at
dispatch; the submitter never sends or sees it, and job history keeps only the name.

```bash
# on any machine with the operator credential; the value comes from stdin or an env var, never argv
claude setup-token | grove secret set CLAUDE_CODE_OAUTH_TOKEN --allow tag:argos
grove secret set CODEX_AUTH_JSON --allow tag:argos < ~/.codex/auth.json
grove secret set OPENAI_API_KEY --from-env OPENAI_API_KEY --allow DEVICE_ID
grove secret list                         # names, grants, fingerprints, last use — never values
grove secret rm OPENAI_API_KEY
```

`--allow` takes a Tailscale tag (`tag:argos`), a Tailscale identity (`tailscale:<stable id>`) or a
device credential id, and repeats; with none, only an operator can request the secret. Running
`grove secret set` again rotates the value (the next dispatch gets it) and replaces the grants.
Names are shell variable names; `PATH`, `HOME` and the like, and the `NOMAD_`/`GROVE_` prefixes,
are refused. Values may be multi-line (up to 64 KiB). The same operations are
`GET /api/v1/secrets` and `PUT`/`DELETE /api/v1/secrets/{name}`.

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
tailnet, through the Mac's own Tailscale login. `network: isolated` seals a pool's VMs instead:

```yaml
pools:
  - name: linux
    network: isolated
  - name: macos
    network: isolated
```

A sealed VM reaches the public internet and nothing at home: not your LAN, not your tailnet, not
its host Mac, and not grove's control plane (Nomad, Orchard, the artifact store, the API). Every
exchange between home and a running job is opened from home.

What changes for that pool's VMs:

- **Softnet, host blocked.** Each VM runs on Softnet instead of Tart's NAT, with
  `--net-softnet-block "out @host"`. The guest reaches public internet addresses only. The host
  can still open connections into the guest, which is how Orchard runs the startup script and how
  grove drives each job.
- **No Nomad, no Tailscale, no credentials.** The startup script turns off the image's Nomad client
  and Tailscale (on macOS it disables and boots out their launchd jobs, `com.grove.nomad` and
  `homebrew.mxcl.tailscale`). The VM holds no tailnet key, no Nomad token, no artifact store key
  and no clone token.
- **Public DNS.** The guest resolves through 1.1.1.1 and 8.8.8.8 (Linux: `/etc/resolv.conf`;
  macOS: `networksetup -setdnsservers` on every network service), because the resolver DHCP hands
  out is the blocked host.
- **A fence for job containers** (Linux). `/usr/local/sbin/grove-egress-fence`, run before every
  Docker start and once by the startup script, keeps job containers off private, CGNAT and
  link-local addresses and off the guest itself.

How a sealed pool's job runs. Its Nomad job (`nomad/jobs/sandbox.nomad.hcl`) isn't placed in a VM
but on the control plane, whose own Nomad agent is also a client for exactly these jobs (node meta
`grove_sandbox_host`). There `grove sandbox run`:

1. fetches the repository on the control plane, with the armed clone token if there is one, but
   checks out no file there: the checkout happens inside the job's container;
2. takes a free slot in one of the pool's sealed VMs (a Linux VM runs as many jobs at once as fit
   its CPU and memory at the pool's `jobCPU`/`jobMemory`; a macOS VM runs one), skipping VMs that
   are shutting down;
3. through Orchard, creates a fresh container in that VM from the pool's runner image and copies
   in the checkout, the script and the job's environment (minus `GH_TOKEN`/`GIT_TOKEN`, which only
   served the clone);
4. starts it, streams its output into the job's log, and polls it every 15 seconds;
5. when it exits, copies a build's `./artifacts/**` out into the artifact store (a directory on
   the control plane, see "Artifacts" below), then removes the container.

A job whose VM stops answering for a minute is reported **lost**, and its timeout (124) and
cancellation work as for any other job. The job's log names the VM it ran in.

Before you switch a pool over:

1. **Softnet on every worker Mac that runs the pool**, able to become root by itself, because
   Orchard starts it with no terminal to ask for a password. `grove install --role worker
   --softnet` installs it and prints the one-time `softnet-root` command for an admin to run;
   `grove doctor` shows a `softnet` row. Redo that command after any `brew upgrade softnet`,
   which drops the SUID bit (isolated VMs on that Mac fail to start until you do).
2. **The control plane runs sealed jobs.** Re-run `grove install --role control-plane` on the
   control plane after upgrading grove: it makes the control plane's Nomad agent the client for
   sealed pools' jobs and restarts it. `grove doctor` shows a `sandbox-host` row once a pool is
   sealed.

Then set `network: isolated` and let the reconciler recreate the pool's VMs (`grove fleet plan`
shows the change first). Re-running `grove serve` (the control-plane install reloads it)
registers the pool's jobs from the sealed template.

Recycling a sealed VM (`POST /api/v1/vms/{name}/recycle`, TTL, or drift) runs its shutdown
script, which marks the VM so no new job lands in it and waits for the running ones to finish.

Limits:

- `allowDockerSocket` can't be combined with it, and the per-job `image` override is ignored.
- A job still reaches anything on the public internet, your home's public IP included.
- (Linux) Job images must already be in the VM image or come from a public registry (the default runner
  image is on ghcr.io): the guest can't pull from a registry on your LAN or tailnet.
- A job can't call home while it runs (the grove API, your orchestrator, anything on your tailnet). Whatever
  it needs goes in with it; whatever it produces comes out as its log, exit code and artifacts.

### Sealed macOS pools

A macOS guest has no container runtime, so a sealed macOS pool's jobs run differently in step 3
and after step 5:

- **The job runs as the unprivileged `_grovejob` user** (the same account a shared macOS pool's
  jobs use), in a fresh run directory under `/private/var/grove-job/runs/`, in its own process
  group; never as root and never as the VM's admin login. See docs/JOBS.md "Sealed pools".
- **A fresh VM for every job.** A sealed macOS VM runs one job at a time and only one job ever:
  once the job is over, grove marks the VM draining, deletes it through Orchard, and the fleet
  reconciler recreates it from the image on its next tick (every 30 seconds by default). Nothing a
  job leaves behind (files, processes, a changed system setting) can reach the next job. A VM
  that still holds an earlier job (its `grove sandbox run` was killed before it could clean up)
  is deleted by the next job's runner instead of being used.
- **Boot cost.** Each job waits for its VM to be recreated and boot first, typically a minute or
  two on top of the job itself, and longer for a large image on a busy Mac. A queue of jobs runs
  at the pace VMs come back.
- **Two VMs per Mac.** Apple's licence lets a Mac run at most two macOS VMs at once, and the
  virtualization framework enforces it. A sealed macOS pool's `perWorker` is therefore 1 or 2,
  and counts against the same two VMs as any shared macOS pool on that Mac; jobs per Mac at a time
  equal the pool's `perWorker`.
- **Sizing.** `jobCPU`/`jobMemory` and the runner image don't apply: the job has the whole VM, as
  sized by the pool's `cpu`/`memory`.
- **Timeouts** are enforced from the control plane (macOS has no `timeout(1)`): a job past its
  timeout is stopped (TERM, then KILL after 10 seconds) and reports 124 as usual.
- `hostXcode` works as on a shared macOS pool: the shared Xcode is checked at boot, and Nomad
  stays off.
- There is no in-guest packet fence like Linux's `grove-egress-fence`: Softnet's host block is
  what keeps the guest (and so the job) off home addresses.

## Log locations

| Component | macOS (launchd) | Linux (systemd --user) |
|---|---|---|
| Orchard worker | `~/Library/Logs/grove/orchard-worker.log` (+`.err.log`) | n/a (worker is macOS-only) |
| Orchard controller | `~/Library/Logs/grove/orchard-controller.log` | `journalctl --user -u grove-orchard-controller` |
| Nomad server | `~/Library/Logs/grove/nomad.log` | `journalctl --user -u grove-nomad` |
| grove server | `~/Library/Logs/grove/server.log` | `journalctl --user -u grove-server` |

Job logs themselves (build/agent/shell) come from Nomad — `grove dispatch`'s status/logs
subcommands, or `GET /api/v1/jobs/{id}/logs?follow=1`.

## Upgrading

- **grove CLI**: re-run `scripts/install.sh` (or `brew upgrade grove` on macOS), then re-run
  `grove install --role <role>` to pick up any new config/unit templates — it's idempotent, so
  already-correct steps are skipped.
- **Orchard / Nomad**: `grove install` re-checks each binary; bump the version pin (and
  every per-platform SHA-256 beside it, in `internal/install/download.go`) in this
  repo's install steps and re-run to pick up the change (Homebrew formulae upgrade in place;
  Linux downloads re-fetch the pinned version).
- **A Homebrew upgrade adds the third-party-tap trust gate**: `brew update && brew upgrade` on a Mac
  that already has `tart`/`nomad` installed can pick up a Homebrew version that now refuses
  to load formulae from an untrusted tap (`Error: Refusing to load formula … from untrusted tap …`)
  even though nothing about grove's own config changed. Re-running `grove install --role <role>`
  fixes it — it re-trusts `openai/tools`/`cirruslabs/cli`/`hashicorp/tap` as needed
  before touching anything that installs from them — or run `grove doctor` first to see exactly
  which tap it is and the `brew trust <tap>` command to run by hand.
- **Restart a supervised service** after a config change: macOS —
  `launchctl kickstart -k gui/$(id -u)/com.grove.<name>`; Linux —
  `systemctl --user restart grove-<name>`.

## Artifacts

Build artifacts live in a plain directory on the control plane: `artifacts.dir` in `config.yaml`,
`~/.local/state/grove/artifacts` unless you changed it, laid out as `jobs/<job id>/<path>`
(directories 0700, files 0600). Nothing else serves or holds them: `grove serve` lists them,
streams them through `GET /api/v1/jobs/{id}/artifacts/{path}` and deletes them once their
retention runs out (docs/JOBS.md "Artifact retention"); `grove sandbox run` writes the ones it
copies out of sealed VMs. Only sealed pools' builds produce artifacts today.

- **Disk use**: at most 2 GiB per job, gone 15 min / 1 h after the job ends. `du -sh
  ~/.local/state/grove/artifacts` shows what's there now.
- **Moving it**: stop `grove serve`, move the directory, `grove config set artifacts.dir <new>`,
  start `grove serve` again. Jobs running at that moment lose their artifacts.
- **Upgrading from MinIO**: re-run `grove install --role control-plane`. It stops and removes the
  old `com.grove.minio` / `grove-minio` service and drops the S3 keys from `config.yaml`. Then
  delete what MinIO left behind yourself: `rm -rf ~/.config/grove/minio` (its data, root
  credential and policy file), plus `~/.local/bin/minio` and `~/.local/bin/grove-mc` on Linux, or
  `brew uninstall minio/stable/minio && brew untap minio/stable` on macOS. Artifacts stored in
  MinIO are not migrated; they were due to expire within the hour anyway.

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
