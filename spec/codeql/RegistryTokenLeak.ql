/**
 * @name S10e: the registry pull login reaches anything but a VM's Orchard exec stdin
 * @description The login from registryauth (the Docker config.json Enrol writes into a Linux VM, and
 *              the credential Resolver presents to a registry) must never reach argv, a log, an API
 *              response, a VM spec, the job record or Nomad dispatch meta or payload
 *              (CLAUDE.md, "internal/registryauth").
 * @kind problem
 * @problem.severity error
 * @id grove/registry-token-leak
 */

import go
import GroveSecrets

module Cfg implements DataFlow::ConfigSig {
  predicate isSource(DataFlow::Node n) {
    exists(DataFlow::MethodCallNode c |
      c.getTarget().getName() = ["PullConfig", "CredentialFor"] and
      c.getTarget().getPackage().getPath() =
        ["github.com/gm2211/grove/internal/registryauth", "github.com/gm2211/grove/internal/fleet"] and
      n = c.getResult(0)
    )
  }

  predicate isSink(DataFlow::Node n) {
    GroveSecrets::argv(n) or GroveSecrets::logged(n) or GroveSecrets::responded(n) or
    GroveSecrets::vmSpec(n) or GroveSecrets::jobRecord(n) or GroveSecrets::dispatchArg(n, [2, 3])
  }

  predicate isBarrier(DataFlow::Node n) { GroveSecrets::fingerprinted(n) }

  // A secret inside a slice, map or struct at a sink counts: argv is a []string.
  predicate allowImplicitRead(DataFlow::Node n, DataFlow::ContentSet c) { isSink(n) and exists(c) }
}

module Flow = TaintTracking::Global<Cfg>;

from DataFlow::Node src, DataFlow::Node sink
where Flow::flow(src, sink)
select sink, "The registry login from $@ reaches this sink.", src, "its source"
