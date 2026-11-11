# spec/ — what grove promises, and the checks that hold it to them

Twenty-two properties of grove, ten about progress (liveness, L) and twelve about who can do what
(security, S), each written down in English, stated in the tool that fits it, and checked against
the Go code. `spec/check.sh` runs all of it and fails if any result changes, in either direction:
a property that should hold fails, or a "before" model stops finding the bug it was written for.
CI runs it on every push (the `spec` job).

| Tool | Used for | Where |
|---|---|---|
| **Quint**, checked by **TLC** (exhaustive) | control loops that race: snooze, dispatch, attempts, fleet, enrolment | `quint/*.qnt` |
| **Lean 4** | pure decisions over every possible input: route auth, artifact paths, the broker's swap | `lean/Grove/*.lean` |
| **CodeQL** taint tracking | where secrets can flow in the Go code | `codeql/*.ql` |
| **Specify** (`gm2211/specify`) | the same 22 properties as behaviors with stable IDs, linked to the Quint invariants and Lean theorems Specify can run | `grove.spec/` |
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
| S5 | A `broker` credential reaches only `GET /broker/entries` (the same route also carries, on request with `?vmImagePull=1`, the registry login for that worker's own VM images; no separate route exists for it). | C | Lean `Auth.lean` | Proved, for every path. |
| S6 | `worker:snooze:<w>` reaches only `/workers/<w>/snooze` and `/workers/<w>/wake`. | C | Lean `Auth.lean` | Proved, for every path and worker name. |
| S7 | Scope boundaries: `read` can't submit, cancel or re-key a job; plain `dispatch` can't run a shell job; access, secrets, GitHub, registry pull credentials, VMs, workers and join approval are operator-only. | C, J | Lean `Auth.lean`, a differential test (every route above with hostile parameters, plus 3,000 random paths, each under five methods), and a test of hostile path spellings | Proved for every route, with any parameters. |
| S8 | The broker writes a real token only into `Authorization` or `X-Api-Key`, only for its kind's hosts, only from a live entry, and only to the job's own VM. Other headers leave exactly as the job sent them. | J | Lean `Broker.lean` | Lookup and header swap in Lean: a token leaves only from an entry bound to the address the request came from, and an unbound entry is refused from every address (`unbound_refused`; it used to be honoured from any, the old finding). That the bound address is the job VM's is **not modelled**: the worker's broker resolves `Entry.VM` through the worker's own Tart on each sync and leaves out an entry it can't bind (`Syncer.bind`, `TartSources`), which Go tests cover (`internal/broker/sync_test.go`, `source_test.go`, `proxy_test.go`), and it rests on A6. The Lean changes for this (`lookup`'s unbound case, `lookup_bound`, the stronger `S8_inject_sound`) were written without a Lean toolchain at hand: until `spec/check.sh` has run on them, read them as unchecked. |
| S9 | An artifact path can't leave the job's artifact directory. | C, J | Lean `ArtifactPath.lean` + differential test | Proved. |
| S10 | Job secrets, the private-repo token, the registry pull login, broker credentials and node keys reach only their allowed destinations (the registry pull login: a Linux VM's Docker config, the registry it was armed for, and, in the `GET /broker/entries?vmImagePull=1` answer to the one worker whose pools' VM images it covers, which keeps it in its Orchard process environment only): never argv, a log, an API response, the job record or a VM spec. Job secrets and the private-repo token go into the dispatch payload only, never dispatch meta. | J, C | CodeQL, 5 queries | No flow found. A coverage query finds each secret at its allowed destination, which shows the queries aren't vacuous. The job-secret queries were changed (allowed destination: the payload, no longer `env_json`; dispatch meta is now a leak sink) without CodeQL at hand: until `spec/check.sh` has run with `CODEQL` set, read that row as unchecked. Go tests cover the same thing (`internal/dispatch/secrets_test.go`), and what the templates do with the payload is tested in `nomad/jobs/secrets_test.go`, not modelled. |
| S11 | A shared-pool job's image is its pool's runner image or a digest-pinned image its pool allows (the control plane pins a tag to its digest at submit, or refuses the job), chosen by the control plane; a job with an environment leaves its pool only for a macOS pool that accepts its control-plane-validated native toolchain; the clone token runs only in the runner image or the root source task. | J (a caller choosing its own image, via `environment` or `meta`) | Go tests: `internal/dispatch/environment_test.go`, `internal/dispatch/environment_tag_test.go`, `nomad/jobs/nomadjobs_test.go` | Holds. A caller's `meta.image` used to be copied into dispatch meta; it no longer is. |
| S22 | A toolchain cache entry is written only by `_grovetool`, from versions the control plane validated; the job user can't write it. | J (a caller choosing what the macOS worker installs, or poisoning the cache a later job runs from) | Go tests: `internal/dispatch/toolchain_test.go` (Submit alone sets `toolchain_json`, from the validated, re-marshalled spec), `internal/fleet/isolation_test.go` (the installer runs as `_grovetool` from root-owned code, the cache is not writable by `_grovejob`) | Holds. |

### How the models are tied to the code

A model proves things about itself. Three kinds of check make the build break when the Go code
stops doing what the model says:

- **Trace replay (S1, L1, L2).** `spec/check.sh` samples 1,000 random traces of the fixed snooze
  model (`snoozeMBT`, `snoozeMBTFresh`: every writer, a second snooze request, one failed Orchard
  resume) with `quint run --out-itf`. `TestSnoozeConformsToModel` replays each against the real
  code: `snooze.Engage`, `snooze.Wake`, `POST /workers/{name}/pause` and `/resume` on the real
  HTTP server, `health.Monitor.Tick` and `snooze.Expirer.Tick`, with a fake Orchard. After every
  request it compares Orchard's pause bit and the recorded reasons with the model's state. The
  chain is: every writer holds the worker's lock, so requests serialize (`TestS1_WritersHoldTheWorkersLock`,
  under `-race`); each request does what the model's step does (the replay); and the model has
  the properties (TLC).
- **Differential tests (S5-S7, S9).** The Lean decisions run as a binary and are compared with the
  Go functions on every route pair and on hostile paths.
- **Scenario tests (L4, L6, S3, L2).** The counterexamples TLC found, replayed against the real
  packages, plus a 500-fleet property test for convergence.

`spec/mutants.py` then puts each old bug back into a scratch copy, one at a time, and requires a
conformance test to fail on it. A mutant that survives, or whose code is gone, fails the build.
Today it holds 6: Engage marking a lapsed snooze's pause external, Wake and the Expirer clearing
before resuming, resume going through while snoozed, pause skipping the operator reason, and Wake
without the lock.

The replay found one bug the model checks couldn't, because the Go code and the model disagreed:
snoozing a Mac again after its snooze lapsed but before the Expirer cleared it recorded the pause
as external, so the Mac stayed paused after the new snooze ended. `Engage` now treats a recorded
snooze, lapsed or not, as grove's own pause.

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
- **A6.** A VM's address: vmnet's shared network doesn't let one VM send from another VM's
  address, Tart reports the address a running VM actually holds (`tart ip`, from the DHCP lease
  of its MAC), and Orchard runs VM `<name>` as the local Tart VM `orchard-<name>-<uid>-<restarts>`.
  S8's VM binding is only as strong as these, and none is checked against a real Mac by a test here.

### Not covered here (still open from the security review)

- **C2**, in part: named secrets left `env_json` for the dispatch payload (S10 now counts dispatch
  meta as a leak). What remains is that Nomad keeps a dispatched job's payload, so a Nomad token
  that can read jobs can read it; nothing here models Nomad's ACLs.
- **H4**: sender-bound secrets.
- **H9**: per-worker Orchard scope (see A3).
- **M5, M7, M10, M13, M14, L12**: none of these is modelled.

## Running it

```
spec/check.sh     # needs quint, TLA2TOOLS_JAR, java, lake (Lean 4.35.0), go; CODEQL for S10; SPECIFY for grove.spec
```

`grove.spec/` is the [Specify](https://github.com/gm2211/specify) view of the same properties: one
behavior per property, its ID in `details` and `tags`, and 18 formal links. `specify formal check
--spec spec/grove.spec` runs them (Quint invariants by seeded simulation, Lean theorems with their
axioms checked). Specify's formal check runs invariants only, so the temporal properties (L1-L5,
L7's replacement half, L9, L10), L6 and S10 stay unlinked there and are checked by the rows above.

To check one model by hand:
`quint verify quint/snooze.qnt --main=snoozeAll --backend=tlc --invariant=snoozedMeansPaused`.
