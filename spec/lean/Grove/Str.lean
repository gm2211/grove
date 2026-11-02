/-!
# Strings as Go sees them

Go's `strings.HasPrefix`, `CutPrefix`, `Cut` and `HasSuffix`, over lists of characters. On valid
UTF-8 a byte prefix is a character prefix, so these agree with Go for every path an HTTP request
can carry once `net/url` has decoded it; `internal/conformance` checks the two against each other
on generated paths.
-/

namespace Grove

abbrev Str := List Char

/-- `strings.HasPrefix s pre` -/
def startsWith : Str → Str → Bool
  | _, [] => true
  | [], _ :: _ => false
  | c :: s, d :: p => c == d && startsWith s p

/-- `strings.HasSuffix s suf` -/
def endsWith (s suf : Str) : Bool := suf.isSuffixOf s

@[simp] theorem endsWith_iff (s suf : Str) : endsWith s suf = true ↔ suf <:+ s := List.isSuffixOf_iff_suffix

/-- `strings.CutPrefix s pre` -/
def cutPrefix : Str → Str → Option Str
  | s, [] => some s
  | [], _ :: _ => none
  | c :: s, d :: p => if c = d then cutPrefix s p else none

/-- `strings.Cut s "/"`: the part before the first slash and the part after it. -/
def cutSlash : Str → Option (Str × Str)
  | [] => none
  | c :: s => if c = '/' then some ([], s) else (cutSlash s).map fun (a, b) => (c :: a, b)

theorem startsWith_append (pre s : Str) : startsWith (pre ++ s) pre = true := by
  induction pre with
  | nil => cases s <;> rfl
  | cons c p ih => simp [startsWith, ih]

theorem startsWith_some {s pre : Str} (h : startsWith s pre = true) : ∃ r, s = pre ++ r := by
  induction pre generalizing s with
  | nil => exact ⟨s, rfl⟩
  | cons d p ih =>
    cases s with
    | nil => simp [startsWith] at h
    | cons c s =>
      simp [startsWith] at h
      obtain ⟨rfl, h⟩ := h
      obtain ⟨r, rfl⟩ := ih h
      exact ⟨r, rfl⟩

theorem cutPrefix_append (pre s : Str) : cutPrefix (pre ++ s) pre = some s := by
  induction pre with
  | nil => cases s <;> rfl
  | cons c p ih => simp [cutPrefix, ih]

theorem cutPrefix_some {s pre r : Str} (h : cutPrefix s pre = some r) : s = pre ++ r := by
  induction pre generalizing s with
  | nil => cases s <;> simp_all [cutPrefix]
  | cons d p ih =>
    cases s with
    | nil => simp [cutPrefix] at h
    | cons c s =>
      simp only [cutPrefix] at h
      split at h
      · subst_vars; simp [ih h]
      · simp at h

theorem cutSlash_noSlash (a b : Str) (h : '/' ∉ a) : cutSlash (a ++ '/' :: b) = some (a, b) := by
  induction a with
  | nil => simp [cutSlash]
  | cons c a ih =>
    simp at h
    simp [cutSlash, Ne.symm h.1, ih h.2]

theorem cutSlash_some {s a b : Str} (h : cutSlash s = some (a, b)) : s = a ++ '/' :: b ∧ '/' ∉ a := by
  induction s generalizing a b with
  | nil => simp [cutSlash] at h
  | cons c s ih =>
    simp only [cutSlash] at h
    split at h
    · simp at h; obtain ⟨rfl, rfl⟩ := h; subst_vars; simp
    · rename_i hc
      cases hs : cutSlash s with
      | none => simp [hs] at h
      | some ab =>
        obtain ⟨a', b'⟩ := ab
        simp [hs] at h
        obtain ⟨rfl, rfl⟩ := h
        obtain ⟨h1, h2⟩ := ih hs
        subst h1
        simp [Ne.symm hc, h2]

end Grove
