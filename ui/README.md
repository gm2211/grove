# grove ui

Vite + React 19 + TypeScript control-plane web UI for grove. Built output is embedded directly
into the `grove` server binary (see `internal/server/ui.go`); this directory is otherwise a
standalone SPA.

## Develop against a real grove server

```sh
npm ci
npm run dev
```

Opens on http://localhost:5173. Configure the server URL + bearer token from the **Settings**
page (defaults to `window.location.origin`, so if you're proxying to a real server set it there).

## Develop standalone with fake data (no server required)

```sh
npm run dev:mock
```

Sets `VITE_MOCK=1`, which swaps every API call (`src/api/client.ts`) for the in-memory fixtures in
`src/mock/data.ts`: a handful of hosts with Orchard workers, VMs and matching Nomad nodes, ~25 jobs
in various states, and a simulated job lifecycle (dispatch → pending → running → success/failed)
plus a fake streaming log. A `MOCK` badge appears in the header so it's never confused with a real
connection.

## Typecheck

```sh
npm run typecheck   # tsc -b --noEmit
```

## Build

```sh
npm run build
```

Runs `tsc -b` then `vite build`. `vite.config.ts` sets `build.outDir` to
`../internal/server/dist` (relative to this directory) because Go's `//go:embed` can only reach
files inside the embedding package's own directory tree — it cannot walk up through `ui/dist`.
`internal/server/ui.go` embeds that directory with `//go:embed all:dist` and serves it as an SPA
(client-side routes fall back to `index.html`; hashed files under `/assets/` get a long
`Cache-Control: immutable`).

From the repo root, `make ui` does the same (`cd ui && npm ci && npm run build`).

On a fresh clone, `internal/server/dist/` contains only a placeholder `.keep` file (the actual
build output is gitignored) — `go build ./...` still succeeds, and the server serves a small "UI
not built, run `make ui`" page until you build the UI for real.

## Pages

- **Fleet** — hosts grouped card-by-card: Orchard worker (online/cordoned, slots used/total),
  its VMs (pool, status, TTL remaining, running jobs, recycle), matching Nomad nodes. Polls every
  5s.
- **Jobs** — table of all jobs, filterable by status/pool. Polls every 3s.
- **Job detail** — request, status timeline, live log via `EventSource` (auto-scroll with pause),
  cancel, artifacts.
- **Dispatch** — form to submit a `JobRequest`; navigates to the new job's detail page.
- **Settings** — server URL + bearer token, stored in `localStorage`.

## Stack

React 19, react-router, `@tanstack/react-query` for polling/mutations, Tailwind v4
(`@tailwindcss/vite`, no separate config file needed — see `src/index.css`). No UI kit; components
are small and hand-written (`src/components/`). Dark/light follows `prefers-color-scheme`.
