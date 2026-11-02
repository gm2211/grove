/**
 * @name S10c: a job's broker credential leaves the registry
 * @description A JobRequest.Credentials token goes only into the broker registry: never dispatch
 *              meta or payload, argv, a log or an API response (internal/broker, CLAUDE.md).
 * @kind problem
 * @problem.severity error
 * @id grove/broker-credential-leak
 */

import go
import GroveSecrets

module Cfg implements DataFlow::ConfigSig {
  predicate isSource(DataFlow::Node n) {
    exists(Field f |
      f.hasQualifiedName(GroveSecrets::dispatchPkg(), "Credential", "Token") and
      n = f.getARead()
    )
  }

  predicate isSink(DataFlow::Node n) {
    GroveSecrets::argv(n) or GroveSecrets::logged(n) or GroveSecrets::responded(n) or
    GroveSecrets::dispatchArg(n, [2, 3])
  }

  predicate isBarrier(DataFlow::Node n) { GroveSecrets::fingerprinted(n) }

  // A secret inside a slice, map or struct at a sink counts: argv is a []string.
  predicate allowImplicitRead(DataFlow::Node n, DataFlow::ContentSet c) { isSink(n) and exists(c) }
}

module Flow = TaintTracking::Global<Cfg>;

from DataFlow::Node src, DataFlow::Node sink
where Flow::flow(src, sink)
select sink, "A broker credential from $@ reaches this sink.", src, "Credential.Token"
