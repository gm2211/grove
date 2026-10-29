# Operating grove

## Recycling / hygiene

Nothing long-lived is cleaned in place — it's replaced. See ARCHITECTURE.md's "Recycling /
hygiene" section for the full mechanism; the short version:

1. Every worker VM has an Orchard **TTL** (fork feature, e.g. `ttl: 12h` in `fleet.yaml`).
2. A couple of minutes before it expires, grove's reconciler drains the VM's Nomad node (2h
   deadline), so no new job lands on it. At expiry Orchard deletes the VM, running its
   **ShutdownScript** (fork feature) inside the guest first, which waits for zero allocations
   before letting the delete proceed. With Nomad ACLs on, the VM's own token is read-only, so it
   can't drain itself (or any other node); without ACLs the script still runs
   `nomad node drain -self` itself. Orchard never learns Nomad exists.
3. grove's fleet reconciler notices the VM is gone (or spec drifted) and recreates it from the
   pool's `image`.

To force a recycle right now (bad VM, want fresh image): `POST /api/v1/vms/{name}/recycle` (drains
via Nomad, then deletes; the reconciler recreates it). `orchard delete <name>` directly skips that
drain; the roll call (below) then drains the node within a tick, since its VM is gone.

## Nomad ACLs and node tokens

`grove install --role control-plane` runs Nomad with ACLs on. It bootstraps them once and keeps
the management token in `config.yaml` (`nomad.token`, file mode 0600); `grove serve` and the CLI
send it on every Nomad call. A request without a token (from the tailnet, or from inside a job)
gets nothing. An install made while ACLs were off is migrated by re-running the installer: it
re-renders `server.hcl`, restarts Nomad once, and bootstraps. If Nomad was already bootstrapped by
hand, set the existing management token with `grove config set nomad.token <secret>`.

With `nomad.token` set, the fleet reconciler also gives each VM it creates:

- **its own node token** (Nomad policy `grove-node-read`: `node` and `agent` read, nothing
  writable, no namespace access, named `grove-vm/<vm>`). It sits only in the VM's shutdown script
  environment, never on the VM's disk, so the script can watch its allocations finish and jobs
  never see it. Draining is the control plane's job: grove drains a VM's node before it deletes
  the VM and before its TTL, so a VM can't drain, mark ineligible or purge any node, its own
  included. Tokens minted under the old `grove-node` policy (node write) keep working until their
  VMs are recycled; the upgrade recreates them all. The reconciler revokes a VM's token once
  Orchard has removed the VM (`nomad acl token list` shows the live ones).
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

Every tick, `grove serve` also takes a **roll call** of Nomad's live, eligible nodes against
Orchard's VMs, the one record of what grove created (the control plane's `grove-sandbox-host` is
left alone):

| Finding | What grove does |
| --- | --- |
| a node whose name is no VM in Orchard (a copied identity, or a VM deleted behind grove's back) | drains it |
| two or more nodes claim one VM | drains them all and deletes the VM, so it is rebuilt with a fresh identity |
| a node outside its pool's node pool | drains it and deletes the VM |

A drained node stays ineligible for good, across restarts and re-registrations: Nomad never
places another job there. A purge would not do: a live node comes straight back, within seconds,
on the identity it holds (`internal/install/nomadtls_integration_test.go` shows both). Its running
jobs keep the usual 2-hour drain deadline, so a VM deleted with `orchard delete` doesn't lose
them. Each one is logged as `roll call took a Nomad node out of service`, with the reason. Once
the node goes down, the down-node sweeper purges it.

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
`dscl .` on stdin (sudo logs argv, and `ps` shows argv to every user), naming the old password as
well, since `admin` holds the image's SecureToken and macOS refuses even root that change without
it (`eDSAuthFailed`). It then fails the script, so
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

The password is not the only lock. Before any job can start, every macOS guest, shared or sealed,
also closes the ways a job user could use it (`macosLockdownScript`, `internal/fleet/lockdown.go`):

- **su and login** authenticate only a caller already in the admin group: a `pam_group ... ruser`
  line (`# grove`) goes ahead of the other auth modules in `/etc/pam.d/su` (after `pam_rootok`, so
  root still su's) and `/etc/pam.d/login`. `sudo`, which Orchard and grove use as admin, has its
  own PAM file and is untouched.
- **SSH**: the job users' pf anchor (`com.apple/grove-jobs`) refuses their TCP connections to port
  22 anywhere, the VM itself included. A sealed guest gets an anchor with just that rule.
- **Privilege prompts**: the `system.privilege.admin` authorization right (`osascript ... with
  administrator privileges`, `AuthorizationExecuteWithPrivileges`) is set to `deny`.

A step that fails stops the startup script, so the VM takes no job, and Orchard restarts it.

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

If workers log in to `ghcr.io` (rather than the packages being made public — see docs/IMAGES.md
"Packages are private by default"), rotate that token like any other credential:

1. On GitHub, revoke the old PAT (**Settings -> Developer settings -> Personal access tokens**) and
   mint a new one with the same minimum `read:packages` scope.
2. Copy it, then on each worker, as the macOS user the worker runs as:

   ```bash
   pbpaste | grove registry login --user <github-username>
   ```

   It always logs in again, even when the user hasn't changed: `tart login` overwrites the
   previous credential in that user's keychain, and grove rewrites the worker's own copy
   (`~/.config/grove/credentials/registry`, 0600) and restarts the running worker onto it. The
   token comes from stdin (or
   `$GROVE_REGISTRY_TOKEN`), never an argument. The marker
   (`~/.config/grove/registry-login.ghcr.io`) records only the user and registry, not the token or
   its age, so nothing nudges you to rotate on a schedule.
3. `grove doctor`'s `registry-login` check confirms the marker is present; it can't (and doesn't
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

Entries live in memory only, on the control plane and on each broker. A control-plane restart
forgets every token, and running jobs get them back when their submitter sends them again
(`PUT /api/v1/jobs/{id}/credentials`). The broker pulls its worker's list every 30 seconds from
`GET /api/v1/broker/entries`, with a device credential whose only scope is `broker` and whose name
is the worker's; no other credential, operator's included, can read that route. An entry leaves when its job ends (the control plane
stops listing it), when its 10-minute lease runs out without renewal, or when the broker restarts.
At most 1,000 entries are held per worker.

Entries come from job submissions: a job's `credentials` (docs/JOBS.md) become placeholders in
its env and entries in the control plane's memory, and a broker is served only the entries of live
jobs placed on its worker. They leave the control plane when the job ends or is canceled. Sealed
pools take no credentials, since their VMs block the host and so the broker.

`grove setup` installs the broker on every worker. When the control plane approves the Mac, it
hands out the broker credential along with the others; setup keeps it in a 0600 file,
`~/.config/grove/credentials/broker`, owned by the worker account (no Keychain: the broker starts
at boot, before anyone logs in), and stages the `com.grove.broker` LaunchDaemon, which the
`worker-daemons` step of the privileged checklist installs into `/Library/LaunchDaemons` with
`UserName` set to the worker account and loads. It runs `grove broker --server <control plane>`
and logs to `~/Library/Logs/grove/broker.log` and `broker.err.log`. A worker set up by an earlier
grove has the credential in Keychain (service `com.gm2211.grove.broker`, account `broker`); the
broker still falls back to that item, and re-running the install copies it into the file. Running
setup again replaces the credential, revokes the old one and restarts the broker. A Mac set up against a control plane
from before the broker gets a warning and a broker with no credential: it still serves its CA and
plain tunnels, every sync fails, and jobs there can't use credentials until setup runs again.

`grove doctor` on a worker has a `worker-daemon` row (the Orchard worker's LaunchDaemon is the one
grove renders, loaded in the system domain, with no leftover LaunchAgent) and a `broker` row:
whether the LaunchDaemon is installed and loaded, whether the credential file exists, is 0600 and
is the user's, and, once a VM has created the bridge, whether the broker answers and its last sync
with the control plane worked, with its entry count.

Not wired in yet: shared VMs don't trust the CA or use the proxy. That lands separately.

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

`--expires-in 180d` (or any Go duration, `"expiresIn": "4320h"` over HTTP) makes a credential stop
working after that long. `grove access list` shows the date, and its holder sees it in
`GET /api/v1/whoami` (`expiresAt`), so it can ask for a new one in time.

### Argos on the control-plane Mac: the local socket

When the orchestrator runs on the control-plane Mac itself (Argos on the Studio), it shouldn't
use Tailscale identity: that Mac is usually also a worker, and its job VMs reach the network
through its address. Give it the local socket instead, and an expiring token on top:

```bash
grove config set server.socket /Users/Shared/grove/api.sock
grove config set server.socketUsers giulio      # the macOS user Argos runs as
# restart grove serve, then:
grove access issue --name argos --scope read --scope dispatch --expires-in 180d
```

`grove serve` then also listens on that socket. Requests on it never touch the network, so
nothing on the tailnet and no job VM can reach them. Only the listed users can connect: grove
checks the user the kernel reports for each connection, not the socket's file mode, and closes
anyone else's connection unread. Every request on the socket still needs a bearer token, and
Tailscale identity counts for nothing there. So if Argos ever moves off this Mac, it carries
its token with it and is never left with no authentication.

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

The job itself does hold the real value, so prefer a per-job credential for anything the broker can
swap (see "The credential broker" above): there the job holds only a placeholder. Argos sends its
own Claude sign-in that way, so a fleet it dispatches to needs no `CLAUDE_CODE_OAUTH_TOKEN` secret
here. Named secrets stay the way for the rest: Codex's `auth.json`, which its CLI parses instead of
sending as a header, and any value whose service the broker does not know.

```bash
# on any machine with the operator credential; the value comes from stdin or an env var, never argv
# (only for a dispatcher that does not send its Claude sign-in as a credential)
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

No worker VM joins the tailnet. A shared VM's Nomad client dials the control plane's RPC
address (port 4647 on the host of `nomad.url`, which the control plane resolves to an IP, since
guests resolve names through public resolvers only) through its host Mac's NAT, and the host's
own Tailscale carries it the rest of the way. So the tailnet's ACLs must let each worker Mac reach
the control plane on 4647; nothing needs to allow VMs as tailnet devices any more, and
`tailscale.authKey` is no longer used (`grove config set` refuses it). The startup script turns
the image's Tailscale daemon off. To get a shell in a VM, use `orchard ssh vm <name>` from the
control plane rather than tailnet SSH.

A worker VM shares its host Mac's network through Tart's NAT. Left to that alone, the VM could
reach whatever that Mac reaches: your LAN, services the Mac itself runs, and every device on your
tailnet, through the Mac's own Tailscale login. The worker Mac's own firewall (below) stops the
VM itself from doing so. Its jobs are held back twice: a firewall in the guest
(`grove-egress-fence` in DOCKER-USER for Linux containers, the `com.apple/grove-jobs` pf anchor
for the macOS job user) refuses every private, tailnet and link-local address, with exactly one
exception, TCP to the default gateway the guest got from DHCP on port 3128: the host Mac's
credential broker, which listens only on that address. The gateway is read each time the fence
loads; a guest with no IPv4 default route gets no exception.

At boot a shared VM fetches the broker's CA from `http://<gateway>:3128/ca.pem` and keeps it only
if it is a single CA certificate (pathlen 0) whose critical name constraints permit only the
broker's hosts (github.com, api.github.com, api.anthropic.com, api.openai.com, chatgpt.com, or
some of them) and exclude every IP address; even a forged CA that passes can vouch for no other
site. The VM then
advertises the broker as Nomad node meta (`grove_broker`, `grove_broker_ca`, in
`grove-broker.hcl`), and each build/agent/shell job's `run.sh` sets `HTTPS_PROXY`/`HTTP_PROXY` to
it and `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE` and
`NODE_EXTRA_CA_CERTS` to the system roots plus that CA. Tools that trust only the macOS keychain
won't accept the broker's certificates. With no broker (none answering within two minutes of
boot, or a CA that fails the check, reported in the startup script's output) jobs run without
one, and a job whose environment holds `gph1_` placeholders says so in its log. After installing
the broker on a worker, recycle its VMs so they pick it up.

`network: isolated` seals a pool's VMs instead:

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
  grove drives each job. Softnet's rules match addresses, not ports, so there is no hole for the
  host's credential broker: a sealed job gets no broker credentials.
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

### The worker Mac's own firewall

The guest fences run inside the VM, so a job that gets root in its VM could take them down. The
`com.apple/grove-vm` pf anchor on each worker Mac holds the same line from outside, for every VM
on vmnet's NAT bridges (`bridge100`–`bridge109`), shared or sealed, Softnet or not. A VM reaches
the internet, and of the Mac and its networks only:

- DHCP (UDP 67) and DNS (UDP/TCP 53) on the vmnet gateway,
- the credential broker on the gateway, TCP 3128,
- Nomad RPC, TCP 4647 on any address: the control plane, on this Mac or across the tailnet.

Everything else bound for a private, tailnet, link-local or multicast address, or for any of
the Mac's own addresses, is refused, and so is all IPv6 apart from neighbour discovery. The Mac's
own connections to its VMs (Orchard's SSH, port forwards) still work, because their replies match
the state they opened.

`grove install --role worker` stages the LaunchDaemon `com.grove.pf-vm` in
`~/.config/grove/pf/` and prints the privileged step that installs it, which an admin runs once
per Mac, and again whenever an upgrade changes the rules (`grove doctor` says so):

```
sudo install -o root -g wheel -m 0644 ~/.config/grove/pf/com.grove.pf-vm.plist /Library/LaunchDaemons/com.grove.pf-vm.plist \
  && { sudo launchctl bootout system/com.grove.pf-vm 2>/dev/null; sudo launchctl bootstrap system /Library/LaunchDaemons/com.grove.pf-vm.plist; }
```

The LaunchDaemon carries its whole loader, rules included, and runs only `/bin/sh` and
`/sbin/pfctl`, so nothing this user (or Homebrew) can rewrite runs as root. It loads the anchor
at boot and every five minutes, which also refreshes the Mac's own addresses, reloads
`/etc/pf.conf` only if pf has lost its `com.apple/*` hook, and turns pf on if it's off. The
`host-firewall` check in `grove doctor` passes when the installed plist is the one this grove
renders and its last run loaded the anchor. To look at the live rules:
`sudo pfctl -a com.apple/grove-vm -s rules`. To take it out:
`sudo launchctl bootout system/com.grove.pf-vm && sudo pfctl -a com.apple/grove-vm -F all`.

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
- **Restart a supervised service** after a config change: macOS control plane —
  `launchctl kickstart -k gui/$(id -u)/com.grove.<name>`; macOS worker (its Orchard worker and
  broker are system LaunchDaemons) — `sudo launchctl kickstart -k system/com.grove.<name>`, or,
  without sudo, as the worker user, `kill -TERM` the `pid` that
  `launchctl print system/com.grove.<name>` shows (KeepAlive starts it again); Linux —
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
