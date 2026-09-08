# grove — flow diagrams

Three views of the same system, each answering a different question. See
[ARCHITECTURE.md](../ARCHITECTURE.md) for the prose version of the layering and the fleet/job
contracts these diagrams summarize.

## 1. Layering — who talks to what

Intent flows down through one control plane into two independent systems (VM lifecycle, job
scheduling), which meet only inside each Mac.

```mermaid
flowchart TB
    subgraph intent["intent"]
        orch["agent orchestrator"]
        mcp["Claude Code / Codex\n(MCP over stdio)"]
        cli["grove CLI\n(grove dispatch, grove fleet, ...)"]
    end

    subgraph cp["control plane — grove server"]
        api["HTTP API /api/v1/...\n+ embedded web UI"]
        reconciler["fleet reconciler\n(desired VMs -> Orchard)"]
        dispatcher["job dispatch\n(JobRequest -> Nomad)"]
    end

    orchard["Orchard controller\n(VM lifecycle: create, place, TTL, delete)"]
    nomad["Nomad server\n(job scheduler: placement, packing, queueing)"]

    subgraph mac["each Mac"]
        worker["Orchard worker"]
        subgraph tart["Tart VMs"]
            linuxvm["Linux VM\nnomad client + docker\n-> jobs as containers"]
            macosvm["macOS VM\nnomad client + xcode\n-> jobs as processes"]
        end
    end

    orch -- HTTP --> api
    mcp -- MCP/stdio --> api
    cli -- HTTP --> api

    api --> reconciler
    api --> dispatcher
    reconciler -- Orchard REST --> orchard
    dispatcher -- Nomad HTTP --> nomad

    orchard -- "workers dial out" --> worker
    worker --> tart
    nomad -- "clients dial out" --> linuxvm
    nomad -- "clients dial out" --> macosvm

    style intent fill:transparent,stroke:#8a9182
    style cp fill:transparent,stroke:#2f6f4f
    style mac fill:transparent,stroke:#8a9182
    style tart fill:transparent,stroke:#8a9182
```

Three separate responsibilities, three separate systems (see ARCHITECTURE.md's "The layering"
table for what each layer owns and never knows about): **Orchard** owns VM lifecycle, **Nomad**
owns job scheduling inside VMs, **grove** is the only layer that sees both.

## 2. One job, start to finish

`grove dispatch` (or the MCP `grove_run` tool, or an orchestrator's executor) to a finished job
with logs
and an exit code:

```mermaid
sequenceDiagram
    participant U as grove dispatch
    participant API as grove server\n(/api/v1/jobs)
    participant N as Nomad server
    participant VM as VM's Nomad client
    participant R as run.sh\n(clone, script, artifacts)

    U->>API: POST /api/v1/jobs\n{kind, pool, repo, ref, script, timeout}
    API->>API: map JobRequest -> meta + payload
    API->>N: nomad job dispatch grove-<kind>-<pool>
    N-->>API: dispatch accepted -> job id
    API-->>U: 201 {"id": "j-8f2"}

    N->>VM: place allocation (constraint: meta.pool)
    VM->>R: exec run.sh (script.sh written to NOMAD_TASK_DIR)
    R->>R: expand env_json -> export
    R->>R: git clone --depth 50 repo@ref (build/agent only)
    R->>R: run_with_timeout $T bash -eo pipefail script.sh
    opt kind == build
        R->>R: upload ./artifacts/** via mc to MinIO
    end
    R-->>VM: exit code

    U->>API: GET /api/v1/jobs/j-8f2/logs?follow=1&sinceOffset=N
    API->>N: stream allocation logs
    N-->>API: log frames
    API-->>U: NDJSON {offset, ts, stream, line}

    VM-->>N: allocation terminal (exit code)
    N-->>API: status change (poll / event)
    U->>API: GET /api/v1/jobs/j-8f2
    API-->>U: {"status": "success", "exitCode": 0, ...}
```

Kinds differ only in what `run.sh` does after the clone (see docs/JOBS.md): `build` uploads
artifacts, `agent` stays long-running with a 10m `kill_timeout`, `shell` skips the clone entirely.

## 3. Recycling loop — nothing is cleaned, only replaced

```mermaid
flowchart LR
    A["VM's Orchard TTL\nexpires (e.g. 12h)"] --> B["Orchard deletes the VM\nvia the normal path"]
    B --> C["ShutdownScript runs\ninside the guest first"]
    C --> D["nomad node drain -self\n-enable -deadline 2h"]
    D --> E["guest waits for\nzero allocations"]
    E --> F["Orchard proceeds\nwith delete"]
    F --> G["grove fleet reconciler\nnotices VM is gone"]
    G --> H["recreates VM from\npool's image"]
    H -.->|"new TTL clock starts"| A

    style A fill:transparent,stroke:#8a9182
    style H fill:transparent,stroke:#2f6f4f
```

The guest drains itself — Orchard never learns Nomad exists, and Nomad never learns about VM
lifecycle (ARCHITECTURE.md's "Recycling / hygiene"). Tailscale inside guests uses ephemeral tagged
auth keys, so a recycled VM also vanishes and reappears on the tailnet under a fresh identity.

Same mechanism drives a forced recycle (`POST /api/v1/vms/{name}/recycle`, or the MCP
`grove_recycle_vm` tool) — it's the TTL-expiry path run on demand instead of waiting for the
clock, then the reconciler takes over exactly as above.
