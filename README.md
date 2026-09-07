# grove

Turn the Macs you already own into a private build-and-agent cloud.

- **Tart** VMs on every Apple Silicon Mac, managed by **Orchard** (our fork adds VM TTLs and shutdown hooks)
- **Nomad** packs jobs — CI builds, deploys, Claude Code / Codex sessions — into those VMs
- one `grove` CLI, one control-plane UI, an MCP server, and an HTTP API for orchestrators like Argos
- reachable only over your **Tailscale** tailnet; nothing dials in

Stop paying for GitHub Actions macOS minutes; keep the isolation.

> Status: under construction. See [ARCHITECTURE.md](ARCHITECTURE.md) for the design and
> [docs/INSTALL.md](docs/INSTALL.md) for setup (written so you can hand it to Claude Code on each machine).

## Quick start

```bash
# on each Mac you want in the fleet (installs via Homebrew — this repo is its own tap)
curl -fsSL https://raw.githubusercontent.com/gm2211/grove/main/scripts/install.sh | sh
grove install --role worker --controller https://grove-cp.<tailnet>.ts.net:6120

# on the always-on box (Linux or a Mac)
grove install --role control-plane

# from anywhere on the tailnet
grove fleet apply
grove dispatch --pool linux --repo git@github.com:you/app.git --ref main -- make test
```

## License

MIT
