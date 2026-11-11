/**
 * @name S10a: a named job secret reaches anything but the dispatch payload
 * @description A value from secrets.Store.Resolve may reach a job only through its dispatch's
 *              PAYLOAD: never dispatch meta (env_json included), argv, a log, an API response or
 *              the job record (CLAUDE.md, internal/secrets).
 * @kind problem
 * @problem.severity error
 * @id grove/job-secret-leak
 */

import go
import GroveSecrets

module Cfg implements DataFlow::ConfigSig {
  predicate isSource(DataFlow::Node n) {
    exists(DataFlow::MethodCallNode c |
      c.getTarget().getName() = "Resolve" and
    c.getTarget().getPackage().getPath() =
      ["github.com/gm2211/grove/internal/secrets", "github.com/gm2211/grove/internal/dispatch"] and
      n = c.getResult(0)
    )
  }

  predicate isSink(DataFlow::Node n) {
    GroveSecrets::argv(n) or GroveSecrets::logged(n) or GroveSecrets::responded(n) or
    GroveSecrets::jobRecord(n) or GroveSecrets::dispatchArg(n, 2)
  }

  predicate isBarrier(DataFlow::Node n) { GroveSecrets::fingerprinted(n) }

  // A secret inside a slice, map or struct at a sink counts: argv is a []string.
  predicate allowImplicitRead(DataFlow::Node n, DataFlow::ContentSet c) { isSink(n) and exists(c) }
}

module Flow = TaintTracking::Global<Cfg>;

from DataFlow::Node src, DataFlow::Node sink
where Flow::flow(src, sink)
select sink, "A job secret from $@ reaches this sink.", src, "Resolve"
