# grove — repo conventions

grove turns Macs (and a Linux box) you already own into a private CI/agent cloud. See
[ARCHITECTURE.md](ARCHITECTURE.md) for the design; this file is about how the repo itself is
organized and how to work in it.

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
internal/broker/              per-worker credential broker: placeholders in, real tokens out (see below)
internal/health/              rebuilds VMs / pauses Macs that keep losing sealed-pool jobs
internal/mcp/                 MCP server (stdio)
internal/install/             role bootstrap planner + doctor checks (see below)
ui/                            Vite + React + TS SPA
images/                         Packer templates
nomad/jobs/                     parameterized Nomad job HCL
scripts/install.sh              curl | sh → brew tap + grove install
docs/                            INSTALL.md, OPERATIONS.md
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
  ever `Apply` for real when running as root with `--yes` — otherwise they're printed as a manual
  checklist.
- `RunPlan` walks the list, never stopping early on a failed step, and returns the first error
  alongside the full per-step results.
- Templates (launchd plists, systemd units, Nomad/Orchard config) live in `render.go` as
  `text/template`s over typed spec structs — keep them deterministic (no map iteration order
  surprises; Go's `text/template` sorts map keys, so that's safe) so golden tests stay meaningful.

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
- Only `https://` repo URLs match: a token does nothing for an SSH remote, so handing one over
  there would be a leak with no upside.

## Nomad mTLS and client introduction

Only the control plane may tell a node what to do, so only nodes grove created may join Nomad.
The control-plane install creates grove's CA (`internal/pki`, under `~/.config/grove/nomad/tls`)
and runs Nomad with mutual TLS on RPC and HTTP and `client_introduction { enforcement = "strict" }`.
A shared VM's spec carries only the CA. Once it runs, `Reconciler.Enrol` (`internal/fleet/enrol.go`,
called every `grove serve` tick) pushes in, over Orchard exec with the secrets on stdin, its own
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
fails closed if `admin`/`admin` still works. A worker that doesn't advertise
`guest-password-rotation` (the gm2211/orchard fallback to the image password) gets no macOS VM:
the pool is reported blocked there and any existing VM is deleted as unsafe. Keep it that way:
never put the password on a command line, in the API or in a planned spec. Details:
docs/OPERATIONS.md "The macOS guest's admin password".

## `internal/secrets` — named job secrets

Same discipline as the private-repo token: a value is set only by an operator (`grove secret set`,
value from stdin or an env var, never argv), lives only in `secrets.json` (0600, atomic rename),
and reaches a job only through that dispatch's `env_json`. Never on `Job.Request` (which keeps
the names), never in the job history, never in an API response (`Secret` carries a fingerprint).
Authorization (who may name which secret) is checked in `internal/server` where the principal is
known; `internal/dispatch` only resolves. `internal/dispatch/secrets_test.go`,
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
- In `internal/dispatch`, a `JobRequest.Credentials` token goes only into the registry: never
  `env_json`, the payload, the stored `Job.Request` (kind and env only) or an API response.
  `internal/dispatch/credentials_test.go` asserts it.

`internal/broker/proxy_test.go` drives the whole path end to end; keep its refusal cases passing.

## Building / testing

```
go build ./...
go vet ./...
go test ./...
make build        # binary to bin/grove, version via -ldflags
make test         # go vet + go test
make ui           # cd ui && npm ci && npm run build (only needed once ui/ has content)
```

CI (`.github/workflows/ci.yml`) runs `go build/vet/test` on `ubuntu-latest` and `macos-latest`,
plus the `ui` build when `ui/package.json` exists. Releases (`.github/workflows/release.yml`) run
on `v*` tags via goreleaser (`.goreleaser.yaml`), which also regenerates `Formula/grove.rb` in
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
4. **Apply**: drop `--dry-run`. Do the printed privileged checklist (worker role only — sudo
   pmset, local-network `defaults write`, `launchctl bootstrap`) yourself; grove never runs those
   for you.
5. **Verify**: `grove doctor`.

Full walkthrough, flags, and failure modes (Local Network permission popup, FileVault blocking
auto-login, Mac App Store Tailscale, the 2-macOS-VM cap): **[docs/INSTALL.md](docs/INSTALL.md)**.
Day-2 operations (recycling, image rollouts, pausing a Mac, logs, upgrading):
**[docs/OPERATIONS.md](docs/OPERATIONS.md)**.
