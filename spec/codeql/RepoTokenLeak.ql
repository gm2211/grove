/**
 * @name S10b: the private-repo token reaches anything but the dispatch payload
 * @description The token from gitauth may go into the build/agent dispatch PAYLOAD only: never
 *              dispatch meta (env_json included), argv, a log, an API response or the job record.
 * @kind problem
 * @problem.severity error
 * @id grove/repo-token-leak
 */

import go
import GroveSecrets

module Cfg implements DataFlow::ConfigSig {
  predicate isSource(DataFlow::Node n) {
    exists(DataFlow::MethodCallNode c |
      c.getTarget().getName() = "TokenFor" and
      c.getTarget().getPackage().getPath() =
        ["github.com/gm2211/grove/internal/gitauth", "github.com/gm2211/grove/internal/dispatch"] and
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
select sink, "The private-repo token from $@ reaches this sink.", src, "TokenFor"
