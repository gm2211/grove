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

- **Map** (`/map`, the default; `/fleet` redirects here) — the live map, after Argos's: columns
  in the order a job travels: the queue of pending jobs → worker Macs → their Tart VMs (one Nomad
  node each, same name) → the jobs on the selected VM. Selecting a queued job lights the Macs and
  VMs of its pool. A detail column for the selection
  (lifecycle rail, live log tail, pause/snooze/wake/recycle/cancel). The selection lives in the URL
  (`?w=&v=&j=`). A Table toggle lists every VM flat. Macs waiting to join show above the columns.
  Polls fleet every 5s, jobs every 3s.
- **Jobs** — every job, newest first, filterable by status and pool. Polls every 3s.
- **Job detail** — lifecycle and attempts, request, script, env, artifacts, live log (fetch +
  NDJSON, auto-scroll with pause), cancel.
- **Dispatch** — form to submit a `JobRequest`; navigates to the new job's detail page.
- **Settings** — server URL + bearer token, stored in `localStorage`; private-repo sourcing for
  operators.

## Look

Dark only, borrowed from Argos's "Nocturne" map: near-black canvas, teal accent
(`--accent: #7ee7c7`), JetBrains Mono for labels and data, a Newsreader serif headline per view,
Inter for prose. Tokens and the shared primitives (`.mapnode`, `.act`, `.chip`, `.kicker`,
`.facts`, `.stages`, `.logpane`) are in `src/index.css`, inside `@layer components` so Tailwind
utilities still override them. Fonts are bundled via `@fontsource`, never fetched from a CDN: the UI
is served from a private tailnet.

`npm run screenshots` regenerates `docs/screenshots/` (stills at 2x and `walkthrough.gif`, which
needs ffmpeg) from the deterministic mock fleet.

## Stack

React 19, react-router, `@tanstack/react-query` for polling/mutations, Tailwind v4
(`@tailwindcss/vite`, no separate config file needed — see `src/index.css`). No UI kit; components
are small and hand-written (`src/components/`). 
