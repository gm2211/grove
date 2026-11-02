/**
 * Grove's secret-handling rules (CLAUDE.md: internal/gitauth, internal/secrets, internal/broker,
 * Nomad mTLS and client introduction) as CodeQL sinks, shared by the S10 queries.
 */

import go

module GroveSecrets {
  string dispatchPkg() { result = "github.com/gm2211/grove/internal/dispatch" }

  /** A command line: os/exec argv, or the argv of an Orchard exec into a VM. */
  predicate argv(DataFlow::Node n) {
    exists(DataFlow::CallNode c |
      c.getTarget().hasQualifiedName("os/exec", ["Command", "CommandContext"]) and
      n = c.getAnArgument()
    )
    or
    exists(DataFlow::MethodCallNode c |
      c.getTarget().getName() = "Exec" and
      c.getTarget().getPackage().getPath() = "github.com/gm2211/grove/internal/orchard" and
      n = c.getArgument(2)
    )
  }

  /** Anything logged: log, log/slog and fmt printing. */
  predicate logged(DataFlow::Node n) {
    n = any(LoggerCall lc).getAMessageComponent()
    or
    exists(DataFlow::CallNode c |
      c.getTarget().getPackage().getPath() = "log/slog" and
      n = c.getAnArgument()
    )
    or
    exists(DataFlow::CallNode c |
      c.getTarget().hasQualifiedName("fmt", ["Print", "Printf", "Println"]) and
      n = c.getAnArgument()
    )
  }

  /** Anything written into an API response. */
  predicate responded(DataFlow::Node n) {
    n = any(Http::ResponseBody rb)
    or
    exists(DataFlow::MethodCallNode c |
      c.getTarget().getName() = "writeJSON" and
      c.getTarget().getPackage().getPath() = "github.com/gm2211/grove/internal/server" and
      n = c.getAnArgument()
    )
  }

  /** The stored job request and its env: kept in the job history and returned by the API. */
  predicate jobRecord(DataFlow::Node n) {
    exists(Write w, Field f |
      w.writesField(_, f, n) and
      (
        f.hasQualifiedName(dispatchPkg(), "JobRequest", "Env") or
        f.hasQualifiedName(dispatchPkg(), "Job", "Request")
      )
    )
  }

  /** Nomad dispatch meta (every task of the job sees it) and payload (the source task only). */
  predicate dispatchArg(DataFlow::Node n, int i) {
    exists(DataFlow::MethodCallNode c |
      c.getTarget().getName() = "Dispatch" and
      c.getTarget().getPackage().getPath() = "github.com/gm2211/grove/internal/nomad" and
      n = c.getArgument(i)
    )
  }

  /** A VM spec: readable by every holder of an Orchard credential. */
  predicate vmSpec(DataFlow::Node n) {
    exists(Write w, Field f |
      w.writesField(_, f, n) and
      f.getDeclaringType().(StructType) = any(DefinedType t |
          t.hasQualifiedName("github.com/gm2211/grove/internal/orchard", "VMSpec")
        ).getUnderlyingType() and
      not f.getName() = "Password"
    )
  }

  /** Hashing a value for display is not leaking it. */
  predicate fingerprinted(DataFlow::Node n) {
    exists(DataFlow::CallNode c |
      c.getTarget().getName() = ["fingerprint", "Fingerprint"] or
      c.getTarget().hasQualifiedName("crypto/sha256", ["Sum256", "New"])
    |
      n = c.getResult() or n = c.getResult(_)
    )
  }
}
