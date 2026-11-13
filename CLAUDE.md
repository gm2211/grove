# grove — repo conventions

grove turns Macs (and a Linux box) you already own into a private CI/agent cloud. See
[ARCHITECTURE.md](ARCHITECTURE.md) for the design; this file is about how the repo itself is
organized and how to work in it.

## NEVER store a login password

Never store, set up, or suggest storing anyone's login password: not the operator's, not a worker
user's (e.g. `argos`), not anywhere on disk, in a config file, a Keychain item you create, or an
environment variable. That includes auto-login setups and anything like the removed
`grove worker remember-login` (the wrapper deletes its old file). Do not run them, recommend them,
or write new code or docs that steer people to them. When something
needs a login (e.g. a macOS VM needing the worker user's unlocked login keychain), offer only
routes that keep the password out of storage, and state their cost: the Map's in-memory "Unlock
keychain" (redo after every reboot), logging the user in, or not using that feature. The
operator's standing rule, set 2026-11-11.

## Layout

```
cmd/grove/                 main.go — cobra entrypoint, calls cli.Execute()
internal/cli/               one file per command; each registers itself on cli.Root via init()
internal/config/             ~/.config/grove/config.yaml (CONTRACT — see below)
internal/orchard/            Orchard client interface + impl (github.com/cirruslabs/orchard fork)
internal/nomad/               Nomad client interface + impl (github.com/hashicorp/nomad/api)
internal/fleet/               fleet.yaml spec → desired VMs; reconciler
internal/dispatch/            JobRequest → parameterized Nomad job; status; logs
internal/server/              HTTP API (/api/v1/…) + embedded UI; token + Tailscale-identity auth
internal/tsauth/              Tailscale identity (tailscale whois on the peer address) → scopes
internal/secrets/             operator-set named job secrets (JobRequest.Secrets)
internal/pki/                 grove's Nomad CA: server, grove-client and per-VM node certificates (mTLS)
internal/gitauth/             operator-armed private-repo credential (see below)
internal/grant/               allow-list syntax (principal id, tag:<name>) shared by gitauth and secrets
internal/registryauth/        operator-armed registry pull login for Linux pools' images (see below)
internal/broker/              per-worker credential broker: placeholders in, real tokens out (see below)
internal/health/              rebuilds VMs / pauses Macs that keep losing sealed-pool jobs
internal/mcp/                 MCP server (stdio)
internal/conformance/         Go tests that pin the spec/ models to the real packages
internal/install/             role bootstrap planner + doctor checks (see below)
ui/                            Vite + React + TS SPA
images/                         Packer templates
nomad/jobs/                     parameterized Nomad job HCL
scripts/install.sh              curl | sh → brew tap + grove install
docs/                            INSTALL.md, OPERATIONS.md
spec/                            22 properties: Quint models, Lean proofs, CodeQL queries, Specify spec (spec/grove.spec); spec/check.sh (see spec/README.md)
```

**Contract packages** (`config`, `orchard`, `nomad`, `dispatch`, `fleet` *types*) define the Go
interfaces every other package builds against. Implementations may change; extend interfaces
additively — don't break the seam other in-flight work depends on.

## Adding a CLI command

One file per command under `internal/cli/`, registering itself on `Root` from an `init()`:

```go
package cli

import "github.com/spf13/cobra"

var fooCmd = &cobra.Command{
	Use:   "foo",
	Short: "...",
	RunE:  runFoo,
}

func init() { Root.AddCommand(fooCmd) }
```

Don't edit `root.go` to wire up a new command — it should only ever need new files added beside
it.

## `internal/install` — the role bootstrap planner

Everything that could touch the host (Homebrew, launchd/systemd, sudo, tailscale) goes through the
`Runner` interface (`Run(ctx, name, args...) (stdout, stderr string, err error)`) — never call
`os/exec` directly from step logic. `ExecRunner` is the real implementation; `FakeRunner` (records
calls, returns scripted results) is for tests. This is what makes `--dry-run` actually mean
"nothing happens", and what makes the planner testable without a real Mac.

- `BuildPlan(runner, opts, out) ([]Step, error)` assembles the step list for a role.
- Each `Step` has `Check` (idempotency test) and `Apply` (do the thing); `Privileged` steps only
  ever `Apply` for real when running as root with `--yes`. Otherwise `RunPlan` lists them as
  `[root]` and `RunRootSteps` (`root.go`) runs their `Root` texts as ONE script through ONE
  `sudo /bin/sh -e -c` (admin user, terminal and a yes, or `--yes` with cached credentials), or
  prints that script as a single heredoc block for an admin (`--no-sudo`, not an admin, declined).
  A `Root` text has no `sudo`, only absolute system binaries, every path through `shQuote`, never a
  secret, and anything to resolve (softnet's realpath) is resolved at plan time through `Runner`.
- `RunPlan` walks the list, never stopping early on a failed step, and returns the first error
  alongside the full per-step results.
- Templates (launchd plists, systemd units, Nomad/Orchard config) live in `render.go` as
  `text/template`s over typed spec structs — keep them deterministic (no map iteration order
  surprises; Go's `text/template` sorts map keys, so that's safe) so golden tests stay meaningful.
- A worker Mac's Orchard worker and credential broker are **LaunchDaemons that run as the
  installing user** (`daemon.go`), so they start at boot with nobody logged in and the install
  works from `su - <user>`. Like `hostfirewall.go`, the unprivileged install only *stages* each
  plist (`daemon-stage:*`, under `~/.config/grove/launchd`); one privileged `worker-daemons` step
  installs them root-owned into `/Library/LaunchDaemons` and loads them into the system domain.
  Keep it that way: every daemon plist carries `UserName` and never root, and `SessionCreate` (its
  own security session, so `tart run` never joins the console user's session) (`daemonPlist`,
  `Options.workerUser`; the install refuses a root user, and the printed command re-checks the
  root-owned copy is byte-identical to the rendered plist, by SHA-256, before loading it); never `launchctl bootstrap gui/…` for these;
  restart one with `restartDaemon` (SIGTERM its pid, KeepAlive does the rest), not `kickstart`.
  The installed plist is compared with what grove renders, never with the staged copy.
- The worker runs Tart only through the grove copy (`tartheadless.go`: `Grove Tart.app`, LSUIElement,
  ad-hoc signed with only the virtualization entitlement, first on the wrapper's PATH; the wrapper
  fails closed with exit 78). Never put Homebrew's `tart` ahead of it.
- A worker's secrets (Orchard bootstrap token, broker credential, registry login) are 0600 files
  in `~/.config/grove/credentials` (`credfile.go`: atomic write, refuses symlinks and foreign
  owners), because a boot-time daemon has no unlocked login Keychain. The Keychain items of an
  older grove are only a migration source. A secret never appears in argv, a plist, the wrapper's
  text, a step description or a log: the wrapper reads the files (`< file`, or `read` into
  `TART_REGISTRY_USERNAME`/`TART_REGISTRY_PASSWORD`). `daemon_test.go` asserts it. The registry
  login is fetched from the control plane at start instead when it holds one covering the worker's
  VM images (see `internal/registryauth`); the file is then only the fallback.
- The worker user's login password unlocks its keychain (macOS VMs need it) one way only, into the
  worker daemon's own session, never stored: typed into the panel (`POST /workers/{name}/unlock-keychain`:
  control-plane memory, then the broker's `GET /broker/entries` answer, then the FIFO
  `credentials/keychain-unlock.fifo` that only the wrapper reads, then `security -i` on stdin).
  The wrapper deletes `credentials/login-keychain`, an older grove's stored password, unread.
  Keep it that way: never a new route for a broker credential (spec S5), never a file or log on
  the way, and `DeliverKeychainUnlock` writes only into a FIFO this user owns.

Adding a step: add it to the relevant `build<Role>Steps` function in `worker.go` /
`controlplane.go` / `client.go`, keep `Check`/`Apply` going through `Runner`, and add a
planner test in the matching `_test.go` asserting the FakeRunner sees (or doesn't see) the
commands you expect.

## `internal/gitauth` — private-repo sourcing

Grove clones public repositories with no credentials at all. Private ones need a token, and the
only way one exists is an operator arming it at runtime through the control plane (`grove github
enable`, `PUT /api/v1/github/sourcing`) — there is no config.yaml key and no environment variable
for it, deliberately.

- `gitauth.Store` persists the armed credential (mode 0600, atomic rename, next to `devices.json`)
  and answers `TokenFor(repoURL)`. It is the `dispatch.Options.RepoAuth` implementation.
- `grove serve` builds ONE store and hands the same pointer to both the dispatch service and the
  HTTP server, so arming/disarming takes effect on the next dispatch with no restart.
- The token goes into the build/agent dispatch PAYLOAD (`grove-payload/1`, token line, script)
  and nowhere else. Only the job's `source` prestart task receives the payload: it clones, then
  deletes it, so the caller's script never sees the token in its env, argv or files. Never put it
  in `env_json` or any dispatch meta (every task sees those), onto `Job.Request.Env`, into the
  on-disk job history, or back out of the API (`Status` carries only a fingerprint). Keep it that
  way — `internal/dispatch/repoauth_test.go`, `nomad/jobs/nomadjobs_test.go` and
  `internal/server/handlers_github_test.go` assert it.
- Arming always names repositories (`--repo owner/name` or `owner/*`); an empty scope covers
  nothing.
- A dispatch scope is not enough to get the token: every worker Mac holds one, and a worker can
  read its own VM. `Store.TokenFor(repo, caller)` gives it only to an operator or a principal on
  the armed `Allow` list (`--allow`, the `internal/grant` syntax named secrets use; empty means
  operators only). The caller is `JobRequest.Caller`, set only by `handleSubmitJob` from the
  authenticated principal (`json:"-"`, not kept on the record); the zero caller gets nothing, and
  there is no caller-less way to ask. A refused submitter's job is dispatched with no token, never
  rejected, so it can't probe what is armed. Redispatch reuses the first dispatch's payload and
  never asks again.
- Nothing is armed forever: `Enable` always sets an expiry (`DefaultTTL` 24h, `MaxTTL` 30 days).
  An operator may arm it with `--renew gh`: `grove serve` then swaps in the token `gh auth token`
  prints for its own user at half-life (`Store.Renew`, `internal/cli/serve_github_renew.go`),
  keeping repos, allow list and TTL. Renewal never arms: only an enabled, unexpired credential
  armed to renew is touched, and the token goes from gh's stdout to the store, never argv, env,
  a file or a log.
  The one exception to both rules is a version 1 `github.json` (`adoptLegacyLocked`): any
  submitter, for at most `LegacyGrace` (24h) after first load, warned in `Status` and the log.
  Never widen it, and never write a state without `Version: stateVersion`.
- Only `https://` repo URLs match: a token does nothing for an SSH remote, so handing one over
  there would be a leak with no upside.

## `internal/registryauth` — pulling private job images

Linux jobs run in containers the VM's docker pulls (the pool's `runnerImage`, a job's environment
image). A private image needs a login, which exists only when an operator arms one at runtime
(`grove registry pull enable`, `PUT /api/v1/registry/pull`); no config.yaml key, no environment
variable. Same discipline as `internal/gitauth`:

- `registryauth.Store` persists it (0600, atomic rename, `registry-pull.json` next to
  `devices.json`; symlinks and foreign owners refused through `internal/secfile`). `grove serve`
  builds ONE store for the HTTP API, the dispatch service (`Resolver`, tag resolution) and the fleet
  reconciler (`Options.RegistryPull`, enrolment). `Status` and the API carry a fingerprint, never
  the token. Arming always names repositories.
- The token has exactly three destinations (the third is the worker Mac's host-side pull, below): the Docker `config.json` that `Reconciler.Enrol` writes
  to `/etc/grove/registry/config.json` on a Linux VM (root, 0600, over Orchard exec, **stdin only**:
  the payload's registry line, base64), and the `Resolver`'s requests to the registry it was armed
  for (never another host: a token realm outside the registry host, or plain http, is refused;
  redirects are never followed). Never argv, a VM spec or planned spec, a startup script, a log, an
  API response, `env_json`, dispatch meta or payload, or the job record. The job spec has no
  `auth` (`nomad/jobs/nomadjobs_test.go`). `spec/codeql/RegistryTokenLeak.ql` and the coverage row
  assert it.
- A worker Mac's Orchard worker pulls the pool's Tart image on the HOST before any VM exists, where
  the broker never sees it. `Store.LoginForVMImages` gives the login to the worker through the
  broker credential's existing answer, `GET /broker/entries?vmImagePull=1` (spec S5: no new route;
  the plain poll never carries it): only to that worker's own broker-scoped, machine-bound
  credential (never a job, VM, device or operator identity), only when every VM image of the
  pools that select it (`fleet.Spec.VMImagesFor`, Orchard's labels for the worker) is on the armed
  registry host (Tart presents `TART_REGISTRY_*` to any host; a name with no "/" is a
  local VM name Tart clones without a registry, so it is ignored; any other name without a
  parsable registry host, e.g. "internalreg/foo", withholds the login) and at least one is in scope. The
  worker wrapper runs the hidden `grove broker pull-login`, which prints user and token on stdout
  into a pipe the wrapper `read`s, and exports `TART_REGISTRY_*` only into the Orchard process:
  never a file, argv, plist, wrapper text or log (the wrapper writes only the non-secret
  `registry-source` marker `grove doctor` reads). The legacy 0600 `credentials/registry` file is a
  fallback only. `grove registry pull scope --add/--remove` (`PATCH /registry/pull`, operator-only,
  unknown fields such as a token refused) edits repos without the token.
  `internal/server/handlers_broker_vmpull_test.go` and `internal/install/workerwrapper_registry_test.go`
  assert it.
- A VM gets a login only when its pool's `runnerImage` or `environments.allow` names a repository
  in scope (`Store.PullConfig`); otherwise `{"auths":{}}`. The file must always exist because the
  docker plugin points at it, so the startup script creates an empty one (`O_EXCL`, never clobbering
  an enrolled one). Enrol keeps the pushed config's fingerprint and re-pushes only on change.
- Nomad **replaces** a later `plugin "docker"` block's config instead of merging it. The startup
  script therefore writes the complete block (`linuxDockerPluginBlock`) to `grove-docker.hcl`; never
  an `auth`-only drop-in, which would re-enable volume mounts. A test keeps the block equal to
  `images/linux-worker/files/client.hcl`.
- `dispatch.Service.Submit` pins an `environment.image` tag (`pinEnvironment`) before anything else
  reads it: allowed repository first, then `ImageResolver.ResolveTag`, then the usual digest
  validation. A failure rejects the job naming the image; never fall back to the tag.
  `internal/dispatch/environment_tag_test.go` and `internal/registryauth/resolve_test.go` assert it.

## Nomad mTLS and client introduction

Only the control plane may tell a node what to do, so only nodes grove created may join Nomad.
The control-plane install creates grove's CA (`internal/pki`, under `~/.config/grove/nomad/tls`)
and runs Nomad with mutual TLS on RPC and HTTP and `client_introduction { enforcement = "strict" }`.
A shared VM's spec carries only the CA. Once it runs, `Reconciler.Enrol` (`internal/fleet/enrol.go`,
called every `grove serve` tick) pushes in, only for a VM on a worker Orchard lists, that its pool's
selector matches and that Plan wouldn't block (spec S3), over Orchard exec with the secrets on stdin, its own
24-hour client certificate (`pki.CA.IssueNode`, renewed after 12 with a SIGHUP) and, until it
registers, an introduction token bound to its name and its pool's Nomad node pool; the pool's jobs
are pinned to that node pool. The control plane's own sandbox-host client is a separate agent for
the same reason. A VM's Nomad ACL token is read-only (`grove-node-read`): the control plane drains
nodes (before deleting a VM, before its TTL), and `Reconciler.RollCall` (`internal/fleet/rollcall.go`)
drains every live node that no Orchard VM accounts for, that shares its VM with another node, or
that sits outside its pool's node pool. Drain, never purge: a purged live node re-registers. Keep it that way: never put a node key or an introduction token into a VM spec
or onto an exec's argv, never render a Nomad config without the `tls` block, never let a client
register on a certificate alone, and keep `internal/install/nomadtls_integration_test.go` (real
Nomad, run in CI) proving that a client with no TLS, another CA's certificate, no introduction
token, or a token for another node pool is refused, and that a renewed certificate is what a
reloaded client handshakes with. Details: docs/OPERATIONS.md "Nomad mutual TLS and client introduction".

## macOS guest admin password

Tart's macOS images boot with `admin`/`admin`, a passwordless sudoer, so a job that knows the
password is root. Every macOS VM therefore gets its own random admin password, generated right
before `CreateVM` (`internal/fleet/guestpassword.go`, the `@GROVE_GUEST_PASSWORD@` placeholder,
like intro tokens: a planned spec never holds one). The value goes into the VM spec's `Password`
and into the startup script's first block. That block sets it via `dscl` stdin, never argv, and
fails closed if `admin`/`admin` still works. Every macOS guest also locks down before any job
(`internal/fleet/lockdown.go`): su and login gated to admin-group callers (pam_group), the job
users kept off SSH (pf), and `system.privilege.admin` denied. A worker that doesn't advertise
`guest-password-rotation` (the gm2211/orchard fallback to the image password) gets no macOS VM:
the pool is reported blocked there and any existing VM is deleted as unsafe. Keep it that way:
never put the password on a command line, in the API or in a planned spec. Details:
docs/OPERATIONS.md "The macOS guest's admin password".

## `internal/secrets` — named job secrets

Same discipline as the private-repo token: a value is set only by an operator (`grove secret set`,
value from stdin or an env var, never argv), lives only in `secrets.json` (0600, atomic rename),
and reaches a job only through that dispatch's PAYLOAD (`grove-payload/2`: token line, one line
holding base64 of the secrets' JSON, script; `internal/dispatch/payload.go`). Never `env_json` or
any other dispatch meta (every task sees those; `env_json` holds only the caller's `Env` and
credential placeholders), never on `Job.Request` (which keeps the names), never in the job
history, never in an API response (`Secret` carries a fingerprint). Keep it that way:

- build/agent: only `source` receives the payload. It writes the secrets line to
  `$state/secrets` (0400, created exclusively, in `finish`, only when the job will run; on macOS
  handed to `_grovejob` with the rest of the state directory), and `main`'s run.sh exports them
  and deletes the file before the caller's script starts. A file it can't read or delete fails
  the job; never run the script without its secrets.
- shell: `main` is the payload's only reader and parses the frame itself (a shell script may not
  start with `grove-payload/`). Sealed pools: `grove sandbox run` reads the payload
  (`sandbox.Request.ApplyPayload`) and the secrets enter the VM only in the bundle's `env.sh`.
- The export goes through `jq` `@sh` and `eval`, like `env_json`; never interpolate a value into
  shell text any other way. Version 2 is sent only for a job that names a secret, and every
  reader still takes version 1 and a bare script.
- A redispatch reuses the payload it reads back; `secretsOutOfMeta` moves the secrets of a job an
  older grove dispatched out of `env_json` first. `Submit` refuses a payload over Nomad's 16 KiB.

Authorization (who may name which secret) is checked in `internal/server` where the principal is
known; `internal/dispatch` only resolves. `internal/dispatch/secrets_test.go`,
`nomad/jobs/secrets_test.go`, `internal/sandbox/secrets_test.go`,
`internal/secrets/secrets_test.go` and `internal/server/tailscale_auth_test.go` assert it.

## `internal/broker` — jobs never hold a real credential

A job gets placeholders (`gph1_` + 256 random bits); the broker on its worker swaps them for real
tokens on the way to a fixed set of upstreams and masks the tokens on the way back. Keep it that way:

- The hosts a kind's token may go to are fixed in code (`kindHosts`), never configured.
- Swap only in `Authorization` and `x-api-key` (`injectHeaders`), never in a body, URL or other
  header: anything else lets a job make an upstream store or echo the token.
- Never follow redirects, never add HTTP/2 or protocol upgrades on intercepted hosts, and never
  pass on a response the stripper can't read (an unrequested `Content-Encoding`).
- Tunnels and plain HTTP go through `Dialer`, which dials only public addresses it resolved itself.
- Entries are in memory only, on both sides (`Registry`, `Store`), with a lease. Only a
  credential whose single scope is `broker` reads `GET /broker/entries`, and only its own worker's.
- A placeholder works only from its job's VM (spec S8). The control plane sends the VM's name;
  the worker's broker resolves its bridge address on every sync (`Syncer.bind`, `TartSources` in
  `source.go`: `tart list`/`tart ip` through `Runner`, on the Grove Tart shim by absolute path) and
  installs the entry with that `Source`. Never install an entry it couldn't bind (no VM name, no
  address yet, an ambiguous one), never trust a `Source` from the control plane, and never let
  `Store.Lookup` treat a missing `Source` as "any address".
- In `internal/dispatch`, a `JobRequest.Credentials` token goes only into the registry: never
  `env_json`, the payload, the stored `Job.Request` (kind and env only) or an API response.
  `internal/dispatch/credentials_test.go` asserts it.
- Only join approval issues a `broker`-scoped credential (`AccessStore.IssueJoinedBound`; `Issue`,
  so `POST /access/devices` and `grove access issue`, refuses the scope: the operator token must
  not mint the credential that reads jobs' tokens), bound to the joining machine's verified
  address and Tailscale node (`Binding`); `GET /broker/entries` refuses it from anywhere else
  (`brokerPeerAllowed`), and a join under an existing worker's name from a machine its credential
  isn't bound to needs `grove join approve --replace` (`joinReplaces`). The control plane's own
  worker gets its credential through a `brokerOnly` join (`internal/cli/install_broker.go`), never
  another route. `internal/server/broker_binding_test.go` asserts it.
- The broker's own credential comes from join approval (`handlers_join.go`) and lives in a 0600
  file under `~/.config/grove/credentials` (`config.ResolveBrokerToken`; the login Keychain item
  of an older grove is only a migration source and read fallback), never in arguments or the
  LaunchDaemon plist (`internal/install/broker.go`); `/healthz`, which job VMs can reach, says
  only the entry count and whether the last sync worked.

`internal/broker/proxy_test.go` drives the whole path end to end; keep its refusal cases passing.

## The worker Mac's VM firewall

`internal/install/hostfirewall.go` renders the `com.grove.pf-vm` LaunchDaemon. It loads the pf
anchor `com.apple/grove-vm`, which lets VMs on vmnet's bridges reach only DHCP, DNS, the broker
(3128) on the gateway, and Nomad RPC (4647) on the control plane's address. Installing it is a
privileged step, printed for an admin. Keep it that way:

- The Nomad RPC pass names addresses, never `any`: the control plane's, resolved at plan time
  through `Runner` (`resolveNomadTargets`: `nomad.url` in this user's config.yaml, else the grove
  server, else the Orchard controller; `self` when that is this Mac). A name that does not resolve
  fails the step and the doctor check; never fall back to any address.

- The plist carries its whole loader inline and runs only `/bin/sh` and `/sbin/pfctl`: a root job
  must never run anything this user or Homebrew could rewrite.
- The install check and `grove doctor` compare the installed plist with what grove renders,
  never with the staged copy.
- Every pass comes before the block. `hostfirewall_test.go` loads the anchor on the macOS CI
  runner's real pf (`GROVE_PF_TEST=1`).

## Self-update — nobody upgrades grove by hand

`internal/install/selfupdate.go` and `internal/cli/self_update.go`: `grove serve` and the broker
install newer releases by themselves (`runAutoUpdate`) and restart. Keep it that way: nothing is
installed unless the archive matches the release's `checksums.txt` and the new binary reports the
expected version; never while a job is pending or running (a restart drops in-memory credential
entries); never through sudo or into a path this user doesn't own (Homebrew's is left to brew);
never a development build. Never tell the operator to upgrade a machine by hand when this covers it.

## Image holds — a pool never goes down for a bad image

`internal/fleet/imagehold.go`: `grove serve` keeps a pool on the last image a VM of it ran while
the image it names can't be pulled (`ImageHolds`, `images.json`), retries the named image on a
growing interval and switches back by itself. Keep it that way: a failure to get an image must
never need the operator to edit fleet.yaml to get the pool's jobs running again, only `Observe`
writes the store (Plan reads it), and it holds image names and Orchard messages, never a
credential. `internal/fleet/imagehold_test.go` asserts it.

## `internal/snooze` — why a worker is paused

Orchard has one pause bit per worker, so `pauses.json` (next to `devices.json`, mode 0600,
`snooze.Store`) records every reason: snooze, health, operator (`external` for a pause grove
found with no record). The bit is set iff a reason is present. Keep it that way:

- A snooze expiring or `wake` clears only the snooze reason and resumes the worker only when no
  other reason remains (`snooze.Wake`, `Expirer.Tick`); never call `ResumeWorker` otherwise.
- Evicted attempts (`dispatch.AttemptEvicted`) never count as losses: not toward quarantine,
  `MaxAttempts`, or `internal/health`.
- The queue is per pool. A snooze never pins or holds a job to a worker; `dispatch.PoolWaiter`
  only stops a job being failed when no other worker could run it and pushes its pending timeout
  past the snooze. Never add a host constraint to a redispatch.
- `worker:snooze:<name>` is granted to that worker's holder only for `/workers/<name>/snooze|wake`.
- Every writer of a worker's pause state (Engage, Wake, the Expirer, `worker pause|resume`, the
  health monitor) holds `snooze.Store.Lock(worker)` from reading Orchard to its last write, and
  Wake and the Expirer resume *before* clearing the snooze, so a failed resume leaves a lapsed
  snooze to retry, never a pause with no reason (spec S1, L1, L2).
- `PoolHealth.WaitReason` holds a job for a snooze that lapsed but isn't cleared, or that ended
  (`EndedAt`) after the fleet observation was listed (`Plan.ListedAt`): never fail a job as "every
  worker is paused" in that window (spec L4).

## Job environments — keep it that way

A shared-pool job's image is its pool's runner image or a digest-pinned image its pool allows,
chosen by the control plane; the clone token runs only in the runner image or the root source task
(spec S11). A native toolchain is likewise chosen by the control plane (spec S22).

- `dispatch.Service.Submit` (`taskImage`) is the only place a build's `task_image` meta is set:
  the environment image after `ValidateEnvironmentImage` (digest, no tag, explicit registry, in the
  pool's `environments.allow`), else the pool's runner image. Never copy a caller's `Meta`, `Env`
  or any other field into it, and never forward `Meta["image"]` (the old passthrough was a bug).
- build.nomad.hcl's `source` task stays on `{{.RunnerImage}}`: it holds the clone token. Only `main`
  uses `${NOMAD_META_task_image}`, and a missing value must fail the task, not default to an image.
- A job with an environment falls back only to a macOS pool with toolchains enabled, and only when
  it carries a toolchain (`effectivePin`, `fallbackTarget`); Submit is the only place
  `toolchain_json` is set, from the validated, re-marshalled spec (`ValidateToolchain`,
  `canonical`), never from a caller's bytes or `Meta`; the installer runs as `_grovetool` from
  root-owned code and `_grovejob` can't write the cache. Sealed pools refuse environments, and
  `fleet.Load` refuses `environments` on macOS and `toolchains` anywhere but a shared macOS pool.
- `internal/dispatch/environment_test.go`, `internal/dispatch/toolchain_test.go` and
  `nomad/jobs/nomadjobs_test.go` assert it;
  `TestTaskImageInterpolationRealNomad` (real Nomad + docker, `GROVE_NOMAD_BIN`) proves the
  interpolation.

## Native toolchains on macOS — keep it that way

A job's environment may name a rust/node/python toolchain; a shared macOS VM installs it with its
pinned mise (`internal/fleet/toolchain.go`, `nomad/jobs/build.nomad.hcl`).

- The install runs only in the `source` task, as `_grovetool` (never root, never the job user), from
  `/`, after the clone token's file and variables are gone, through the root-owned
  `/usr/local/libexec/grove-toolchain`. The installer re-validates the spec and recomputes its hash;
  the only `mise.toml` mise reads is the one it writes. Nothing a job controls reaches mise's config.
- The result is read-only to the job: `run.sh` prepends a toolchain path to `PATH` only if it lies
  inside the entry's directory. `_grovetool` stays in the pf fence and the SSH block.
- `internal/fleet/toolchain_test.go` and `nomad/jobs/toolchain_test.go` assert it.

## Local socket (`server.socket`)

`internal/server/localsock.go` lets a client on the control-plane Mac (Argos) reach the API
without the network. The gate is the peer uid the kernel reports (`peerUID`, per OS), never the
socket's file mode, and `withAuth` still demands a bearer token on it, never Tailscale identity.
Keep both: the token is what stops a later move of the client off this Mac from running with no
auth.

## Building / testing

```
go build ./...
go vet ./...
go test ./...
make build        # binary to bin/grove, version via -ldflags
make test         # go vet + go test
make ui           # cd ui && npm ci && npm run build (only needed once ui/ has content)
spec/check.sh     # every model check, proof and query; needs quint, TLA2TOOLS_JAR, java, lake, go (CODEQL optional)
```

A change to snooze, dispatch reconcile, attempts, fleet TTL/health, Enrol/RollCall, route scopes,
artifact paths or the broker's header swap must keep `spec/check.sh` passing; update the model
when the design changes, and keep each "before" instance failing.
`TestSnoozeConformsToModel` replays sampled `snoozeMBT` traces against the real snooze, server and
health code, and `spec/mutants.py` requires each listed old bug to be caught: when you change that
code, change the model with it, and when you fix a bug a model found, add its mutant.

CI (`.github/workflows/ci.yml`) runs `go build/vet/test` on `ubuntu-latest` and `macos-latest`,
plus the `ui` build when `ui/package.json` exists. Releases (`.github/workflows/release.yml`) run
via goreleaser (`.goreleaser.yaml`) and need no hands: when CI is green on main and the commit
changes what a release ships, `.github/workflows/autorelease.yml` tags the next patch version
(`.github/scripts/next-release-tag.sh`) and calls release.yml for it. Never ask the operator to
push a tag for a patch release; a hand-pushed `v*` tag is only for a new minor or major version.
goreleaser also regenerates `Formula/grove.rb` in
this same repo — grove is its own Homebrew tap, no separate `gm2211/homebrew-tap` repo or PAT.

## If asked to install grove on this machine

Decision tree:

1. **Is this a fresh machine with no `grove` binary?** Run `scripts/install.sh`'s one-liner (or,
   on macOS, `brew tap gm2211/grove https://github.com/gm2211/grove && brew install
   gm2211/grove/grove`) first. Already installed? `brew upgrade grove`.
2. **What role?**
   - Runs jobs (a Mac, ideally always-on, lid open or docked) → `worker`.
   - The one always-on box (Linux, or a dedicated Mac) that should host the control plane →
     `control-plane`.
   - Just wants to submit/watch jobs from here → `client`.
3. **Preview first**: `grove install --role <role> [flags] --dry-run` and read the plan before
   applying it.
4. **Apply**: drop `--dry-run`. Run it as the ordinary account the worker should run as (never
   root; `su - <user>` works, no GUI session needed), or, from an admin account, `grove setup --user
   <user>` (one sudo prompt; the admin's grove renders the root script itself and never runs
   anything the worker user's process produced — `internal/cli/setup_user.go`). The worker role's root steps (`pmset`, the `worker-daemons`
   install of the Orchard worker and broker LaunchDaemons, the `host-firewall` step) run as one
   script through one `sudo` when the user is an admin on a terminal and says yes; otherwise grove
   prints one block for an admin to paste, and you re-run the same command.
5. **Verify**: `grove doctor`.
6. **Remove** (later): `grove uninstall --role worker [--user <user>] --dry-run`, then without
   `--dry-run` (docs/OPERATIONS.md "Removing a worker Mac").

Full walkthrough, flags, and failure modes (Local Network permission popup, FileVault blocking
auto-login, Mac App Store Tailscale, the 2-macOS-VM cap): **[docs/INSTALL.md](docs/INSTALL.md)**.
Day-2 operations (recycling, image rollouts, pausing a Mac, logs, upgrading):
**[docs/OPERATIONS.md](docs/OPERATIONS.md)**.
