/**
 * @name S10 coverage: each secret DOES reach the one place it is allowed to go
 * @description Non-vacuity check for the S10 queries: with the same sources and the same taint
 *              tracking, each secret must be found at its allowed destination. A row missing here
 *              means the leak query above it would be vacuously empty. CI expects five rows.
 * @kind problem
 * @problem.severity recommendation
 * @id grove/secret-coverage
 */

import go
import GroveSecrets

predicate jobSecret(DataFlow::Node n) {
  exists(DataFlow::MethodCallNode c |
    c.getTarget().getName() = "Resolve" and
    c.getTarget().getPackage().getPath() =
      ["github.com/gm2211/grove/internal/secrets", "github.com/gm2211/grove/internal/dispatch"] and
    n = c.getResult(0)
  )
}

predicate repoToken(DataFlow::Node n) {
  exists(DataFlow::MethodCallNode c |
    c.getTarget().getName() = "TokenFor" and
    c.getTarget().getPackage().getPath() =
      ["github.com/gm2211/grove/internal/gitauth", "github.com/gm2211/grove/internal/dispatch"] and
    n = c.getResult(0)
  )
}

predicate brokerCredential(DataFlow::Node n) {
  exists(Field f | f.hasQualifiedName(GroveSecrets::dispatchPkg(), "Credential", "Token") and n = f.getARead())
}

predicate nodeSecret(DataFlow::Node n) {
  exists(Field f | f.hasQualifiedName("github.com/gm2211/grove/internal/pki", "NodeCert", "Key") and n = f.getARead())
  or
  exists(DataFlow::MethodCallNode c | c.getTarget().getName() = "CreateIntroToken" and n = c.getResult(0))
}

predicate registryLogin(DataFlow::Node n) {
  exists(DataFlow::MethodCallNode c |
    c.getTarget().getName() = "PullConfig" and
    c.getTarget().getPackage().getPath() =
      ["github.com/gm2211/grove/internal/registryauth", "github.com/gm2211/grove/internal/fleet"] and
    n = c.getResult(0)
  )
}

/** The allowed destination of each kind of secret. */
predicate allowed(string kind, DataFlow::Node n) {
  kind = "job secret -> dispatch payload" and GroveSecrets::dispatchArg(n, 3)
  or
  kind = "repo token -> dispatch payload" and GroveSecrets::dispatchArg(n, 3)
  or
  kind = "broker credential -> broker.Entry.Token" and
  exists(Write w, Field f |
    w.writesField(_, f, n) and f.hasQualifiedName("github.com/gm2211/grove/internal/broker", "Entry", "Token")
  )
  or
  kind = ["node secret -> Orchard exec stdin", "registry login -> Orchard exec stdin"] and
  exists(DataFlow::MethodCallNode c |
    c.getTarget().getName() = "Exec" and
    c.getTarget().getPackage().getPath() = "github.com/gm2211/grove/internal/orchard" and
    n = c.getArgument(3)
  )
}

module Cfg implements DataFlow::StateConfigSig {
  class FlowState = string;

  predicate isSource(DataFlow::Node n, FlowState s) {
    jobSecret(n) and s = "job secret -> dispatch payload"
    or
    repoToken(n) and s = "repo token -> dispatch payload"
    or
    brokerCredential(n) and s = "broker credential -> broker.Entry.Token"
    or
    nodeSecret(n) and s = "node secret -> Orchard exec stdin"
    or
    registryLogin(n) and s = "registry login -> Orchard exec stdin"
  }

  predicate isSink(DataFlow::Node n, FlowState s) { allowed(s, n) }

  predicate allowImplicitRead(DataFlow::Node n, DataFlow::ContentSet c) { allowed(_, n) and exists(c) }
}

module Flow = TaintTracking::GlobalWithState<Cfg>;

from string kind
where exists(DataFlow::Node sink | Flow::flowTo(sink) and allowed(kind, sink) and
  exists(DataFlow::Node src | Flow::flow(src, sink)))
select kind, kind
