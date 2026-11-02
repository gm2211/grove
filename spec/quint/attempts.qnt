// A sealed-pool job's attempts (S2, L5 in spec/README.md): sandbox.Runner.Run's loop in
// internal/sandbox/runner.go and attemptLog.count in internal/sandbox/attempts.go.
//
// Each pass acquires a VM on some machine (any machine: the model lets the adversary choose,
// which covers acquire's preference for machines the job wasn't lost on) and the run ends one of
// five ways: it exits (any code but lost), it is lost before or after the job's script started,
// the Mac is snoozed with --now (evicted), or the VM was shutting down (draining).
module attempts {
  const retryLost: bool
  const maxEvictions: int   // how many --now snoozes the owners make while the job runs
  const maxDrains: int      // how many VMs are found shutting down

  pure val MaxAttempts = 3         // dispatch.MaxAttempts
  pure val QuarantineMachines = 2  // dispatch.QuarantineMachines
  pure val machines = Set("m1", "m2", "m3")

  type Outcome = { kind: str, machine: str, started: bool }

  var outcomes: List[Outcome]   // attemptLog.record.Attempts (drained ones are dropped)
  var lostOn: Set[str]          // machines the job was lost on after its script started
  var status: str               // "running" | "exited" | "lost" | "gaveUp" | "quarantined"
  var evictions: int
  var drains: int

  // attemptLog.count: evicted attempts do not count
  val counted = outcomes.select(o => o.kind != "evicted").length()
  val lostCounted = outcomes.select(o => o.kind == "lost").length()

  action init = all {
    outcomes' = List(), lostOn' = Set(), status' = "running", evictions' = 0, drains' = 0,
  }

  action exit = all {
    status == "running",
    nondet m = machines.oneOf()
    all { outcomes' = outcomes.append({ kind: "exited", machine: m, started: true }), status' = "exited" },
    lostOn' = lostOn, evictions' = evictions, drains' = drains,
  }

  action evicted = all {
    status == "running", evictions < maxEvictions,
    nondet m = machines.oneOf()
    outcomes' = outcomes.append({ kind: "evicted", machine: m, started: true }),
    evictions' = evictions + 1,
    lostOn' = lostOn, status' = status, drains' = drains,
  }

  action draining = all {
    status == "running", drains < maxDrains,
    drains' = drains + 1,   // rec.drop: the attempt is forgotten
    outcomes' = outcomes, lostOn' = lostOn, status' = status, evictions' = evictions,
  }

  action lost = all {
    status == "running",
    nondet m = machines.oneOf()
    nondet st = Set(true, false).oneOf()
    val o2 = outcomes.append({ kind: "lost", machine: m, started: st })
    val l2 = if (st) lostOn.union(Set(m)) else lostOn
    val n = o2.select(o => o.kind != "evicted").length()
    all {
      outcomes' = o2, lostOn' = l2,
      status' = if (l2.size() >= QuarantineMachines) "quarantined"
                else if (not(retryLost)) "lost"
                else if (n >= MaxAttempts) "gaveUp"
                else "running",
    },
    evictions' = evictions, drains' = drains,
  }

  action step = any { exit, evicted, draining, lost }

  val terminal = status != "running"

  // --- S2: attempt accounting ---
  // No job runs more than MaxAttempts counted attempts.
  val atMostMaxAttempts = counted <= MaxAttempts
  // Giving up takes MaxAttempts counted attempts; evictions never bring it closer.
  val gaveUpOnlyAfterMax = status == "gaveUp" implies counted == MaxAttempts
  // Quarantine takes losses (after the script started) on two different machines, never evictions.
  val quarantineNeedsTwoMachines = status == "quarantined" implies
    outcomes.select(o => o.kind == "lost" and o.started).foldl(Set(), (s, o) => s.union(Set(o.machine))).size() >= QuarantineMachines
  // A job that was only ever evicted (or drained) is still running or exited: never lost for it.
  val evictionsAreFree = lostCounted == 0 implies Set("running", "exited").contains(status)
  val accounting = atMostMaxAttempts and gaveUpOnlyAfterMax and quarantineNeedsTwoMachines and evictionsAreFree

  // --- L5: every sealed job terminates (owners snooze and VMs drain only finitely often) ---
  temporal fairness = weakFair(step, (outcomes, lostOn, status, evictions, drains))
  temporal terminates = fairness implies eventually(terminal)
}

module attemptsRetry { import attempts(retryLost = true, maxEvictions = 2, maxDrains = 1).* }
module attemptsNoRetry { import attempts(retryLost = false, maxEvictions = 2, maxDrains = 1).* }
