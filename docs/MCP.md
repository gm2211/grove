# grove MCP server

`grove mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io) server on stdio,
exposing the same fleet/job operations as the grove HTTP API (see
[ARCHITECTURE.md](../ARCHITECTURE.md) § "grove HTTP API (v1)") as MCP tools, resources and a
prompt. Point any MCP-capable client at it — Claude Code, Codex, Claude Desktop — and it can
inspect the fleet, submit jobs, and manage VMs/workers directly.

It reads `server.url` and `server.token` from the grove config (`~/.config/grove/config.yaml`,
or `$GROVE_CONFIG`) and opens **no network listener of its own** — it only speaks stdio to its
MCP client and HTTPS/HTTP to the grove server you've configured.

## What it exposes

**Tools**

| Tool | Purpose |
|---|---|
| `grove_fleet` | Normalised fleet summary: hosts, workers online/cordoned, VMs by pool/status, nodes ready, running jobs. |
| `grove_run` | Submit a job (`shell`, `build`, or `agent`) to a pool. Waits for completion by default and returns exit code, node, duration and a log tail; pass `wait: false` to fire-and-forget (useful for long-lived `agent` sessions). |
| `grove_job_status` | Get a job's current status/exit code/node/timestamps. |
| `grove_job_logs` | Fetch the trailing log lines for a job. |
| `grove_job_cancel` | Cancel a running or pending job. |
| `grove_recycle_vm` | Drain + delete a VM by name; the fleet reconciler recreates it from its image. |
| `grove_pause_worker` | Cordon/resume scheduling on a worker Mac. |

**Resources**

- `grove://fleet` — the same fleet summary as `grove_fleet`, as a JSON resource.
- `grove://jobs/recent` — the most recent 50 jobs, newest first, as JSON.

**Prompts**

- `grove_ci` — "run this repo's tests on pool X and report". Arguments: `pool` (required),
  `repo`, `ref`, `script` (all optional, default to the current repo/branch/test target).

## Register with Claude Code

```bash
claude mcp add grove -- grove mcp
```

This adds an entry to Claude Code's MCP config that runs `grove mcp` on demand; make sure
`grove` is on `PATH` for whatever shell Claude Code launches (or use an absolute path to the
binary in place of `grove` above).

## Register with Codex

Add to `~/.codex/config.toml` (or your project's `.codex/config.toml`):

```toml
[mcp_servers.grove]
command = "grove"
args = ["mcp"]
# Optional: point at a non-default config file.
# env = { GROVE_CONFIG = "/Users/you/.config/grove/config.yaml" }
```

## Register with Claude Desktop

Add to Claude Desktop's `claude_desktop_config.json` (macOS:
`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "grove": {
      "command": "grove",
      "args": ["mcp"]
    }
  }
}
```

Restart Claude Desktop after editing the config.

## Example tool calls

Check what's available before submitting work:

```json
{"name": "grove_fleet", "arguments": {}}
```

Run a test suite on the `linux` pool and wait for the result:

```json
{
  "name": "grove_run",
  "arguments": {
    "pool": "linux",
    "repo": "git@github.com:you/app.git",
    "ref": "main",
    "script": "make test",
    "timeout_seconds": 600
  }
}
```

Kick off a long-lived agent session without waiting, then check on it later:

```json
{"name": "grove_run", "arguments": {"pool": "macos", "kind": "agent", "script": "codex --exec 'fix the flaky test'", "wait": false}}
```

```json
{"name": "grove_job_status", "arguments": {"job_id": "<id from the previous call>"}}
```

Recycle a wedged VM:

```json
{"name": "grove_recycle_vm", "arguments": {"name": "linux-mac-1-0"}}
```

## Security

- The server never opens a listening socket; it only makes outbound HTTP calls to
  `server.url`, which should be a tailnet-only address (e.g.
  `http://grove-cp.<tailnet>.ts.net:6120`) — nothing routable from the public internet.
- `server.token` (in `~/.config/grove/config.yaml`, mode `0600`) is sent as
  `Authorization: Bearer <token>` on every request. Treat that config file like a credential:
  don't commit it, don't put it in a shared dotfiles repo without encryption.
- `grove_recycle_vm` and `grove_pause_worker` affect real machines immediately — there's no
  confirmation step in the protocol layer, so whatever policy you want (e.g. "ask before
  recycling") has to live in the calling client/agent's own judgment.
- Every HTTP call the server makes is time-bounded; a wedged or unreachable grove server
  produces a clear tool error (e.g. "grove server unreachable at `<url>` — is `grove serve`
  running and are you on the tailnet?") rather than hanging the calling client indefinitely.

## Implementation note

The HTTP client this package uses (`internal/mcp/apiclient.go`) is a minimal, unexported
client hand-rolled directly against the "grove HTTP API (v1)" contract in ARCHITECTURE.md. A
shared `internal/apiclient` package is expected to land separately; once it does, this package
should switch over to it instead of maintaining its own client.
