/**
 * @name S10d: a VM's Nomad identity reaches argv, a VM spec, a log or an API response
 * @description A node key or an introduction token goes to its VM over Orchard exec STDIN only
 *              (CLAUDE.md, "Nomad mTLS and client introduction").
 * @kind problem
 * @problem.severity error
 * @id grove/node-secret-leak
 */

import go
import GroveSecrets

module Cfg implements DataFlow::ConfigSig {
  predicate isSource(DataFlow::Node n) {
    exists(Field f |
      f.hasQualifiedName("github.com/gm2211/grove/internal/pki", "NodeCert", "Key") and
      n = f.getARead()
    )
    or
    exists(DataFlow::MethodCallNode c |
      c.getTarget().getName() = "CreateIntroToken" and
      n = c.getResult(0)
    )
  }

  predicate isSink(DataFlow::Node n) {
    GroveSecrets::argv(n) or GroveSecrets::logged(n) or GroveSecrets::responded(n) or
    GroveSecrets::vmSpec(n)
  }

  predicate isBarrier(DataFlow::Node n) { GroveSecrets::fingerprinted(n) }

  // A secret inside a slice, map or struct at a sink counts: argv is a []string.
  predicate allowImplicitRead(DataFlow::Node n, DataFlow::ContentSet c) { isSink(n) and exists(c) }
}

module Flow = TaintTracking::Global<Cfg>;

from DataFlow::Node src, DataFlow::Node sink
where Flow::flow(src, sink)
select sink, "A node secret from $@ reaches this sink.", src, "its source"
