// A shared pool's lost job (S23, L11 in spec/README.md): internal/dispatch/reschedule.go
// (handleLosses, decideLost) and the `disconnect { replace = false }` block in nomad/jobs.
//
// The job runs as one Nomad job at a time. Nomad places its allocation on some machine (the
// adversary picks which), and the run either exits or is lost with its machine, before or after
// the script started. Grove's reconcile then records the loss and reruns the job (a new Nomad job;
// the old one is stopped and purged), or ends it, stopping its Nomad job first. Nomad may also
// place its own replacement for a lost allocation (`nomadReplaces`): it does, on Nomad 2.0.7, even
// with `reschedule { attempts = 0 }`, unless the group says `disconnect { replace = false }`.
//
// `fixed = false` is grove before reschedule.go: a lost allocation ended the job as lost, without
// stopping its Nomad job, and a newer allocation (Nomad's replacement) was simply followed.
module reschedule {
  const retryLost: bool
  const poisoned: bool       // a job with the same kind, pool, repo, ref and script was quarantined
  const nomadReplaces: bool  // no `disconnect { replace = false }` in the job
  const fixed: bool

  pure val MaxAttempts = 3         // dispatch.MaxAttempts
  pure val QuarantineMachines = 2  // dispatch.QuarantineMachines
  pure val machines = Set("m1", "m2", "m3")
  pure val runCap = 5              // keeps the state space finite where nothing else bounds runs

  type Loss = { machine: str, started: bool }

  var alloc: str            // the current Nomad job's latest allocation: none | running | lost | exited
  var where: str            // the machine it is on
  var live: bool            // Nomad holds a copy of the job that can still run (not stopped)
  var losses: List[Loss]    // every allocation Nomad lost, in order
  var recorded: int         // how many of them grove recorded (Job.Attempts)
  var status: str           // grove's record: pending | running | exited | lost | quarantined
  var runs: int             // runs started, ever
  var unwatched: int        // runs started while grove's record said the job had ended

  val terminal = Set("exited", "lost", "quarantined").contains(status)

  // decideLost
  pure def lostMachines(ls: List[Loss]): Set[str] =
    ls.select(o => o.started).foldl(Set(), (s, o) => s.union(Set(o.machine)))
  pure def verdict(ls: List[Loss]): str =
    if (lostMachines(ls).size() >= QuarantineMachines) "quarantine"
    else if (poisoned and ls[ls.length() - 1].started) "quarantine"
    else if (not(retryLost)) "lost"
    else if (ls.length() >= MaxAttempts) "lost"
    else "rerun"

  action init = all {
    alloc' = "none", where' = "", live' = true, losses' = List(), recorded' = 0,
    status' = "pending", runs' = 0, unwatched' = 0,
  }

  // Nomad starts a run: the queued copy, or (nomadReplaces) a replacement for a lost allocation.
  action start(m: str): bool = all {
    runs' = runs + 1, alloc' = "running", where' = m,
    unwatched' = if (terminal) unwatched + 1 else unwatched,
    live' = live, losses' = losses, recorded' = recorded, status' = status,
  }
  action place = all {
    live, alloc == "none", runs < runCap,
    nondet m = machines.oneOf()
    start(m),
  }
  action nomadReplace = all {
    nomadReplaces, live, alloc == "lost", runs < runCap,
    nondet m = machines.oneOf()
    start(m),
  }

  action exit = all {
    alloc == "running", alloc' = "exited",
    where' = where, live' = live, losses' = losses, recorded' = recorded, status' = status,
    runs' = runs, unwatched' = unwatched,
  }

  action lose = all {
    alloc == "running",
    nondet st = Set(true, false).oneOf()
    all { alloc' = "lost", losses' = losses.append({ machine: where, started: st }) },
    where' = where, live' = live, recorded' = recorded, status' = status,
    runs' = runs, unwatched' = unwatched,
  }

  // grove's reconcile, reschedule.go
  action groveFixed = all {
    not(terminal),
    if (losses.length() > recorded) {
      val v = verdict(losses)
      if (alloc == "exited")
        // the run after the losses (Nomad's replacement) finished: its result stands
        all { recorded' = losses.length(), status' = "exited", alloc' = alloc, live' = live }
      else if (v == "rerun" and alloc == "running")
        // Nomad's replacement is the rerun
        all { recorded' = losses.length(), status' = "running", alloc' = alloc, live' = live }
      else if (v == "rerun")
        // requeue: a new Nomad job, the old one stopped and purged
        all { recorded' = losses.length(), status' = "pending", alloc' = "none", live' = true }
      else
        // end it, stopping the Nomad job first
        all { recorded' = losses.length(), status' = if (v == "quarantine") "quarantined" else "lost",
              alloc' = alloc, live' = false }
    } else if (alloc == "exited") {
      all { status' = "exited", recorded' = recorded, alloc' = alloc, live' = live }
    } else all {
      alloc == "running", status != "running",
      status' = "running", recorded' = recorded, alloc' = alloc, live' = live,
    },
    where' = where, losses' = losses, runs' = runs, unwatched' = unwatched,
  }

  // grove before reschedule.go: a lost allocation ends the job, and Nomad's job is left alone
  action groveBefore = all {
    not(terminal),
    any {
      all { alloc == "lost", status' = "lost" },
      all { alloc == "exited", status' = "exited" },
      all { alloc == "running", status != "running", status' = "running" },
    },
    recorded' = losses.length(), alloc' = alloc, live' = live,
    where' = where, losses' = losses, runs' = runs, unwatched' = unwatched,
  }

  action grove = if (fixed) groveFixed else groveBefore

  action step = any { place, nomadReplace, exit, lose, grove }

  // --- S23: a lost job's runs are grove's to count ---
  // Nothing runs while grove says the job ended: no Nomad copy outlives the record.
  val noUnwatchedRun = unwatched == 0
  // Once grove ends the job as lost or quarantined, Nomad holds no copy that could still run.
  val endedMeansStopped = Set("lost", "quarantined").contains(status) implies not(live)
  // A job runs at most MaxAttempts times, and once when it may not be rerun.
  val atMostMaxRuns = runs <= MaxAttempts and (not(retryLost) implies runs <= 1)
  // Quarantine needs losses after the script started on two machines, or one loss after the
  // script started of a job like one already quarantined.
  val quarantineHasCause = status == "quarantined" implies
    (lostMachines(losses).size() >= QuarantineMachines or (poisoned and lostMachines(losses).size() >= 1))
  // A job lost only during setup is never quarantined.
  val setupLossesAreInnocent = lostMachines(losses).size() == 0 implies status != "quarantined"
  val accounting = noUnwatchedRun and endedMeansStopped and atMostMaxRuns and quarantineHasCause and setupLossesAreInnocent

  // --- L11: every shared-pool job ends ---
  temporal fairness = weakFair(step, (alloc, where, live, losses, recorded, status, runs, unwatched))
  temporal terminates = fairness implies eventually(terminal)
}

module rescheduleRetry { import reschedule(retryLost = true, poisoned = false, nomadReplaces = false, fixed = true).* }
module rescheduleNoRetry { import reschedule(retryLost = false, poisoned = false, nomadReplaces = false, fixed = true).* }
module reschedulePoisoned { import reschedule(retryLost = true, poisoned = true, nomadReplaces = false, fixed = true).* }
// grove's handling alone, with Nomad still replacing lost allocations (no disconnect block): one
// run too many slips in between Nomad's replacement and grove's next reconcile.
module rescheduleNomadReplaces { import reschedule(retryLost = true, poisoned = false, nomadReplaces = true, fixed = true).* }
// grove before this change, with Nomad replacing lost allocations as it does by default.
module rescheduleBefore { import reschedule(retryLost = true, poisoned = false, nomadReplaces = true, fixed = false).* }
module rescheduleBeforeNoRetry { import reschedule(retryLost = false, poisoned = false, nomadReplaces = true, fixed = false).* }
