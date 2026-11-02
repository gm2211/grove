// A job queued on a pool whose only worker is snoozed (L3, L4 in spec/README.md).
//
// Models, per 30s round, the controllers `grove serve` runs: the snooze Expirer
// (internal/snooze Expirer.Tick), the fleet tick (fleet.Reconciler.Plan/Apply, then
// PoolHealth.ObserveFleet), the VM booting and enrolling into Nomad, and Nomad placing the job.
// The dispatch reconcile (dispatch.service.reconcile, poolWait, unplaceableReason) runs at least
// once a round and at any other moment: grove serve's loop and every API Get both call it.
//
// `fixed = false` is the code before spec/: WaitReason only honours a snooze whose Until is still
// in the future. `fixed = true` also honours a lapsed snooze the Expirer hasn't cleared yet, and
// one that ended after the fleet observation PoolHealth still holds was listed
// (snooze.Store.EndedAt vs Plan.ListedAt), and the Expirer resumes before it clears.
module dispatch {
  const fixed: bool
  const operatorPause: bool   // the worker is paused by an operator instead of snoozed
  const timeout: int          // pending timeout, in rounds (30m / 30s = 60; any value > settle time)

  var sn: str           // snooze entry: "active" | "lapsed" (Until passed, still recorded) | "none"
  var bit: bool         // Orchard SchedulingPaused
  var vm: str           // the pool's VM: "none" | "booting" | "ready" (a Nomad node of the pool)
  var obsPaused: bool   // PoolHealth.pausedOnly[pool] from the last completed fleet tick
  var endedAfterObs: bool     // the snooze ended after that observation's listing started
  var endedAfterListing: bool // ... after the in-flight fleet tick's listing started
  var fb: bool          // bit as the in-flight fleet tick listed it
  var fpc: str          // fleet tick: "f0" (idle) | "f1" (listed, not yet observed)
  var xpc: str          // Expirer: "x0" | "x1" (cleared, not yet resumed)
  var job: str          // "pending" | "running" | "success" | "failed" | "lost"
  var age: int          // rounds since the job's pendingFrom (Submitted, or PendingSince)
  var ran: Set[str]     // round controllers that finished their pass this round

  pure val controllers = Set("X", "F", "B", "N", "D")
  val terminal = Set("success", "failed", "lost").contains(job)

  action init = all {
    sn' = if (operatorPause) "none" else "active",
    bit' = true,
    vm' = "none",
    nondet o = Set(true, false).oneOf()   // the fleet may or may not have seen the pause yet
    obsPaused' = o,
    endedAfterObs' = false, endedAfterListing' = false, fb' = false,
    fpc' = "f0", xpc' = "x0",
    job' = "pending", age' = 0, ran' = Set(),
  }

  // --- snooze.Expirer.Tick for a lapsed snooze with no other reason ---
  // Before spec/ (fixed = false): ClearSnooze, then ResumeWorker.
  // Fixed: ResumeWorker, then ClearSnooze, which records the end time (Store.EndedAt): that time
  // is after the resume, so after every listing that could still have seen the worker paused.
  action expirer = any {
    all { xpc == "x0", not(ran.contains("X")), sn == "lapsed",
          sn' = if (fixed) sn else "none", bit' = if (fixed) operatorPause else bit, xpc' = "x1",
          vm' = vm, obsPaused' = obsPaused, endedAfterObs' = endedAfterObs,
          endedAfterListing' = endedAfterListing, fb' = fb, fpc' = fpc, job' = job, age' = age, ran' = ran },
    all { xpc == "x0", not(ran.contains("X")), sn != "lapsed",
          ran' = ran.union(Set("X")),
          sn' = sn, xpc' = xpc, bit' = bit, vm' = vm, obsPaused' = obsPaused, endedAfterObs' = endedAfterObs,
          endedAfterListing' = endedAfterListing, fb' = fb, fpc' = fpc, job' = job, age' = age },
    all { xpc == "x1",
          sn' = "none", bit' = operatorPause, xpc' = "x0", ran' = ran.union(Set("X")),
          endedAfterObs' = (fixed or endedAfterObs), endedAfterListing' = (fixed or endedAfterListing),
          vm' = vm, obsPaused' = obsPaused, fb' = fb, fpc' = fpc, job' = job, age' = age },
  }

  // --- fleet tick: Plan lists workers (ListedAt), Apply creates/deletes, ObserveFleet records ---
  action fleet = any {
    all { fpc == "f0", not(ran.contains("F")),
          fb' = bit, endedAfterListing' = false, fpc' = "f1",
          sn' = sn, bit' = bit, vm' = vm, obsPaused' = obsPaused, endedAfterObs' = endedAfterObs,
          xpc' = xpc, job' = job, age' = age, ran' = ran },
    all { fpc == "f1",
          obsPaused' = fb, endedAfterObs' = endedAfterListing, fpc' = "f0",
          vm' = if (fb) "none" else if (vm == "none") "booting" else vm,  // a paused worker's VMs go
          ran' = ran.union(Set("F")),
          sn' = sn, bit' = bit, endedAfterListing' = endedAfterListing, fb' = fb,
          xpc' = xpc, job' = job, age' = age },
  }

  // --- the VM boots and its node enrols (Reconciler.Enrol) within a round ---
  action boot = all {
    not(ran.contains("B")),
    vm' = if (vm == "booting") "ready" else vm, ran' = ran.union(Set("B")),
    sn' = sn, bit' = bit, obsPaused' = obsPaused, endedAfterObs' = endedAfterObs,
    endedAfterListing' = endedAfterListing, fb' = fb, fpc' = fpc, xpc' = xpc, job' = job, age' = age,
  }

  // --- Nomad places a pending job on a ready node of its pool ---
  action place = all {
    not(ran.contains("N")),
    job' = if (job == "pending" and vm == "ready") "running" else job, ran' = ran.union(Set("N")),
    sn' = sn, bit' = bit, vm' = vm, obsPaused' = obsPaused, endedAfterObs' = endedAfterObs,
    endedAfterListing' = endedAfterListing, fb' = fb, fpc' = fpc, xpc' = xpc, age' = age,
  }

  // --- the job's own run ---
  action finish = all {
    job == "running", job' = "success",
    sn' = sn, bit' = bit, vm' = vm, obsPaused' = obsPaused, endedAfterObs' = endedAfterObs,
    endedAfterListing' = endedAfterListing, fb' = fb, fpc' = fpc, xpc' = xpc, age' = age, ran' = ran,
  }

  // --- dispatch reconcile of a pending job with no allocation ---
  // PoolHealth.WaitReason: does a snooze of the pool's (observed-paused) worker hold the job?
  val waits = obsPaused and (if (fixed) (sn != "none" or endedAfterObs) else sn == "active")
  // PoolHealth.UnplaceableReason != "" with no eligible node: here only "every worker is paused"
  val unplaceable = vm != "ready" and obsPaused
  action reconcile = all {
    job == "pending",
    val a = if (waits and sn == "active") 0 else age        // PendingSince = Until, in the future
    all {
      age' = a,
      job' = if (a > timeout) "lost" else if (unplaceable and not(waits)) "failed" else job,
    },
    ran' = ran.union(Set("D")),  // runs at least once a round, and on every API Get
    sn' = sn, bit' = bit, vm' = vm, obsPaused' = obsPaused, endedAfterObs' = endedAfterObs,
    endedAfterListing' = endedAfterListing, fb' = fb, fpc' = fpc, xpc' = xpc,
  }

  // --- time: the snooze's Until passes at any moment; a round ends once every controller ran ---
  action lapse = all {
    sn == "active", sn' = "lapsed",
    bit' = bit, vm' = vm, obsPaused' = obsPaused, endedAfterObs' = endedAfterObs,
    endedAfterListing' = endedAfterListing, fb' = fb, fpc' = fpc, xpc' = xpc, job' = job, age' = age, ran' = ran,
  }
  action tick = all {
    ran.union(if (job == "pending") Set() else Set("D")) == controllers, fpc == "f0", xpc == "x0",
    ran' = Set(), age' = if (job == "pending" and age <= timeout) age + 1 else age,
    sn' = sn, bit' = bit, vm' = vm, obsPaused' = obsPaused, endedAfterObs' = endedAfterObs,
    endedAfterListing' = endedAfterListing, fb' = fb, fpc' = fpc, xpc' = xpc, job' = job,
  }

  action step = any { expirer, fleet, boot, place, finish, reconcile, lapse, tick }

  // --- properties ---
  // L4 (safety half): a job that waits only on a snooze is never failed for it, nor timed out.
  val neverFailedForSnooze = job != "failed"
  val neverLost = job != "lost"

  temporal fairness = all {
    weakFair(expirer, (sn, bit, vm, obsPaused, endedAfterObs, endedAfterListing, fb, fpc, xpc, job, age, ran)),
    weakFair(fleet, (sn, bit, vm, obsPaused, endedAfterObs, endedAfterListing, fb, fpc, xpc, job, age, ran)),
    weakFair(boot, (sn, bit, vm, obsPaused, endedAfterObs, endedAfterListing, fb, fpc, xpc, job, age, ran)),
    weakFair(place, (sn, bit, vm, obsPaused, endedAfterObs, endedAfterListing, fb, fpc, xpc, job, age, ran)),
    weakFair(finish, (sn, bit, vm, obsPaused, endedAfterObs, endedAfterListing, fb, fpc, xpc, job, age, ran)),
    weakFair(reconcile, (sn, bit, vm, obsPaused, endedAfterObs, endedAfterListing, fb, fpc, xpc, job, age, ran)),
    weakFair(lapse, (sn, bit, vm, obsPaused, endedAfterObs, endedAfterListing, fb, fpc, xpc, job, age, ran)),
    weakFair(tick, (sn, bit, vm, obsPaused, endedAfterObs, endedAfterListing, fb, fpc, xpc, job, age, ran)),
  }
  // L3: every job reaches a terminal state.
  temporal terminates = fairness implies eventually(terminal)
  // L4 (liveness half): the job queued behind the snooze runs once it ends.
  temporal runsAfterSnooze = fairness implies eventually(job == "success")
  // Not vacuous: a pool paused only by an operator still fails its job fast, with the reason.
  temporal operatorPauseFailsFast = fairness implies eventually(job == "failed")
}

module dispatchFixed { import dispatch(fixed = true, operatorPause = false, timeout = 6).* }
module dispatchBuggy { import dispatch(fixed = false, operatorPause = false, timeout = 6).* }
module dispatchOperator { import dispatch(fixed = true, operatorPause = true, timeout = 6).* }
