// One job's whole life, with everything in grove that touches it at once (S24, L12 in
// spec/README.md): internal/dispatch service.go (Submit, reconcile, Cancel), reschedule.go
// (handleLosses, requeue), fallback.go (redispatch) and evict.go.
//
// The other models each look at one controller. This one looks at the record they share. Grove
// keeps a job as one record (jobs.json) naming one Nomad copy of it (NomadJobID). Many callers
// reconcile that record: the 30 s sweep (RunReconciler), every API Get and List, Evict, a log
// follower's watchForTerminal. Each reads the record, talks to Nomad, and writes the whole record
// back. Cancel does the same. Nothing else orders them, so a writer can act on a record another
// writer has already changed.
//
// Actors: Submit; two reconcilers R1 (the sweep: fair) and R2 (any other caller: may never come);
// one Cancel (may never come). Nomad: copies of the job, each queued, running, unknown (its node
// stopped answering; it may still run there), lost, exited, stopped or purged. The adversary picks
// when a copy is placed (or never: the fleet may stay full), lost, or comes back. Time is `age`,
// the rounds a pending record has waited.
//
// Each Nomad call sequence one actor makes inside a step (redispatch's Dispatch, then StopJob of
// the old copy) is atomic here; what interleaves is the actors, between their read of the record
// and their write of it. That is where the bugs this model was written for live.
//
// `fixed = false` is grove before this change:
//   - no per-job lock: reconcile works on the copy of the record its caller read (often a List a
//     sweep ago), and writes it back over whatever is there; Cancel likewise;
//   - Cancel overwrites a finished job's status;
//   - a Dispatch error is a refusal and nothing is recorded, though Nomad may have created the
//     copy (a timeout after Nomad committed it, or a commit landing after the error).
// `fixed = true`: every writer of a job's record holds that job's lock from reading it to its
// last write, and re-reads it under the lock (dispatch.service.lockJob); Cancel of a finished job
// changes nothing; Submit dispatches with the job's id as Nomad's idempotency token, and after a
// Dispatch error stops any copy with that token, keeps a tombstone record, and the sweep keeps
// hunting for such a copy until the commit window has passed (A7).
module lifecycle {
  const fixed: bool
  const retryLost: bool
  const ambiguous: bool     // Nomad may fail a Dispatch it committed (or commits later)

  pure val MaxAttempts = 3     // dispatch.MaxAttempts
  pure val T = 2               // the pending timeout, in rounds
  pure val CopyCap = 6         // keeps the state space finite; never reached by the fixed code

  type JobRec = { status: str, copy: int, losses: int, orphan: bool }
  pure val NoRec: JobRec = { status: "none", copy: 0, losses: 0, orphan: false }

  pure def isTerminal(st: str): bool = Set("done", "lost", "canceled", "refused").contains(st)
  pure val liveStates = Set("queued", "running", "unknown")

  var copies: int -> str     // Nomad's copies of the job
  var nextCopy: int              // the id the next Dispatch gets
  var rec: JobRec               // grove's record (status "none": no record)
  var sub: str               // Submit: init | ok | err | accepted | refused
  var pendingCommit: bool    // Nomad has yet to commit a Dispatch that already returned an error
  var windowOpen: bool       // that commit may still land (A7)
  var pc: str -> str         // R1, R2, C: idle | read
  var snap: str -> JobRec       // what each actor read
  var seen: str -> str          // what ListAllocations said of its copy, right after
  var lock: str              // the job's lock: "" or its holder (fixed only)
  var runs: int              // copies Nomad started, ever
  var firstEnd: str          // the first terminal status the record took
  var age: int               // rounds since the record's copy was dispatched (the clock against PendingSince)

  val liveCopies = copies.keys().filter(c => liveStates.contains(copies.get(c)))
  def isLive(c: int): bool = copies.keys().contains(c) and liveStates.contains(copies.get(c))

  // Every write of the record goes through here, so firstEnd remembers the first end.
  action setRec(r: JobRec): bool = all {
    rec' = r,
    // a new copy waits afresh (requeue sets PendingSince); otherwise time is time
    age' = if (r.copy != rec.copy) 0 else age,
    firstEnd' = if (firstEnd == "" and isTerminal(r.status)) r.status else firstEnd,
  }

  action init = all {
    copies' = Map(), nextCopy' = 1, rec' = NoRec, sub' = "init", pendingCommit' = false,
    windowOpen' = false, pc' = Set("R1", "R2", "C").mapBy(_ => "idle"),
    snap' = Set("R1", "R2", "C").mapBy(_ => NoRec), seen' = Set("R1", "R2", "C").mapBy(_ => ""), lock' = "", runs' = 0, firstEnd' = "", age' = 0,
  }

  // --- Submit -------------------------------------------------------------------------------
  // Nomad answers the Dispatch: ok, a clean refusal, an error after it created the copy, or an
  // error with the commit still to land.
  action submitDispatch = all {
    sub == "init",
    nondet outcome = (if (ambiguous) Set("ok", "refuse", "ambiguous", "late") else Set("ok", "refuse")).oneOf()
    all {
      copies' = if (outcome == "ok" or outcome == "ambiguous") copies.put(nextCopy, "queued") else copies,
      nextCopy' = if (outcome == "late") nextCopy else nextCopy + 1,
      sub' = if (outcome == "ok") "ok" else "err",
      pendingCommit' = outcome == "late",
      windowOpen' = outcome != "ok",
    },
    rec' = rec, pc' = pc, snap' = snap, seen' = seen, lock' = lock, runs' = runs, firstEnd' = firstEnd, age' = age,
  }

  // A late commit lands. Nomad's idempotency token stops a second one.
  action commitLate = all {
    pendingCommit,
    copies' = copies.put(nextCopy, "queued"), nextCopy' = nextCopy + 1, pendingCommit' = false,
    rec' = rec, sub' = sub, windowOpen' = windowOpen, pc' = pc, snap' = snap, seen' = seen, lock' = lock,
    runs' = runs, firstEnd' = firstEnd, age' = age,
  }

  // A7: a Dispatch Nomad has not committed by the end of the window never commits.
  action closeWindow = all {
    windowOpen, not(pendingCommit),
    windowOpen' = false,
    copies' = copies, nextCopy' = nextCopy, rec' = rec, sub' = sub, pendingCommit' = pendingCommit,
    pc' = pc, snap' = snap, seen' = seen, lock' = lock, runs' = runs, firstEnd' = firstEnd, age' = age,
  }

  action submitFinish = all {
    sub == "ok" or sub == "err",
    if (sub == "ok") all {
      setRec({ status: "pending", copy: 1, losses: 0, orphan: false }),
      sub' = "accepted", copies' = copies,
    } else if (not(fixed)) all {
      // before: the caller is told no, and nothing is kept
      sub' = "refused", copies' = copies, rec' = rec, firstEnd' = firstEnd, age' = age,
    } else all {
      // fixed: stop whatever copy carries the token (the sweep may fail), keep a tombstone
      nondet nomadUp = Set(true, false).oneOf()
      copies' = if (nomadUp and isLive(1)) copies.set(1, "purged") else copies,
      setRec({ status: "refused", copy: 1, losses: 0, orphan: true }),
      sub' = "refused",
    },
    nextCopy' = nextCopy, pendingCommit' = pendingCommit, windowOpen' = windowOpen,
    pc' = pc, snap' = snap, seen' = seen, lock' = lock, runs' = runs,
  }

  // --- reconcile (R1, R2) and Cancel (C): read, then act -------------------------------------
  // before: reads whatever record its caller had (a List's copy: any version since submit, here
  // the current one at some earlier moment) and holds no lock. fixed: takes the job's lock and
  // reads the record under it.
  action read(a: str): bool = all {
    pc.get(a) == "idle",
    rec.status != "none",
    a == "C" or not(isTerminal(rec.status)) or rec.orphan,
    fixed implies lock == "",
    pc' = pc.set(a, "read"), snap' = snap.set(a, rec), lock' = if (fixed) a else lock,
    seen' = seen.set(a, if (copies.keys().contains(rec.copy)) copies.get(rec.copy) else "purged"),
    copies' = copies, nextCopy' = nextCopy, rec' = rec, sub' = sub, pendingCommit' = pendingCommit,
    windowOpen' = windowOpen, runs' = runs, firstEnd' = firstEnd, age' = age,
  }

  action done(a: str): bool = all { pc' = pc.set(a, "idle"), lock' = if (fixed) "" else lock }

  // dispatch.service.reconcile on the record s, as one step against Nomad.
  action reconcileAct(a: str): bool = all {
    a != "C", pc.get(a) == "read",
    val s = snap.get(a)
    val c = s.copy
    val cs = seen.get(a)
    // redispatch reads the old copy's meta and payload back (DispatchInput), any time after the
    // listing: before another actor purged that copy, or after, when the 404 is an error and
    // nothing is written
    nondet early = Set(true, false).oneOf()
    val canRedispatch = nextCopy <= CopyCap and
      (early or (copies.keys().contains(c) and copies.get(c) != "purged"))
    any {
      // the tombstone sweep: stop any copy with the token; drop the tombstone once the window
      // has passed and a sweep worked
      all { s.orphan,
            copies' = if (isLive(c)) copies.set(c, "purged") else copies,
            setRec(if (not(windowOpen)) { ...s, orphan: false } else s),
            nextCopy' = nextCopy, runs' = runs },
      all { not(s.orphan), isTerminal(s.status),
            // only a Cancel's read can hold a finished record; reconcile skips it
            copies' = copies, rec' = rec, firstEnd' = firstEnd, age' = age, nextCopy' = nextCopy, runs' = runs },
      all { not(s.orphan), not(isTerminal(s.status)), any {
        // ListAllocations: 404, the Nomad job is gone
        all { cs == "purged", setRec({ ...s, status: "lost" }), copies' = copies, nextCopy' = nextCopy },
        // no allocation past the pending timeout: stop and purge it, mark it lost
        all { cs == "queued", age > T,
              copies' = copies.set(c, "purged"), setRec({ ...s, status: "lost" }), nextCopy' = nextCopy },
        // no allocation yet: the record is written back unchanged (store.put of what was read)
        all { cs == "queued", age <= T, setRec(s), copies' = copies, nextCopy' = nextCopy },
        all { cs == "running", setRec({ ...s, status: "running" }), copies' = copies, nextCopy' = nextCopy },
        // ran to its end (a stopped copy's allocation ends killed: failed)
        all { cs == "exited" or cs == "stopped", setRec({ ...s, status: "done" }), copies' = copies, nextCopy' = nextCopy },
        // unknown within the grace: still running
        all { cs == "unknown", setRec({ ...s, status: "running" }), copies' = copies, nextCopy' = nextCopy },
        // lost (or unknown past the grace): rerun within the budget, else end it, stopping it
        all { cs == "unknown" or cs == "lost",
              if (retryLost and s.losses + 1 < MaxAttempts and not(canRedispatch)) all {
                // the redispatch failed: nothing changes, the next reconcile tries again
                copies' = copies, nextCopy' = nextCopy, rec' = rec, firstEnd' = firstEnd, age' = age,
              } else if (retryLost and s.losses + 1 < MaxAttempts) all {
                // requeue: redispatch a new copy, purge the old one
                copies' = copies.set(c, "purged").put(nextCopy, "queued"), nextCopy' = nextCopy + 1,
                setRec({ ...s, status: "pending", copy: nextCopy, losses: s.losses + 1 }),
              } else all {
                copies' = if (isLive(c)) copies.set(c, "stopped") else copies, nextCopy' = nextCopy,
                setRec({ ...s, status: "lost", losses: s.losses + 1 }),
              } },
      }, runs' = runs },
    },
    done(a),
    snap' = snap, seen' = seen, sub' = sub, pendingCommit' = pendingCommit, windowOpen' = windowOpen,
  }

  // Any Nomad call of a reconcile fails: it writes nothing (the next one tries again).
  action reconcileFails(a: str): bool = all {
    a != "C", pc.get(a) == "read",
    done(a),
    copies' = copies, nextCopy' = nextCopy, rec' = rec, firstEnd' = firstEnd, age' = age, runs' = runs,
    snap' = snap, seen' = seen, sub' = sub, pendingCommit' = pendingCommit, windowOpen' = windowOpen,
  }

  // dispatch.service.Cancel on the record s: StopJob of its copy, then the record says canceled.
  action cancelAct = all {
    pc.get("C") == "read",
    val s = snap.get("C")
    if (fixed and isTerminal(s.status)) all {
      copies' = copies, rec' = rec, firstEnd' = firstEnd, age' = age,
    } else all {
      copies' = if (isLive(s.copy)) copies.set(s.copy, "stopped") else copies,
      setRec({ ...s, status: "canceled" }),
    },
    done("C"),
    nextCopy' = nextCopy, snap' = snap, seen' = seen, sub' = sub, pendingCommit' = pendingCommit,
    windowOpen' = windowOpen, runs' = runs,
  }

  // --- Nomad and the machines ----------------------------------------------------------------
  action nomadMove(c: int, src: str, dst: str): bool = all {
    copies.keys().contains(c), copies.get(c) == src,
    copies' = copies.set(c, dst),
    runs' = if (src == "queued" and dst == "running") runs + 1 else runs,
    nextCopy' = nextCopy, rec' = rec, sub' = sub, pendingCommit' = pendingCommit, windowOpen' = windowOpen,
    pc' = pc, snap' = snap, seen' = seen, lock' = lock, firstEnd' = firstEnd, age' = age,
  }
  action place = nondet c = copies.keys().oneOf() nomadMove(c, "queued", "running")
  action exit = nondet c = copies.keys().oneOf() nomadMove(c, "running", "exited")
  action lose = nondet c = copies.keys().oneOf() nomadMove(c, "running", "unknown")
  action comeBack = nondet c = copies.keys().oneOf() nomadMove(c, "unknown", "running")
  action lostAfter = nondet c = copies.keys().oneOf() nomadMove(c, "unknown", "lost")

  // Time: a pending record waits another round.
  action tick = all {
    rec.status == "pending", age <= T,
    age' = age + 1, rec' = rec,
    copies' = copies, nextCopy' = nextCopy, sub' = sub, pendingCommit' = pendingCommit,
    windowOpen' = windowOpen, pc' = pc, snap' = snap, seen' = seen, lock' = lock, runs' = runs, firstEnd' = firstEnd,
  }

  action step = any {
    submitDispatch, submitFinish, commitLate, closeWindow,
    read("R1"), read("R2"), read("C"), reconcileAct("R1"), reconcileAct("R2"), cancelAct,
    reconcileFails("R1"), reconcileFails("R2"),
    place, exit, lose, comeBack, lostAfter, tick,
  }

  // --- S24: one job, one watched copy --------------------------------------------------------
  // Nomad never holds two copies of a job that could run.
  val oneLiveCopy = liveCopies.size() <= 1
  // Every copy that could run is the one grove's record follows, on a record still open (or a
  // refused submit's tombstone, still hunting it), or Submit is between Dispatch and persisting.
  val noUnwatchedCopy = liveCopies.forall(c =>
    (sub == "ok" or sub == "err")
    or (rec.copy == c and rec.status != "none" and (not(isTerminal(rec.status)) or rec.orphan)))
  // A job that ended stays ended: no cancel turns a success into "canceled", no stale reconcile
  // turns a canceled or lost job back into a pending or running one.
  val endIsFinal = firstEnd == "" or rec.status == firstEnd
  // Nomad starts the job at most MaxAttempts times (once when it may not be rerun).
  val atMostMaxRuns = runs <= MaxAttempts and (not(retryLost) implies runs <= 1)
  val safe = oneLiveCopy and noUnwatchedCopy and endIsFinal and atMostMaxRuns

  // Not vacuous: spec/check.sh requires each of these to fail on the fixed model, so the model
  // does reach a rerun, a cancel, and a refused submit's copy that grove then stopped.
  val neverRerun = rec.losses == 0 or rec.status != "pending"
  val neverCanceled = rec.status != "canceled"
  val refusedCopyNeverStopped = not(sub == "refused" and copies.keys().contains(1) and copies.get(1) == "purged")

  // --- L12: every job settles ----------------------------------------------------------------
  // The sweep (R1), a started Cancel, Submit, time and Nomad's own progress are fair; placement,
  // losses, a second reconciler and a Cancel are the adversary's to give or withhold. Strong
  // fairness where the adversary could otherwise interrupt forever: Nomad answers a reconcile
  // that keeps trying, a job that keeps getting a machine back finishes on one, and a node that
  // keeps going away is eventually given up on (lost_after), and the sweep gets the job's lock
  // (sync.Mutex hands a lock to a waiter starved for 1 ms).
  val vars = (copies, nextCopy, rec, sub, pendingCommit, windowOpen, pc, snap, seen, lock, runs, firstEnd, age)
  temporal fairness = all {
    weakFair(submitDispatch, vars), weakFair(submitFinish, vars), weakFair(commitLate, vars),
    weakFair(closeWindow, vars), strongFair(read("R1"), vars), strongFair(reconcileAct("R1"), vars),
    weakFair(reconcileAct("R2"), vars), weakFair(cancelAct, vars), weakFair(tick, vars),
    strongFair(exit, vars), strongFair(lostAfter, vars),
  }
  val settled = (sub == "accepted" or sub == "refused") and
    (rec.status == "none" or isTerminal(rec.status)) and not(rec.orphan) and liveCopies.size() == 0
  // Every submit settles: an accepted job ends, a refused one leaves nothing that could run.
  temporal settles = fairness implies eventually(always(settled))
}

module lifecycleFixed { import lifecycle(fixed = true, retryLost = true, ambiguous = true).* }
module lifecycleFixedNoRetry { import lifecycle(fixed = true, retryLost = false, ambiguous = true).* }
// grove before this change, with a Nomad that answers every Dispatch truthfully: the races
// between writers of the record alone.
module lifecycleBefore { import lifecycle(fixed = false, retryLost = true, ambiguous = false).* }
// ... and with a Dispatch that errors after (or before) Nomad commits it.
module lifecycleBeforeAmbiguous { import lifecycle(fixed = false, retryLost = true, ambiguous = true).* }
