import Grove.Str

/-!
# The credential broker swaps placeholders only where it should (S8 in spec/README.md)

A transcription of `kindHosts`, `hostAllowed`, `Store.Lookup` (internal/broker/store.go) and
`inject` (internal/broker/inject.go). An entry's `source` is its VM's address on the worker's
bridge, which the worker's broker sets when it installs the entry (`Syncer.bind`,
internal/broker/sync.go); `none` is an entry nobody bound. A header value is a list of pieces: plain text, or a
`gph1_` placeholder (`placeholderRE`); HTTP Basic decoding only changes how the pieces are
spelled, so it is left out. Header names are compared as Go's `http.Header` canonicalizes them.

Proved: a real token is only ever written into `Authorization` or `X-Api-Key`, only for a host
its kind allows, only from a live entry, and only from the entry's own source address. An entry
with no source is refused from every address (`unbound_refused`); it used to be honoured from any,
which was the open half of S8.

Not modelled: that the address the broker binds is the VM's. That is `Syncer.bind` and
`TartSources` (internal/broker/source.go) asking the worker's own Tart, covered by Go tests
only, and it rests on vmnet not letting one VM send from another's address.
-/

namespace Grove.Broker

open Grove

inductive Kind | github | anthropic | openai
  deriving DecidableEq, Repr

/-- `kindHosts`: fixed in code, never configured. -/
def kindHosts : Kind → List Str
  | .github => ["github.com".toList, "api.github.com".toList]
  | .anthropic => ["api.anthropic.com".toList]
  | .openai => ["api.openai.com".toList, "chatgpt.com".toList]

def hostAllowed (k : Kind) (host : Str) : Bool := (kindHosts k).contains host

abbrev Addr := Nat

structure Entry where
  placeholder : Str
  kind : Kind
  token : Str
  expires : Nat
  source : Option Addr
  deriving DecidableEq, Repr

inductive Err | unknown | wrongHost | wrongSource
  deriving DecidableEq, Repr

/-- `Store.Lookup` -/
def lookup (entries : List Entry) (now : Nat) (ph host : Str) (src : Addr) : Except Err Entry :=
  match entries.find? (·.placeholder == ph) with
  | none => .error .unknown
  | some e =>
    if ¬ now < e.expires then .error .unknown
    else if ¬ hostAllowed e.kind host then .error .wrongHost
    else match e.source with
      | some a => if a = src then .ok e else .error .wrongSource
      | none => .error .wrongSource

inductive Piece | text (s : Str) | ph (p : Str) | secret (t : Str)
  deriving DecidableEq, Repr

inductive HName | authorization | xApiKey | other (canonical : Str)
  deriving DecidableEq, Repr

structure Header where
  name : HName
  value : List Piece
  deriving DecidableEq, Repr

/-- `injectHeaders` -/
def injected : HName → Bool
  | .authorization | .xApiKey => true
  | .other _ => false

def swapValue (entries : List Entry) (now : Nat) (host : Str) (src : Addr) :
    List Piece → Except Err (List Piece)
  | [] => .ok []
  | .ph p :: rest => do
    let e ← lookup entries now p host src
    let r ← swapValue entries now host src rest
    pure (.secret e.token :: r)
  | x :: rest => do
    let r ← swapValue entries now host src rest
    pure (x :: r)

def headerValue (entries : List Entry) (now : Nat) (host : Str) (src : Addr) (h : Header) :
    Except Err (List Piece) :=
  if injected h.name then swapValue entries now host src h.value else .ok h.value

/-- `inject`: every placeholder in an injected header is swapped, and any one failing lookup
fails the whole request (nothing is sent). Other headers, the URL and the body pass unchanged. -/
def inject (entries : List Entry) (now : Nat) (host : Str) (src : Addr) :
    List Header → Except Err (List Header)
  | [] => .ok []
  | h :: rest => do
    let v ← headerValue entries now host src h
    let r ← inject entries now host src rest
    pure ({ h with value := v } :: r)

/-- The entry a real token in the output came from, and the lookup that let it through. -/
def Granted (entries : List Entry) (now : Nat) (host : Str) (src : Addr) (t : Str) : Prop :=
  ∃ p e, lookup entries now p host src = .ok e ∧ e.token = t

theorem lookup_ok {entries now ph host src e} (h : lookup entries now ph host src = .ok e) :
    e ∈ entries ∧ now < e.expires ∧ hostAllowed e.kind host = true ∧ (∀ a, e.source = some a → a = src) := by
  unfold lookup at h
  split at h
  · cases h
  · rename_i e' he
    have hmem := List.mem_of_find?_eq_some he
    split at h
    · cases h
    · split at h
      · cases h
      · rename_i hexp hhost
        split at h
        · rename_i a hs
          split at h
          · cases h; exact ⟨hmem, by omega, by simpa using hhost, fun b hb => by simp_all⟩
          · cases h
        · cases h

/-- A lookup that succeeds found an entry bound to the address the request came from. -/
theorem lookup_bound {entries now ph host src e} (h : lookup entries now ph host src = .ok e) :
    e.source = some src := by
  unfold lookup at h
  split at h
  · cases h
  · split at h
    · cases h
    · split at h
      · cases h
      · split at h
        · split at h
          · cases h; simp_all
          · cases h
        · cases h

theorem swapValue_secrets {entries now host src v v'}
    (h : swapValue entries now host src v = .ok v') :
    ∀ t, .secret t ∈ v' → .secret t ∈ v ∨ Granted entries now host src t := by
  induction v generalizing v' with
  | nil => simp [swapValue] at h; subst h; simp
  | cons x rest ih =>
    cases x with
    | ph p =>
      simp only [swapValue, bind, Except.bind] at h
      split at h
      · cases h
      · rename_i e he
        split at h
        · cases h
        · rename_i r hr
          cases h
          intro t ht
          simp at ht
          rcases ht with rfl | ht
          · exact Or.inr ⟨p, e, he, rfl⟩
          · rcases ih hr t ht with h' | h'
            · exact Or.inl (by simp [h'])
            · exact Or.inr h'
    | text s =>
      simp only [swapValue, bind, Except.bind] at h
      split at h
      · cases h
      · rename_i r hr
        cases h
        intro t ht
        simp at ht
        rcases ih hr t ht with h' | h'
        · exact Or.inl (by simp [h'])
        · exact Or.inr h'
    | secret s =>
      simp only [swapValue, bind, Except.bind] at h
      split at h
      · cases h
      · rename_i r hr
        cases h
        intro t ht
        simp at ht
        rcases ht with rfl | ht
        · exact Or.inl (by simp)
        · rcases ih hr t ht with h' | h'
          · exact Or.inl (by simp [h'])
          · exact Or.inr h'

/-- S8: whatever a job sends, the request that leaves the broker carries a real token only (a) in
`Authorization` or `X-Api-Key`, (b) from an entry whose kind may go to this host, that is live,
and is bound to this source address, or (c) where the job already had it. -/
theorem S8_inject_sound {entries now host src hs hs'}
    (h : inject entries now host src hs = .ok hs') :
    ∀ h' ∈ hs', ∀ t, .secret t ∈ h'.value →
      (∃ h0 ∈ hs, h0.name = h'.name ∧ .secret t ∈ h0.value) ∨
      (injected h'.name = true ∧ ∃ e ∈ entries, e.token = t ∧ now < e.expires ∧
        hostAllowed e.kind host = true ∧ e.source = some src) := by
  induction hs generalizing hs' with
  | nil => simp [inject] at h; subst h; simp
  | cons x rest ih =>
    simp only [inject, bind, Except.bind] at h
    cases hv : headerValue entries now host src x with
    | error e => rw [hv] at h; cases h
    | ok v =>
      rw [hv] at h
      dsimp only at h
      cases hr : inject entries now host src rest with
      | error e => rw [hr] at h; cases h
      | ok r =>
        rw [hr] at h
        cases h
        intro h' hmem t ht
        simp at hmem
        rcases hmem with rfl | hmem
        · by_cases hinj : injected x.name = true
          · unfold headerValue at hv; rw [if_pos hinj] at hv
            rcases swapValue_secrets hv t ht with h0 | ⟨p, e, he, rfl⟩
            · exact Or.inl ⟨x, by simp, rfl, h0⟩
            · obtain ⟨hm, hx, hh, _⟩ := lookup_ok he
              exact Or.inr ⟨hinj, e, hm, rfl, hx, hh, lookup_bound he⟩
          · unfold headerValue at hv; rw [if_neg hinj] at hv
            cases hv
            exact Or.inl ⟨x, by simp, rfl, ht⟩
        · rcases ih hr h' hmem t ht with ⟨h0, hm, hn, hv0⟩ | h2
          · exact Or.inl ⟨h0, by simp [hm], hn, hv0⟩
          · exact Or.inr h2

/-- S8: a header the broker does not inject into leaves exactly as the job wrote it. -/
theorem S8_other_headers_untouched {entries now host src hs hs'}
    (h : inject entries now host src hs = .ok hs') :
    List.map Header.name hs' = List.map Header.name hs ∧
      ∀ i (hi : i < hs.length) (hi' : i < hs'.length), injected hs[i].name = false → hs'[i] = hs[i] := by
  induction hs generalizing hs' with
  | nil => simp [inject] at h; subst h; simp
  | cons x rest ih =>
    simp only [inject, bind, Except.bind] at h
    cases hv : headerValue entries now host src x with
    | error e => rw [hv] at h; cases h
    | ok v =>
      rw [hv] at h
      dsimp only at h
      cases hr : inject entries now host src rest with
      | error e => rw [hr] at h; cases h
      | ok r =>
        rw [hr] at h
        cases h
        obtain ⟨hnames, hsame⟩ := ih hr
        refine ⟨by simp [hnames], ?_⟩
        intro i hi hi' hinj
        cases i with
        | zero =>
          simp at hinj ⊢
          unfold headerValue at hv
          rw [if_neg (by simp [hinj])] at hv
          cases hv; rfl
        | succ j =>
          simp at hi hi' hinj ⊢
          exact hsame j hi hi' hinj

/-- The old finding, closed: an entry with no source was honoured from any address, and is now
refused from every one. -/
theorem unbound_refused :
    let e : Entry := ⟨"gph1_x".toList, .github, "ghp_real".toList, 100, none⟩
    lookup [e] 0 "gph1_x".toList "api.github.com".toList 7 = .error .wrongSource ∧ lookup [e] 0 "gph1_x".toList "api.github.com".toList 8 = .error .wrongSource := by
  intro e; exact ⟨rfl, rfl⟩

/-- With a source, the same placeholder from another address is refused. -/
theorem source_bound :
    let e : Entry := ⟨"gph1_x".toList, .github, "ghp_real".toList, 100, some 7⟩
    lookup [e] 0 "gph1_x".toList "api.github.com".toList 7 = .ok e ∧ lookup [e] 0 "gph1_x".toList "api.github.com".toList 8 = .error .wrongSource := by
  intro e; exact ⟨rfl, rfl⟩

/-- And a host outside the kind's list is refused. -/
theorem wrong_host_refused :
    let e : Entry := ⟨"gph1_x".toList, .github, "ghp_real".toList, 100, none⟩
    lookup [e] 0 "gph1_x".toList "evil.example".toList 7 = .error .wrongHost := by
  intro e; rfl

end Grove.Broker
