# spec/ — what grove promises, and the checks that hold it to them

Twenty properties of grove, ten about progress (liveness, L) and ten about who can do what
(security, S), each written down in English, stated in the tool that fits it, and checked against
the Go code. `spec/check.sh` runs all of it and fails if any result changes, in either direction:
a property that should hold fails, or a "before" model stops finding the bug it was written for.
CI runs it on every push (the `spec` job).

| Tool | Used for | Where |
|---|---|---|
| **Quint**, checked by **TLC** (exhaustive) | control loops that race: snooze, dispatch, attempts, fleet, enrolment | `quint/*.qnt` |
| **Lean 4** | pure decisions over every possible input: route auth, artifact paths, the broker's swap | `lean/Grove/*.lean` |
| **CodeQL** taint tracking | where secrets can flow in the Go code | `codeql/*.ql` |
| **Go tests** | pin each model to the real packages, so code and model can't drift apart | `../internal/conformance`, `../internal/server/spec_conformance_test.go`, `../internal/fleet/convergence_test.go` |

The threat model is the security review's: a tailnet device grove doesn't control (**T**), a
malicious or compromised job (**J**), a compromised worker Mac (**W**), a leaked credential
(**C**). Root on the control plane is the control plane, so it is out of scope.

## Liveness

| ID | Property | Tool | Result |
|---|---|---|---|
| L1 | A worker an operator resumed works again once any snooze on it lapses, unless someone pauses it again. | Quint `snooze.qnt` | **Failed before this change** (a resume racing a snooze left the Mac paused with no reason, forever). Holds now. |
| L2 | Once requests stop, Orchard's pause bit matches the recorded reasons: paused iff a reason exists. Holds even when Orchard refuses one resume. | Quint `snooze.qnt` | **Failed before**, three ways: concurrent pause and resume, a phantom "external" pause, and a failed resume after a snooze (the snooze was cleared first). Holds now. |
| L3 | Every job reaches a terminal state. | Quint `dispatch.qnt` | Holds. |
| L4 | A job queued only because its pool's workers are snoozed is never failed for it, and runs once the snooze ends. | Quint `dispatch.qnt` + Go `TestL4_…` | **Failed before**: in the up to 60 s between a snooze's end and the next fleet tick, the reconcile failed the job as "every worker is paused". Holds now. |
| L5 | A sealed-pool job terminates, given that owners snooze its Macs and VMs drain only finitely often. | Quint `attempts.qnt` | Holds. |
| L6 | From any fleet, one Plan + Apply converges: the next Plan is empty, and stays empty. Every VM left runs on an online, unpaused worker its pool's selector matches. | Go property test, 500 random fleets | Holds. |
| L7 | A VM reaching its TTL is drained before Orchard recycles it (no job lands on a dying VM), and the pool gets a replacement. | Quint `fleet.qnt` | Holds while DrainNode fails on fewer than 4 ticks in a row; the 4-failure instance shows the bound is tight. |
| L8 | A running VM's Nomad certificate is renewed before it expires. | Quint `enrol.qnt` | Holds through any outage under 11 h; the 11 h instance shows the bound is tight. |
| L9 | A genuine new VM eventually joins Nomad, even when a previous VM of its name still has a draining node. | Quint `enrol.qnt` | Holds, given the VM's Nomad client keeps retrying (strong fairness). |
| L10 | A failed or boot-looping VM is rebuilt. | Quint `fleet.qnt` | Holds. |

## Security

| ID | Property | Attacker | Tool | Result |
|---|---|---|---|---|
| S1 | A snoozed Mac does no work: snoozed implies paused in Orchard. | C (a resume racing a snooze) | Quint `snooze.qnt` + Go `TestS1_…` | **Failed before this change**: TLC found it in 8 steps. Holds now. |
| S2 | Attempt accounting: evictions are free, quarantine needs losses on 2 machines, and a job gets at most 3 counted attempts. | J (a job that crashes Macs) | Quint `attempts.qnt` | Holds. |
| S3 | No Nomad identity for a pool reaches a worker outside that pool. | W | Quint `enrol.qnt` + Go `TestEnrol_OnlyVMsTheReconcilerWouldHaveMade` | **Failed before**: a worker could create a VM of another pool on itself, with grove's exact spec, and Enrol pushed it that pool's identity, and with it that pool's jobs and secrets. Holds now, assuming A3. |
| S4 | After every roll call, each eligible Nomad node is a VM in Orchard, alone, in its own pool. | T, W, C (a leaked node identity) | Quint `enrol.qnt` | Holds. |
| S5 | A `broker` credential reaches only `GET /broker/entries`. | C | Lean `Auth.lean` | Proved, for every path. |
| S6 | `worker:snooze:<w>` reaches only `/workers/<w>/snooze` and `/workers/<w>/wake`. | C | Lean `Auth.lean` | Proved, for every path and worker name. |
| S7 | Scope boundaries: `read` can't submit, cancel or re-key a job; plain `dispatch` can't run a shell job; access, secrets, GitHub, VMs, workers and join approval are operator-only. | C, J | Lean `Auth.lean`, a differential test (24,860 (method, path) pairs), and a test of hostile path spellings | Proved for every route, with any parameters. |
| S8 | The broker writes a real token only into `Authorization` or `X-Api-Key`, only for its kind's hosts, only from a live entry, and only to the job's own VM. Other headers leave exactly as the job sent them. | J | Lean `Broker.lean` | Proved, except the last clause: **the VM binding fails**. `dispatch.BrokerEntries` never sets `Entry.Source`, so any VM on the same worker that learns a placeholder can use it until the entry's lease ends. Still open. |
| S9 | An artifact path can't leave the job's artifact directory. | C, J | Lean `ArtifactPath.lean` + differential test | Proved. |
| S10 | Job secrets, the private-repo token, broker credentials and node keys reach only their one allowed destination: never argv, a log, an API response, the job record or a VM spec. | J, C | CodeQL, 4 queries | No flow found. A coverage query finds each secret at its allowed destination, which shows the queries aren't vacuous. |

### What the fixes in this change are

- **Snooze races (S1, L1, L2).** Every writer of a worker's pause state holds that worker's lock
  (`snooze.Store.Lock`), from reading Orchard to its last write: Engage, Wake, the Expirer,
  `worker pause` and `worker resume`, and the health monitor. Wake and the Expirer resume the worker
  *before* they clear the snooze, so a failed resume leaves a lapsed snooze for the next pass,
  not a pause with no reason.
- **Snooze end (L4).** `PoolHealth.WaitReason` also holds a job for a snooze that lapsed but isn't
  cleared yet, and for one that ended (`snooze.Store.EndedAt`) after the fleet observation it has
  was listed (`Plan.ListedAt`).
- **Pool-crossing identity (S3).** `Reconciler.Enrol` enrols a VM only on a worker its pool's
  selector matches, that Orchard lists, and that Plan wouldn't block.

### Assumptions (where a proof stops)

- **A1.** Go's `ServeMux` cleans a path before routing, so `requiredScope` sees the path of the
  route that serves the request. `TestSpec_NoSpellingOfAnOperatorRouteGetsPastAuth` checks it.
- **A2.** Nomad's strict client introduction refuses a client with no TLS, another CA's
  certificate, no token, or a token for another node pool. `internal/install/nomadtls_integration_test.go`
  checks it against real Nomad.
- **A3.** A worker's Orchard credential can't exec into, or port-forward to, VMs on other Macs.
  **Upstream Orchard doesn't give this** (review finding H9: `compute:connect` is fleet-wide). Until
  the Orchard fork scopes worker credentials, a compromised Mac can reach other Macs' VMs directly,
  and S3 is only as strong as that.
- **A4.** Time: grove serve ticks every 30 s, and Orchard's TTL counts from a VM's creation.
- **A5.** Model sizes: snooze uses one worker and six concurrent writers; dispatch uses one job
  and one worker; attempts uses 3 machines, 2 evictions and 1 drain; enrolment uses 2 workers and
  2 pools. TLC explores every interleaving within those bounds.

### Not covered here (still open from the security review)

- **C2**: secrets still travel in `env_json`. S10 treats that as their allowed destination, so S10
  proves nothing about it.
- **H4**: sender-bound secrets.
- **H9**: per-worker Orchard scope (see A3).
- **M5, M7, M10, M13, M14, L12**: none of these is modelled.

## Running it

```
spec/check.sh     # needs quint, TLA2TOOLS_JAR, java, lake (Lean 4.35.0), go; CODEQL for S10
```

To check one model by hand:
`quint verify quint/snooze.qnt --main=snoozeAll --backend=tlc --invariant=snoozedMeansPaused`.
