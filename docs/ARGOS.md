# grove — the Argos contract

This is the contract Argos's `grove` executor (a job-executor backend registered with Argos)
uses to drive grove: which endpoints it calls and why, how a WorkUnit maps onto a `JobRequest`,
how grove's `Job.status` maps back onto Argos's status vocabulary, the idempotency-key
convention that makes Argos's retries safe, and how to resume a dropped NDJSON log stream.
Read this if you're implementing or debugging Argos's `groveClient`. For the underlying
job-template/dispatch mechanics (how `kind`+`pool` map onto Nomad jobs, what runs inside the
allocation), see `docs/JOBS.md` — this doc only covers the Argos-facing surface.

## Endpoints the executor calls

Full endpoint list and response shapes: ARCHITECTURE.md → "grove HTTP API (v1)". From Argos's
side, the `grove` executor only ever touches five of them:

| Argos executor action | Endpoint |
|---|---|
| Submit a WorkUnit | `POST /jobs` |
| Poll status (no active log stream, or after a disconnect) | `GET /jobs/{id}` |
| Stream / resume logs | `GET /jobs/{id}/logs?follow=1&sinceOffset=N` (NDJSON) |
| Fetch a completed build's output | `GET /jobs/{id}/artifacts/{path}` (URL comes from `Job.artifacts[]`, not constructed by Argos) |
| Cancel a running WorkUnit | `DELETE /jobs/{id}` (or `POST /jobs/{id}/cancel` — same operation) |

`GET /fleet`, `/vms/{name}/recycle`, `/workers/{name}/pause`/`resume` and `GET /healthz` are
operator/UI surface, not part of the Argos contract — the executor never calls them.

## WorkUnit → JobRequest

| Argos WorkUnit field | grove JobRequest field | Notes |
|---|---|---|
| `kind: repo-verify \| repo-lint \| repo-deploy` | `kind: "build"` | deploy is a build with a deploy script — no separate grove kind |
| `kind: agent-session` | `kind: "agent"` | |
| `source.remoteUrl` | `repo` | |
| `source.commit` | `ref` | |
| `script.command` + `script.args` | `script` | shell-quoted and joined into one string (grove's `script` is a single bash command run via `bash -eo pipefail`) |
| `secretRefs` | `secrets` | names only — grove resolves these server-side from its secret store into env, never sent as values |
| `constraints` | `pool` | Argos's `groveClient` picks the first matching pool label — grove itself doesn't interpret `constraints` |
| `taskId` + `runId` | `idempotencyKey` | convention: `argos:<taskId>:<runId>` — see below |
| `beadId`, `repo`, `taskId` (whatever Argos wants round-tripped) | `meta` | opaque, round-tripped verbatim onto `Job.meta` — not consumed by grove's job templates, just carried |

Example submitted `JobRequest` for a `repo-verify` WorkUnit:

```json
{
  "kind": "build",
  "pool": "linux",
  "repo": "https://github.com/gm2211/grove",
  "ref": "a1b2c3d",
  "script": "make verify",
  "secrets": ["GH_TOKEN"],
  "idempotencyKey": "argos:task-1234:run-2",
  "meta": {
    "argosTaskId": "task-1234",
    "argosBeadId": "bd-9981",
    "argosRepo": "gm2211/grove"
  },
  "requester": "argos"
}
```

`POST /jobs` returns `{"id": "<job id>"}` — `201 Created` when a new job was dispatched, `200 OK`
when `idempotencyKey` matched an existing job (nothing was re-dispatched).

## Status mapping

| grove `Job.status` | Argos status |
|---|---|
| `pending` | `pending` |
| `running` | `running` |
| `success` | `succeeded` |
| `failed` | `failed` |
| `canceled` | `cancelled` |
| `lost` | `lost` |

`lost` deliberately does **not** collapse into `failed`: it means the allocation itself
disappeared (a node died, a driver/setup error, Nomad lost track of it) rather than the script
running and exiting non-zero. When `status` is `lost`, `Job.failureReason` is set specifically so
Argos's executor can classify the WorkUnit as transient/retryable instead of a genuine script
failure. Related `Job` fields useful to the executor:

- `timedOut` — the script hit its own `timeout` wrapper inside the allocation (run.sh, not an
  Argos-side timeout); a `timedOut: true` job is `status: "failed"`, not `lost`.
- `signal` — the Unix signal (by name, e.g. `"SIGTERM"`) that ended the task, when known. Set on
  a `canceled` job (grove cancels via `nomad job stop`, which signals the task) and occasionally
  on `lost`/`failed` ones.
- `placement` — `{allocId, nodeId, vmId, workerId}`, which alloc/node/VM/worker the job ran on.
  Not needed to interpret status, but useful for debugging a flaky worker (correlate several
  `lost` jobs by `workerId`).

## Idempotency key convention

Use `argos:<taskId>:<runId>` as `JobRequest.idempotencyKey` for every submit. Argos's own retry
logic may re-submit a WorkUnit after a network blip without knowing whether the first `POST
/jobs` actually landed on the server. Submitting the same key again doesn't dispatch a second
Nomad job — grove returns the already-dispatched `Job` (`200 OK` instead of `201 Created`), so a
retried submit is safe by construction; the executor never needs its own dedupe table.

## NDJSON log resumption

`GET /jobs/{id}/logs?follow=1&sinceOffset=N` with `Accept: application/x-ndjson` streams one JSON
object per line:

```json
{"offset": 0, "ts": "2026-09-06T10:15:03.1Z", "stream": "stdout", "line": "cloning repo..."}
{"offset": 20, "ts": "2026-09-06T10:15:03.4Z", "stream": "stdout", "line": "running make verify"}
```

`offset` is a running byte counter over the *emitted* NDJSON stream itself, not any offset Nomad
tracks internally — grove replays the job's full log from Nomad's own retained start on every
request and recomputes offsets from 0 each time, only emitting (or, with `sinceOffset` set,
skipping) lines below the threshold. Practically this means: resuming after a dropped connection
is "reconnect with `sinceOffset` set to the last offset you successfully processed," not
client-side deduping of a continuous stream — the server does the skipping, the client just
remembers where it got to.

Argos-side reconnect loop, in prose:

1. Open `GET /jobs/{id}/logs?follow=1&sinceOffset=0`.
2. Read NDJSON objects one line at a time; for each, append `line` to the task's log and record
   `offset` as `lastOffset`.
3. On disconnect (network error, timeout, EOF before the job reached a terminal status): re-open
   with `sinceOffset=lastOffset+1` (or just `lastOffset` — the server's `<` comparison makes
   re-sending the last-seen offset harmless, at worst re-checking one already-processed line).
4. Stop when the connection closes cleanly *and* a `GET /jobs/{id}` poll shows a terminal status
   (`success`, `failed`, `canceled`, `lost`) — a clean close before that just means grove finished
   replaying what Nomad had buffered so far; treat it as another reconnect point, not the end.

## Artifact download

Once a `build` job reaches a terminal status, `Job.artifacts[]` is populated:

```json
{"path": "coverage.xml", "url": "/api/v1/jobs/j-8f2/artifacts/coverage.xml", "size": 4021, "contentType": "application/xml"}
```

`url` is already a full API path (`/api/v1/jobs/{id}/artifacts/{urlencoded path}`) — Argos `GET`s
it directly with the same bearer token used for everything else. grove proxies the download from
its own MinIO/artifact-store credentials, so the executor never needs separate bucket
credentials or a second auth scheme.

## Curl walkthrough

```console
# submit a build job with an idempotency key
$ curl -sS -X POST https://grove.tailnet.ts.net:6120/api/v1/jobs \
    -H "Authorization: Bearer $GROVE_TOKEN" -H 'Content-Type: application/json' \
    -d '{
      "kind": "build",
      "pool": "linux",
      "repo": "https://github.com/gm2211/grove",
      "ref": "a1b2c3d",
      "script": "make verify",
      "idempotencyKey": "argos:task-1234:run-2",
      "meta": {"argosTaskId": "task-1234", "argosBeadId": "bd-9981"},
      "requester": "argos"
    }'
# -> {"id":"j-8f2"}
```

```console
# poll status
$ curl -sS https://grove.tailnet.ts.net:6120/api/v1/jobs/j-8f2 \
    -H "Authorization: Bearer $GROVE_TOKEN"
# -> {"id":"j-8f2","status":"running",...}
```

```console
# stream logs from the start, NDJSON, following while the job runs
$ curl -sS -N "https://grove.tailnet.ts.net:6120/api/v1/jobs/j-8f2/logs?follow=1&sinceOffset=0" \
    -H "Authorization: Bearer $GROVE_TOKEN" -H 'Accept: application/x-ndjson'
```

```console
# connection dropped after offset 340 — reconnect and resume from there
$ curl -sS -N "https://grove.tailnet.ts.net:6120/api/v1/jobs/j-8f2/logs?follow=1&sinceOffset=340" \
    -H "Authorization: Bearer $GROVE_TOKEN" -H 'Accept: application/x-ndjson'
```

```console
# job finished (status: success) — download the artifact its Job.artifacts[] listed
$ curl -sS -o coverage.xml "https://grove.tailnet.ts.net:6120/api/v1/jobs/j-8f2/artifacts/coverage.xml" \
    -H "Authorization: Bearer $GROVE_TOKEN"
```

```console
# cancel a still-running job
$ curl -sS -X DELETE https://grove.tailnet.ts.net:6120/api/v1/jobs/j-8f2 \
    -H "Authorization: Bearer $GROVE_TOKEN"
# -> 204 No Content
```
