import Grove.Str

/-!
# Artifact paths stay inside the job's directory (S9 in spec/README.md)

`safeArtifactPath` (internal/server/handlers_jobs.go) guards `GET /jobs/{id}/artifacts/{path...}`:
the path is joined under the job's artifact prefix. Resolving a path the way a filesystem or
`path.Clean` does (`.` stays, `..` pops) never leaves that prefix for any path the guard accepts,
and the guard is not vacuous: `a/../../x` would leave it, and is refused.
-/

namespace Grove.ArtifactPath

open Grove

/-- `strings.Split(p, "/")` -/
def splitSlash : Str → List Str
  | [] => [[]]
  | c :: s =>
    match splitSlash s with
    | [] => [[c]]  -- unreachable: splitSlash never returns []
    | seg :: rest => if c = '/' then [] :: seg :: rest else (c :: seg) :: rest

def badSegment (seg : Str) : Bool := seg.isEmpty || seg == ".".toList || seg == "..".toList

/-- `safeArtifactPath` -/
def safe (p : Str) : Bool :=
  !p.isEmpty && !startsWith p "/".toList && !p.contains '\\' && !(splitSlash p).any badSegment

/-- Resolve segments under a directory: `..` pops (never above the filesystem root), `.` and
empty segments stay put, anything else descends. -/
def resolve : List Str → List Str → List Str
  | dir, [] => dir
  | dir, seg :: rest =>
    if seg == "..".toList then resolve dir.dropLast rest
    else if seg.isEmpty || seg == ".".toList then resolve dir rest
    else resolve (dir ++ [seg]) rest

theorem resolve_plain (dir segs : List Str) (h : segs.all (fun s => !badSegment s) = true) :
    resolve dir segs = dir ++ segs := by
  induction segs generalizing dir with
  | nil => simp [resolve]
  | cons seg rest ih =>
    simp only [List.all_cons, Bool.and_eq_true] at h
    have hs := h.1
    simp only [badSegment, Bool.not_or, Bool.and_eq_true, Bool.not_eq_true'] at hs
    simp only [resolve]
    have h1 : (seg == "..".toList) = false := hs.2
    have h2 : (seg.isEmpty || seg == ".".toList) = false := by rw [hs.1.1, hs.1.2]; rfl
    rw [h1, h2]
    simp [ih (dir ++ [seg]) h.2]

/-- S9: every accepted path resolves strictly inside the job's artifact directory. -/
theorem S9_contained (dir : List Str) (p : Str) (h : safe p = true) :
    ∃ rest, resolve dir (splitSlash p) = dir ++ rest ∧ rest ≠ [] := by
  simp only [safe, Bool.and_eq_true, Bool.not_eq_true'] at h
  have hall : (splitSlash p).all (fun s => !badSegment s) = true := by
    have := h.2
    simpa [List.any_eq_false, List.all_eq_true] using this
  refine ⟨splitSlash p, resolve_plain dir _ hall, ?_⟩
  cases p with
  | nil => simp at h
  | cons c s =>
    simp only [splitSlash]
    split
    · simp
    · split <;> simp

/-- The guard is needed: without it, `a/../../x` climbs out of the directory. -/
theorem escape_without_guard :
    resolve ["jobs".toList, "j1".toList] (splitSlash "a/../../x".toList) = ["jobs".toList, "x".toList] ∧
      safe "a/../../x".toList = false := by
  decide

end Grove.ArtifactPath
