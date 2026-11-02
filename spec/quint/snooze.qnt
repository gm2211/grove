// One Grove worker's pause state: pauses.json (internal/snooze) and Orchard's pause bit, with
// every code path that writes them: snooze.Engage, snooze.Wake, handleWorkerPause,
// handleWorkerResume (internal/server/handlers_workers.go), health.Monitor and snooze.Expirer.
// Each Go call that touches a different system is its own step, so TLC explores every
// interleaving of concurrent requests.
//
// Properties: S1 snoozedMeansPaused, L1 eventuallyWorkingAfterResume, L2 eventuallyConsistent
// (spec/README.md). The instances at the bottom are what CI checks.
module snooze {
  // fixed: the code as spec/ left it. Every request (and every Expirer pass) holds the worker's
  // mutex from its first step to its last (snooze.Store.Lock, internal/snooze/lock.go), and
  // Wake and the Expirer resume the worker BEFORE they clear the snooze, so a failed resume
  // leaves a lapsed snooze for the next Expirer pass instead of a pause with no reason.
  // fixed = false is the code before: no lock, clear first.
  const fixed: bool
  // Orchard may refuse one ResumeWorker call (network blip, Orchard restarting).
  const resumeCanFail: bool
  // Which concurrent writers exist in this run.
  const withPause: bool
  const withHealth: bool
  const withResume: bool
  const withEngage: bool
  const withWake: bool
  // A second snooze request (process F), so a trace can snooze the worker again, e.g. after the
  // first snooze lapsed but before the Expirer cleared it.
  const withSecondEngage: bool
  // Start with the worker paused by an operator (else running with no pause).
  const initOperatorPaused: bool
  // A snooze lapses only between requests. Go samples the clock once per request, so traces the
  // Go replay test (internal/conformance/snooze_mbt_test.go) runs come from this setting; the
  // checked instances leave it off, which allows strictly more behaviours.
  const atomicTime: bool

  var bit: bool            // Orchard SchedulingPaused
  var reasons: Set[str]    // pauses.json reasons
  var expired: bool        // the snooze's Until has passed
  var pc: str -> str       // per-process program counter
  var eb: bool             // bit as Engage read it
  var hb: bool             // bit as the health monitor read it
  var xe: Set[str]         // reasons as the Expirer listed them
  var xb: bool             // bit as the Expirer listed it
  var fails: int           // ResumeWorker failures still allowed

  pure val starts = Set("e0", "w0", "r0", "p0", "h0", "x0", "done", "refused")
  val active = reasons.contains("snooze") and not(expired)
  def mid(p: str): bool = not(starts.contains(pc.get(p)))
  def free(p: str): bool = not(fixed) or pc.keys().exclude(Set(p)).forall(q => not(mid(q)))
  def at(p: str, l: str): bool = free(p) and pc.get(p) == l

  action upd(nbit: bool, nreasons: Set[str], nexp: bool, p: str, l: str,
             neb: bool, nhb: bool, nxe: Set[str], nxb: bool): bool = all {
    bit' = nbit, reasons' = nreasons, expired' = nexp, pc' = pc.set(p, l),
    eb' = neb, hb' = nhb, xe' = nxe, xb' = nxb, fails' = fails,
  }
  // a ResumeWorker call that returns an error: nothing changes but the pc
  action failResume(p: str, l: str): bool = all {
    fails > 0, fails' = fails - 1,
    bit' = bit, reasons' = reasons, expired' = expired, pc' = pc.set(p, l),
    eb' = eb, hb' = hb, xe' = xe, xb' = xb,
  }
  // shorthand: change only the pc (and optionally one remembered value)
  action go(p: str, l: str): bool = upd(bit, reasons, expired, p, l, eb, hb, xe, xb)
  action goR(p: str, l: str, r: Set[str]): bool = upd(bit, r, expired, p, l, eb, hb, xe, xb)
  action goB(p: str, l: str, b: bool): bool = upd(b, reasons, expired, p, l, eb, hb, xe, xb)

  pure def startOr(on: bool, l: str): str = if (on) l else "done"
  action init = all {
    bit' = initOperatorPaused,
    reasons' = if (initOperatorPaused) Set("operator") else Set(),
    expired' = false,
    pc' = Map("E" -> startOr(withEngage, "e0"), "W" -> startOr(withWake, "w0"),
              "R" -> startOr(withResume, "r0"), "P" -> startOr(withPause, "p0"),
              "H" -> startOr(withHealth, "h0"), "F" -> startOr(withSecondEngage, "e0"), "X" -> "x0"),
    eb' = false, hb' = false, xe' = Set(), xb' = false,
    fails' = if (resumeCanFail) 1 else 0,
  }

  // snooze.Engage: read Orchard, maybe record "external", record snooze, pause if it wasn't
  action engageAs(p: str): bool = any {
    all { at(p, "e0"), upd(bit, reasons, expired, p, "e1", bit, hb, xe, xb) },
    all { at(p, "e1"), goR(p, "e2", if (eb and reasons == Set()) reasons.union(Set("operator")) else reasons) },
    all { at(p, "e2"), upd(bit, reasons.union(Set("snooze")), false, p, "e3", eb, hb, xe, xb) },
    all { at(p, "e3"), goB(p, "done", bit or not(eb)) },
  }
  action engage = any { engageAs("E"), engageAs("F") }
  // snooze.Wake: end the snooze, resume only if no other reason remains
  action wake = if (fixed) any {
    // resume (when nothing else holds the worker), then clear; a failed resume clears nothing
    all { at("W", "w0"), go("W", if (reasons.exclude(Set("snooze")) == Set()) "w1" else "w2") },
    all { at("W", "w1"), goB("W", "w2", false) },
    all { at("W", "w1"), failResume("W", "done") },
    all { at("W", "w2"), goR("W", "done", reasons.exclude(Set("snooze"))) },
  } else any {
    all { at("W", "w0"), goR("W", "w1", reasons.exclude(Set("snooze"))) },
    all { at("W", "w1"), go("W", if (reasons == Set()) "w2" else "done") },
    all { at("W", "w2"), goB("W", "done", false) },
    all { at("W", "w2"), failResume("W", "done") },
  }
  // handleWorkerResume: refuse while snoozed, clear operator/health, resume
  action resume = any {
    all { at("R", "r0"), go("R", if (active) "refused" else "r1") },
    all { at("R", "r1"), goR("R", "r2", reasons.exclude(Set("operator", "health"))) },
    all { at("R", "r2"), goB("R", "done", false) },
  }
  // handleWorkerPause: record operator, then pause
  action pause = any {
    all { at("P", "p0"), goR("P", "p1", reasons.union(Set("operator"))) },
    all { at("P", "p1"), goB("P", "done", true) },
  }
  // health.Monitor.Tick for a machine over its loss threshold
  action health = any {
    all { at("H", "h0"), upd(bit, reasons, expired, "H", "h1", eb, bit, xe, xb) },
    all { at("H", "h1"), hb,
          goR("H", "done", if (active and reasons.intersect(Set("health", "operator")) == Set()) reasons.union(Set("health")) else reasons) },
    all { at("H", "h1"), not(hb), goB("H", "h2", true) },
    all { at("H", "h2"), goR("H", "done", reasons.union(Set("health"))) },
  }
  // snooze.Expirer.Tick, forever
  action expirer = any {
    all { at("X", "x0"), upd(bit, reasons, expired, "X", "x1", eb, hb, reasons, xb) },
    all { at("X", "x1"), upd(bit, reasons, expired, "X", "x2", eb, hb, xe, bit) },
    // a lapsed snooze, before: clear it, then resume if no reason remains
    all { not(fixed), at("X", "x2"), xe.contains("snooze") and expired, goR("X", "x3", reasons.exclude(Set("snooze"))) },
    all { at("X", "x3"), goB("X", "x0", if (reasons == Set() and xb) false else bit) },
    all { at("X", "x3"), reasons == Set() and xb, failResume("X", "x0") },
    // fixed: resume if no other reason remains, then clear; a failed resume clears nothing
    all { fixed, at("X", "x2"), xe.contains("snooze") and expired,
          go("X", if (reasons.exclude(Set("snooze")) == Set() and xb) "x5" else "x6") },
    all { at("X", "x5"), goB("X", "x6", false) },
    all { at("X", "x5"), failResume("X", "x0") },
    all { at("X", "x6"), goR("X", "x0", reasons.exclude(Set("snooze"))) },
    // no lapsed snooze: re-pause a worker Orchard shows running that still has a reason
    all { at("X", "x2"), not(xe.contains("snooze") and expired), go("X", "x4") },
    all { at("X", "x4"), goB("X", "x0", if (not(xb) and reasons != Set()) true else bit) },
  }
  action lapse = all {
    reasons.contains("snooze"), not(expired),
    not(atomicTime) or pc.keys().forall(q => not(mid(q))),
    bit' = bit, reasons' = reasons, expired' = true, pc' = pc, eb' = eb, hb' = hb, xe' = xe, xb' = xb,
    fails' = fails,
  }

  action client = any { engage, wake, resume, pause, health }
  action step = any { client, expirer, lapse }

  // --- properties ---
  // CLAUDE.md: "The bit is set iff a reason is present."
  val pausedIffReason = (reasons != Set()) iff bit
  // S1, the owner's promise: a snoozed Mac does no work. Excepted: the step between Engage
  // recording the snooze and pausing, and the step between Wake resuming and clearing it (the
  // owner asked to end it).
  val snoozedMeansPaused = (active and pc.get("E") != "e3" and pc.get("F") != "e3" and not(fixed and pc.get("W") == "w2")) implies bit
  val clientsDone = pc.keys().exclude(Set("X")).forall(p => Set("done", "refused").contains(pc.get(p)))

  temporal fairness = weakFair(client, (bit, reasons, expired, pc, eb, hb, xe, xb, fails))
                  and weakFair(expirer, (bit, reasons, expired, pc, eb, hb, xe, xb, fails))
                  and weakFair(lapse, (bit, reasons, expired, pc, eb, hb, xe, xb, fails))
  // Liveness: once every request is handled, the bit settles to match the reasons.
  temporal eventuallyConsistent = fairness implies eventually(always(clientsDone implies pausedIffReason))
  // L1: an operator resume that went through (not refused while snoozed), with nobody pausing the
  // worker again, leaves it working once any snooze lapses.
  temporal eventuallyWorkingAfterResume = fairness implies eventually(always(pc.get("R") == "done" implies not(bit)))
}

// Every writer at once, starting from an operator pause, and one failed Orchard resume:
// checks S1 and L2.
module snoozeAll {
  import snooze(fixed = true, resumeCanFail = true, withPause = true, withHealth = true,
                withResume = true, withEngage = true, withWake = true, initOperatorPaused = true, atomicTime = false, withSecondEngage = false).*
}

// An operator resumes a paused worker while its owner snoozes it: checks L1.
module snoozeResume {
  import snooze(fixed = true, resumeCanFail = false, withPause = false, withHealth = false,
                withResume = true, withEngage = true, withWake = false, initOperatorPaused = true, atomicTime = false, withSecondEngage = false).*
}

// The same scenarios against the code before spec/ (expected to FAIL; CI checks they still do,
// so the models keep finding the bugs they were written for).
module snoozeAllBefore {
  import snooze(fixed = false, resumeCanFail = true, withPause = true, withHealth = true,
                withResume = true, withEngage = true, withWake = true, initOperatorPaused = true, atomicTime = false, withSecondEngage = false).*
}
module snoozeResumeBefore {
  import snooze(fixed = false, resumeCanFail = false, withPause = false, withHealth = false,
                withResume = true, withEngage = true, withWake = false, initOperatorPaused = true, atomicTime = false, withSecondEngage = false).*
}
// Only a snooze and its expiry, with one failed resume, in the old clear-then-resume order: L2 fails.
module snoozeExpiryFailsBefore {
  import snooze(fixed = false, resumeCanFail = true, withPause = false, withHealth = false,
                withResume = false, withEngage = true, withWake = false, initOperatorPaused = false, atomicTime = false, withSecondEngage = false).*
}

// Traces for the Go replay test: every writer, one failed resume, time moving between requests.
// spec/check.sh samples it with `quint run --out-itf` and replays each trace against the real code.
module snoozeMBT {
  import snooze(fixed = true, resumeCanFail = true, withPause = true, withHealth = true,
                withResume = true, withEngage = true, withWake = true, initOperatorPaused = true,
                atomicTime = true, withSecondEngage = true).*
}
// Same, starting with nothing paused.
module snoozeMBTFresh {
  import snooze(fixed = true, resumeCanFail = true, withPause = true, withHealth = true,
                withResume = true, withEngage = true, withWake = true, initOperatorPaused = false,
                atomicTime = true, withSecondEngage = true).*
}
