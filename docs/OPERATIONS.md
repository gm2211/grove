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

## Nomad mutual TLS and client introduction

ACLs gate Nomad's API, not who may register as a client node. Without more, anything that reached
the server's RPC port (4647) — any tailnet device, or a job inside a VM, through the host's NAT —
could start `nomad agent -client` with `meta { pool = "linux" }` and be handed real jobs, with
their payloads. So `grove install --role control-plane` also:

- **creates grove's own CA** in `~/.config/grove/nomad/tls/` (`internal/pki`; keys mode 0600, the
  CA key never leaves the control plane) and certificates for the server (`server.global.nomad`
  plus the address it binds), for grove itself (`cli.pem`, which `nomad.clientCert` points at),
  and for the control plane's own sandbox-host client (`node.pem`, `client.global.nomad`). VMs
  never get that one: each gets a certificate of its own at enrolment (below);
- **turns on mutual TLS** for Nomad's RPC and HTTP, with `verify_server_hostname` (a client
  certificate cannot pose as a server) and `verify_https_client` (the HTTP API answers only
  CA-signed callers, on top of the ACL token). `nomad.url` becomes `https://`;
- **enforces client introduction strictly**: a node registers only with an introduction token
  bound to its node name and node pool, minted by grove. Once registered, a node authenticates
  with the identity the server issued it, so a reboot rejoins without a new token;
- **runs the control plane's sandbox-host client as its own agent** (`com.grove.nomad-sandbox-client`
  / `grove-nomad-sandbox-client`, config `~/.config/grove/nomad/sandbox-client.hcl`, node name
  `grove-sandbox-host`), because the server holds every client to introduction, its own included.
  The install mints its token and restarts it.

Nothing a VM's Nomad client needs to authenticate is in its Orchard spec, which every Orchard
worker and API caller can read. A shared VM's startup script carries only the CA certificate and
the TLS config, then waits (up to 30 minutes, `# grove:nomad-enrol wait`) before restarting Nomad.
Each reconciler tick in `grove serve`, the fleet **enrols** every running shared VM that needs it
over Orchard exec (`sudo -n /bin/sh -c …`, with everything secret on stdin, never argv). Only a
VM exactly as the reconciler would create it qualifies (its pool's image and scripts, on the worker
its name pins it to): any worker's Orchard credential can create VMs, and one that merely looks
like grove's gets nothing. Each enrolled VM gets:

- a client certificate of its own (`client.global.nomad`, which Nomad's role check needs, plus
  the VM's name as a DNS name and OU), valid **24 hours** and renewed after 12, with a new key each
  time. A renewal is written over the old files and Nomad is sent `SIGHUP`, which reloads them and
  reconnects (an established connection outlives its certificate; a new handshake does not). A
  certificate copied out of a VM is useless within a day;
- while its Nomad client isn't registered, an **introduction token** bound to the VM's name and
  to its pool's **node pool** (valid 30 minutes, Nomad's maximum; re-minted if the VM hasn't
  registered by then). The VM's Nomad config names that node pool, and the pool's jobs are pinned
  to it (`node_pool` in `grove-<kind>-<pool>`), so a VM can only ever be handed its own pool's
  jobs: unlike `meta.pool`, which a node sets for itself, the node pool is checked against the
  token by the server. grove creates the node pools; `default` and `all` are Nomad's own, so no
  fleet pool may be named either.

Enrolment state lives in `grove serve`'s memory: after a restart it simply enrols every VM again,
which renews its certificate. `grove fleet apply` creates VMs but doesn't enrol them; the running
`grove serve` does, within a tick. A VM that is never enrolled fails its startup script after 30
minutes, and Orchard's restarts then hand it to the reconciler's boot-loop rebuild.

Upgrading an existing control plane is re-running the installer. Nomad restarts onto TLS, the
server's old built-in client goes down (the reconciler purges it like any down node), and the
reconciler recreates every VM: a VM made before TLS can no longer register, so its startup script
reads as drifted. Jobs queue until the new VMs are up.

What each refusal looks like in a client's log, for debugging a VM that never shows up in
`nomad node status`:

| Log line | Meaning |
| --- | --- |
| `rpc error: stream closed` on `Node.Register` | the client has no TLS config (an old image or startup script) |
| `x509: certificate signed by unknown authority` | the client trusts another CA: the control plane's CA was recreated, so recreate the VM |
| `rpc error: Permission denied` on `Node.Register` | no introduction token, an expired one (older than 30 minutes), or one minted for another name |
| `introduction claims mismatch` in the server's log | the token names another node pool than the client's config: the VM is not the pool it claims to be |
| `tls: expired certificate` | the VM's certificate wasn't renewed: is `grove serve` running, and does `serve: fleet reconciler: enrolling VMs into Nomad failed` say why? |
| startup script fails with `the control plane never enrolled this VM into Nomad` | the same, for a VM's first enrolment |

`grove doctor` has a `nomad-tls` row: it fails when Nomad runs without mTLS and when a
certificate is within 30 days of expiry. grove's CA lasts ten years and the control plane's own
certificates two; renewing is re-running the installer (a new CA makes the fleet recreate its VMs).
VMs' certificates renew themselves, as above.

## The macOS guest's admin password

Tart's macOS images boot with `admin`/`admin`, and `admin` is a passwordless sudoer. A job that
knows that password (`su admin`, `ssh admin@localhost`, Screen Sharing, an `osascript … with
administrator privileges` prompt) is root in the VM, past the pf fence (which matches only the job
users) and able to read the Nomad node key, the introduction token and other jobs' secrets.

So every macOS VM gets its own admin password. Right before `CreateVM` the reconciler generates a
random one (24 bytes from `crypto/rand`), hands it to Orchard as the VM's `Password`, and fills it
into the first block of the startup script (`# grove:guest-password`). That block sets it through
`dscl .` on stdin (sudo logs argv, and `ps` shows argv to every user), then fails the script, so
the VM never takes a job, if `admin`/`admin` still authenticates. A planned spec never holds the
password, the API never returns it (`orchard.VM` has no password field), and drift detection
ignores its value. The only copies are in Orchard's VM record and the guest's own account
database.

Orchard's startup and shutdown scripts both log in with the VM's `Password`. On first boot the
guest still has `admin`, so the worker has to fall back to the image password. Workers built from
gm2211/orchard with that fallback advertise the `guest-password-rotation` capability. A worker
without it gets no macOS VM at all, because a VM left on `admin`/`admin` hands every job root:
`grove fleet plan` and `grove serve` report the macOS pool as blocked on that worker, and any macOS
VM already running there is deleted as `unsafe`. Linux VMs on the same Mac are unaffected. Once the
worker is upgraded, the fleet creates its macOS VMs on the next tick. To upgrade a worker, upgrade
grove on the Mac and re-run the worker install. It rebuilds Orchard at the pinned commit (see
"Upgrading") and restarts the worker:

```sh
brew upgrade grove && grove install --role worker
```

Linux pools keep the image password: their jobs run in containers that the egress fence keeps
off the VM's own addresses. A pool that sets its own `password` in fleet.yaml has already replaced
the image's, and is left alone.

A VM that reboots comes up at the login window, because the image's auto-login password no longer
matches. Nothing grove runs needs a GUI session.

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

## Where grove listens

grove is only ever reachable over the tailnet. `grove serve` binds `server.listen` from
config.yaml (default `127.0.0.1:6120`) and refuses to start if that address is a wildcard
(`0.0.0.0:6120`, `:6120`) or resolves to anything but loopback or a Tailscale address
(100.64.0.0/10, fd7a:115c:a1e0::/48). `grove doctor` checks the same thing (`grove-listen`). Don't
put grove behind Tailscale Funnel or a public reverse proxy.

```bash
grove config set server.listen 100.x.y.z:6120   # this machine's tailnet IP
```

A server with no operator token, device store or Tailscale auth configured answers every
`/api/v1` request with 503 rather than running open.

## The credential broker

A job never holds a real credential. It gets a placeholder (`gph1_…`), and each worker Mac runs
`grove broker`, the proxy its job VMs send traffic through (port 3128 on the VM bridge address,
192.168.64.1 unless vmnet is configured otherwise). For a request to one of the fixed upstreams
(github.com, api.github.com, api.anthropic.com, api.openai.com, chatgpt.com) the broker:

- terminates TLS with its own CA (`<grove config dir>/broker/broker-ca.pem`), which is
  name-constrained to those five hosts, so it can't be used to impersonate any other site;
- refuses the request unless the placeholder is live, issued for that service, and (when the entry
  names one) sent from the job's own VM;
- swaps the placeholder for the real token in `Authorization` (Bearer, token or Basic, which is
  how git sends it) or `x-api-key`, never anywhere else;
- masks that token in the response headers and body (as is, URL- or JSON-escaped, base64), and
  refuses an upstream reply in an encoding it can't read through.

Any other HTTPS is a plain tunnel and plain HTTP is relayed, both to public addresses only: the
tailnet, the LAN, the host and every other private or special range are refused.

Entries live in memory only. The broker pulls its worker's list every 30 seconds from
`GET /api/v1/broker/entries`, with a device credential whose only scope is `broker` and whose name
is the worker's (`grove access issue --name <worker> --scope broker`); no other credential,
operator's included, can read that route. An entry leaves when its job ends (the control plane
stops listing it), when its 10-minute lease runs out without renewal, or when the broker restarts.
At most 1,000 entries are held per worker.

Not wired in yet: dispatch doesn't create entries, VMs don't trust the CA or use the proxy, and
the worker install doesn't run `grove broker`. Those land separately.

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
```

Grant tags, not user logins. An untagged machine is identified by its owner's login, and a grant
to a login covers every machine that person owns. That includes worker Macs, whose shared-pool
jobs reach the control plane through the Mac's own network and so arrive with the Mac's identity.
For that reason grove refuses Tailscale identity auth from any Orchard worker's Mac (matched by its
MagicDNS name) whatever it is granted; a worker uses the device credential `grove setup` stored
in its Keychain. `server.tailscale.trustWorkerHosts: true` turns that check off, and with it lets
every job on a shared pool act with its host's scopes.

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
but on the control plane, whose sandbox-host Nomad client runs exactly these jobs (node meta
`grove_sandbox_host`; see "Nomad mutual TLS and client introduction"). There `grove sandbox run`:

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
   control plane after upgrading grove: it sets up and registers the sandbox-host Nomad client
   that runs sealed pools' jobs. `grove doctor` shows a `sandbox-host` row once a pool is
   sealed.

Then set `network: isolated` and let the reconciler recreate the pool's VMs (`grove fleet plan`
shows the change first). Re-running `grove serve` (the control-plane install reloads it)
registers the pool's jobs from the sealed template.

Recycling a sealed VM (`POST /api/v1/vms/{name}/recycle`, TTL, or drift) runs its shutdown
script, which marks the VM so no new job lands in it and waits for the running ones to finish.

Limits:

- The per-job `image` override is ignored (as on every pool).
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
- **Timeouts** are kept by the job's own `run.sh` in the VM (macOS has no `timeout(1)`), with the
  control plane as a backstop: a job past its timeout is stopped (TERM, then KILL after 10
  seconds) and reports 124 as usual.
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
- **Orchard** (the control plane's controller and every worker) is built from source at
  `OrchardForkCommit` (`internal/install/download.go`), fetched by SHA, never from the tip of
  gm2211/orchard's main. Each build records its commit: the worker bundle in
  `Contents/Resources/grove-orchard-commit`, the controller's binary in `orchard.grove-commit`
  beside it. When the pin moves, `grove install --role control-plane` / `--role worker` rebuilds
  and restarts the controller / worker. To move it, merge to the fork, then bump the
  constant here in a reviewed PR.
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
