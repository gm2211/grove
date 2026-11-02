import Grove

/-! `grove-spec`: the Lean transcriptions as a filter, for internal/conformance's differential
tests. Each input line is `<command> <args...>` (tab-separated); each output line is the answer.

  scope <METHOD> <path>     → the scope requiredScope demands (Scope.encode)
  artifact <path>           → "ok" or "refused" (safeArtifactPath)
-/

open Grove

def methodOf : String → Auth.Method
  | "GET" => .GET | "POST" => .POST | "PUT" => .PUT | "DELETE" => .DELETE | _ => .other

def answer (line : String) : String :=
  match line.splitOn "\t" with
  | ["scope", m, p] => String.ofList (Auth.requiredScope (methodOf m) p.toList).encode
  | ["artifact", p] => if ArtifactPath.safe p.toList then "ok" else "refused"
  | _ => "error"

partial def loop (stdin : IO.FS.Stream) (stdout : IO.FS.Stream) : IO Unit := do
  let line ← stdin.getLine
  if line.isEmpty then return
  stdout.putStrLn (answer (String.ofList (line.toList.filter (· != (Char.ofNat 10)))))
  loop stdin stdout

def main : IO Unit := do
  loop (← IO.getStdin) (← IO.getStdout)
