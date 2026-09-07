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
internal/server/              HTTP API (/api/v1/…) + embedded UI; token auth
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
- Templates (launchd plists, systemd units, Nomad/MinIO/Orchard config) live in `render.go` as
  `text/template`s over typed spec structs — keep them deterministic (no map iteration order
  surprises; Go's `text/template` sorts map keys, so that's safe) so golden tests stay meaningful.

Adding a step: add it to the relevant `build<Role>Steps` function in `worker.go` /
`controlplane.go` / `client.go`, keep `Check`/`Apply` going through `Runner`, and add a
planner test in the matching `_test.go` asserting the FakeRunner sees (or doesn't see) the
commands you expect.

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
on `v*` tags via goreleaser (`.goreleaser.yaml`), which also updates the Homebrew tap
(gm2211/homebrew-tap).

## If asked to install grove on this machine

Decision tree:

1. **Is this a fresh machine with no `grove` binary?** Run `scripts/install.sh`'s one-liner (or
   `brew install gm2211/tap/grove` on macOS) first.
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
