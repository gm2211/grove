import Grove.Str

/-!
# Who may call which route (S5, S6, S7 in spec/README.md)

A transcription of `requiredScope`, `snoozeRouteWorker`, `validWorkerName` and
`principalHasScope` (internal/server/auth.go, access_store.go) and of the route table in
internal/server/server.go. `withAuth` lets a request through iff
`principalHasScope principal (requiredScope r)`, so these theorems are about every path a
request can name, with route parameters as arbitrary strings.

Go's ServeMux cleans a path before routing (`/a/../b` is redirected, never served), and
`withAuth` sits behind it, so `requiredScope` only ever sees the path of a route it serves.
`internal/conformance` checks that, and checks this transcription against the Go function.
-/

namespace Grove.Auth

open Grove

inductive Method | GET | POST | PUT | DELETE | other
  deriving DecidableEq, Repr

/-- The scope strings of internal/server: `read`, `dispatch`, ..., `worker:snooze:<w>`. -/
inductive Scope
  | read | dispatch | build | agent | shell | operator | broker
  | snooze (worker : Str)
  deriving DecidableEq, Repr

/-- The string Go compares (`ScopeSnoozePrefix + worker` for a snooze scope). -/
def Scope.encode : Scope → Str
  | .read => "read".toList
  | .dispatch => "dispatch".toList
  | .build => "dispatch:build".toList
  | .agent => "dispatch:agent".toList
  | .shell => "dispatch:shell".toList
  | .operator => "operator".toList
  | .broker => "broker".toList
  | .snooze w => "worker:snooze:".toList ++ w

/-- Go compares scopes as strings; that is the same as comparing these values. -/
theorem Scope.encode_injective (a b : Scope) (h : a.encode = b.encode) : a = b := by
  cases a <;> cases b <;> simp_all [Scope.encode] <;> exact List.append_cancel_left h

def workerChar (c : Char) : Bool :=
  ('a' ≤ c && c ≤ 'z') || ('A' ≤ c && c ≤ 'Z') || ('0' ≤ c && c ≤ '9') || c == '-' || c == '.' || c == '_'

/-- `validWorkerName` -/
def validWorkerName (w : Str) : Bool := !w.isEmpty && w.length ≤ 128 && w.all workerChar

theorem validWorkerName_noSlash {w : Str} (h : validWorkerName w = true) : '/' ∉ w := by
  intro hw
  simp only [validWorkerName, Bool.and_eq_true, List.all_eq_true] at h
  have := h.2 '/' hw
  simp [workerChar] at this

/-- `snoozeRouteWorker`: the `<name>` of `/workers/<name>/snooze` or `/workers/<name>/wake`. -/
def snoozeRouteWorker (p : Str) : Option Str :=
  (cutPrefix p "/workers/".toList).bind fun rest =>
    (cutSlash rest).bind fun (name, action) =>
      if action = "snooze".toList ∨ action = "wake".toList then some name else none

def operatorOnly (p : Str) : Bool :=
  p == "/access".toList || startsWith p "/access/".toList || p == "/secrets".toList ||
  startsWith p "/secrets/".toList || startsWith p "/join/approve/".toList ||
  p == "/join/pending".toList || startsWith p "/vms/".toList || startsWith p "/workers/".toList ||
  startsWith p "/github/".toList

def dispatchRoute (m : Method) (p : Str) : Bool :=
  (m == .POST && p == "/jobs".toList) ||
  (m == .DELETE && startsWith p "/jobs/".toList) ||
  (m == .PUT && startsWith p "/jobs/".toList && endsWith p "/credentials".toList) ||
  (m == .POST && endsWith p "/cancel".toList)

def afterSnooze (m : Method) (p : Str) : Scope :=
  if operatorOnly p then .operator
  else if p = "/broker/entries".toList then .broker
  else if dispatchRoute m p then .dispatch
  else .read

/-- `requiredScope` -/
def requiredScope (m : Method) (p : Str) : Scope :=
  if p = "/whoami".toList then .read
  else match snoozeRouteWorker p with
    | some w => if validWorkerName w then .snooze w else afterSnooze m p
    | none => afterSnooze m p

/-- `principalHasScope` -/
def hasScope (ps : List Scope) (s : Scope) : Bool :=
  ps.any fun c => c == .operator || c == s ||
    (s == .dispatch && (c == .build || c == .agent || c == .shell))

/-- `principalCanDispatchKind` for a shell job: operator, or `dispatch:shell` exactly. -/
def canDispatchShell (ps : List Scope) : Bool := hasScope ps .operator || ps.contains .shell

/-! ## Facts about the pieces -/

theorem snoozeRouteWorker_some {p w : Str} (h : snoozeRouteWorker p = some w) :
    p = "/workers/".toList ++ w ++ "/snooze".toList ∨ p = "/workers/".toList ++ w ++ "/wake".toList := by
  unfold snoozeRouteWorker at h
  cases hc : cutPrefix p "/workers/".toList with
  | none => rw [hc] at h; cases h
  | some rest =>
    have hp := cutPrefix_some hc
    cases hs : cutSlash rest with
    | none => rw [hc, Option.bind_some, hs] at h; cases h
    | some na =>
      obtain ⟨name, action⟩ := na
      obtain ⟨hr, _⟩ := cutSlash_some hs
      rw [hc, Option.bind_some, hs, Option.bind_some] at h
      dsimp only at h
      by_cases hact : action = "snooze".toList ∨ action = "wake".toList
      · rw [if_pos hact] at h
        cases h
        subst hp hr
        rcases hact with rfl | rfl <;> simp
      · rw [if_neg hact] at h; cases h

theorem snoozeRouteWorker_route (w a : Str) (hw : '/' ∉ w) :
    snoozeRouteWorker ("/workers/".toList ++ w ++ '/' :: a) =
      if a = "snooze".toList ∨ a = "wake".toList then some w else none := by
  unfold snoozeRouteWorker
  rw [List.append_assoc, cutPrefix_append, Option.bind_some, cutSlash_noSlash _ _ hw, Option.bind_some]

theorem afterSnooze_cases (m : Method) (p : Str) :
    afterSnooze m p = .operator ∨ (afterSnooze m p = .broker ∧ p = "/broker/entries".toList) ∨
      afterSnooze m p = .dispatch ∨ afterSnooze m p = .read := by
  unfold afterSnooze
  by_cases h1 : operatorOnly p = true
  · left; rw [if_pos h1]
  · rw [if_neg h1]
    by_cases h2 : p = "/broker/entries".toList
    · right; left; rw [if_pos h2]; exact ⟨rfl, h2⟩
    · rw [if_neg h2]
      by_cases h3 : dispatchRoute m p = true
      · right; right; left; rw [if_pos h3]
      · right; right; right; rw [if_neg h3]

theorem afterSnooze_ne_snooze (m : Method) (p w : Str) : afterSnooze m p ≠ .snooze w := by
  rcases afterSnooze_cases m p with h | ⟨h, _⟩ | h | h <;> rw [h] <;> simp

theorem afterSnooze_broker {m : Method} {p : Str} (h : afterSnooze m p = .broker) :
    p = "/broker/entries".toList := by
  rcases afterSnooze_cases m p with h' | ⟨_, h'⟩ | h' | h' <;> simp_all

theorem hasScope_single (c s : Scope) :
    hasScope [c] s = true ↔ c = .operator ∨ c = s ∨ (s = .dispatch ∧ (c = .build ∨ c = .agent ∨ c = .shell)) := by
  simp [hasScope, or_assoc]

/-! ## S5: a broker credential reaches only `GET /broker/entries` -/

theorem requiredScope_broker (m : Method) (p : Str) :
    requiredScope m p = .broker ↔ p = "/broker/entries".toList := by
  constructor
  · intro h
    unfold requiredScope at h
    by_cases hw : p = "/whoami".toList
    · rw [if_pos hw] at h; cases h
    · rw [if_neg hw] at h
      cases hs : snoozeRouteWorker p with
      | none => rw [hs] at h; exact afterSnooze_broker h
      | some w =>
        rw [hs] at h
        by_cases hv : validWorkerName w = true
        · simp [hv] at h
        · simp [hv] at h; exact afterSnooze_broker h
  · rintro rfl
    cases m <;> rfl

theorem S5_broker_only_entries (m : Method) (p : Str) :
    hasScope [.broker] (requiredScope m p) = true ↔ p = "/broker/entries".toList := by
  rw [hasScope_single, ← requiredScope_broker m]
  constructor
  · rintro (h | h | ⟨_, h | h | h⟩) <;> first | cases h | exact h.symm
  · intro h; exact Or.inr (Or.inl h.symm)

/-! ## S6: `worker:snooze:<w>` reaches only `/workers/<w>/snooze` and `/workers/<w>/wake` -/

theorem requiredScope_snooze (m : Method) (p w : Str) :
    requiredScope m p = .snooze w ↔
      validWorkerName w = true ∧
        (p = "/workers/".toList ++ w ++ "/snooze".toList ∨ p = "/workers/".toList ++ w ++ "/wake".toList) := by
  constructor
  · intro h
    unfold requiredScope at h
    by_cases hw : p = "/whoami".toList
    · rw [if_pos hw] at h; cases h
    · rw [if_neg hw] at h
      cases hs : snoozeRouteWorker p with
      | none => rw [hs] at h; exact absurd h (afterSnooze_ne_snooze _ _ _)
      | some w' =>
        rw [hs] at h
        dsimp only at h
        by_cases hv : validWorkerName w' = true
        · rw [if_pos hv] at h
          cases h
          exact ⟨hv, snoozeRouteWorker_some hs⟩
        · rw [if_neg hv] at h; exact absurd h (afterSnooze_ne_snooze _ _ _)
  · rintro ⟨hv, hp⟩
    have hs := validWorkerName_noSlash hv
    have hroute : snoozeRouteWorker p = some w := by
      rcases hp with rfl | rfl
      · have := snoozeRouteWorker_route w "snooze".toList hs
        simpa using this
      · have := snoozeRouteWorker_route w "wake".toList hs
        simpa using this
    have hwho : p ≠ "/whoami".toList := by rcases hp with rfl | rfl <;> simp
    unfold requiredScope
    rw [if_neg hwho, hroute]
    simp [hv]

theorem S6_snooze_scope_only_its_worker (m : Method) (p w : Str) :
    hasScope [.snooze w] (requiredScope m p) = true ↔
      validWorkerName w = true ∧
        (p = "/workers/".toList ++ w ++ "/snooze".toList ∨ p = "/workers/".toList ++ w ++ "/wake".toList) := by
  rw [hasScope_single, ← requiredScope_snooze m]
  constructor
  · rintro (h | h | ⟨_, h | h | h⟩) <;> first | cases h | exact h.symm
  · intro h; exact Or.inr (Or.inl h.symm)

/-! ## S7: the route table

Every route `grove serve` registers, with its parameters as arbitrary single path segments
(`{name}`: no slash, as ServeMux matches it) and `{path...}` as anything at all. -/

/-- A `{name}` parameter: one path segment. -/
def Seg (x : Str) : Prop := '/' ∉ x

macro "route_scope" : tactic =>
  `(tactic| (simp [requiredScope, snoozeRouteWorker, cutPrefix, afterSnooze, operatorOnly,
              dispatchRoute, startsWith, List.suffix_cons_iff]))

/-- For the `/workers/<n>/<action>` routes that are not snooze or wake. -/
theorem workersRoute_none (n a : Str) (hn : Seg n) (ha : ¬(a = "snooze".toList ∨ a = "wake".toList)) :
    snoozeRouteWorker ("/workers/".toList ++ n ++ '/' :: a) = none := by
  rw [snoozeRouteWorker_route _ _ hn, if_neg ha]

-- read: what any credential may see
theorem route_get_fleet : requiredScope .GET "/fleet".toList = .read := by decide
theorem route_get_fleet_reconcile : requiredScope .GET "/fleet/reconcile".toList = .read := by decide
theorem route_get_jobs : requiredScope .GET "/jobs".toList = .read := by decide
theorem route_get_whoami : requiredScope .GET "/whoami".toList = .read := by decide

-- dispatch: submitting and cancelling work
theorem route_post_jobs : requiredScope .POST "/jobs".toList = .dispatch := by decide

theorem route_delete_job (id : Str) :
    requiredScope .DELETE ("/jobs/".toList ++ id) = .dispatch := by route_scope

theorem route_cancel_job (id : Str) :
    requiredScope .POST ("/jobs/".toList ++ id ++ "/cancel".toList) = .dispatch := by route_scope

theorem route_put_credentials (id : Str) :
    requiredScope .PUT ("/jobs/".toList ++ id ++ "/credentials".toList) = .dispatch := by route_scope

-- read: a job's own pages, whatever its id and artifact path
theorem route_get_job (id : Str) :
    requiredScope .GET ("/jobs/".toList ++ id) = .read := by route_scope

theorem route_get_logs (id : Str) :
    requiredScope .GET ("/jobs/".toList ++ id ++ "/logs".toList) = .read := by route_scope

theorem route_get_artifact (id path : Str) :
    requiredScope .GET ("/jobs/".toList ++ id ++ "/artifacts/".toList ++ path) = .read := by route_scope

-- operator: everything that changes the fleet, the access list, secrets or GitHub sourcing
theorem route_recycle_vm (n : Str) :
    requiredScope .POST ("/vms/".toList ++ n ++ "/recycle".toList) = .operator := by route_scope

theorem route_worker_pause (n : Str) (hn : Seg n) :
    requiredScope .POST ("/workers/".toList ++ n ++ "/pause".toList) = .operator := by
  have hr := workersRoute_none n "pause".toList hn (by decide)
  unfold requiredScope
  rw [show "/pause".toList = '/' :: "pause".toList from rfl, hr]
  simp [afterSnooze, operatorOnly, startsWith]

theorem route_worker_resume (n : Str) (hn : Seg n) :
    requiredScope .POST ("/workers/".toList ++ n ++ "/resume".toList) = .operator := by
  have hr := workersRoute_none n "resume".toList hn (by decide)
  unfold requiredScope
  rw [show "/resume".toList = '/' :: "resume".toList from rfl, hr]
  simp [afterSnooze, operatorOnly, startsWith]

theorem route_join_pending : requiredScope .GET "/join/pending".toList = .operator := by decide

theorem route_join_approve (code : Str) :
    requiredScope .POST ("/join/approve/".toList ++ code) = .operator := by route_scope

theorem route_access_devices (m : Method) : requiredScope m "/access/devices".toList = .operator := by
  cases m <;> decide

theorem route_revoke_device (id : Str) :
    requiredScope .DELETE ("/access/devices/".toList ++ id) = .operator := by route_scope

theorem route_secrets (m : Method) : requiredScope m "/secrets".toList = .operator := by
  cases m <;> decide

theorem route_secret (m : Method) (n : Str) :
    requiredScope m ("/secrets/".toList ++ n) = .operator := by route_scope

theorem route_github (m : Method) : requiredScope m "/github/sourcing".toList = .operator := by
  cases m <;> decide

-- snooze and wake: that worker's holder, or the operator
theorem route_snooze (m : Method) (n : Str) (hv : validWorkerName n = true) :
    requiredScope m ("/workers/".toList ++ n ++ "/snooze".toList) = .snooze n :=
  (requiredScope_snooze m _ n).2 ⟨hv, Or.inl rfl⟩

theorem route_wake (m : Method) (n : Str) (hv : validWorkerName n = true) :
    requiredScope m ("/workers/".toList ++ n ++ "/wake".toList) = .snooze n :=
  (requiredScope_snooze m _ n).2 ⟨hv, Or.inr rfl⟩

-- broker
theorem route_broker_entries : requiredScope .GET "/broker/entries".toList = .broker := by decide

/-! ### S7 headline facts -/

/-- Only the operator scope passes an operator route. -/
theorem S7_operator_routes_need_operator (ps : List Scope) :
    hasScope ps .operator = true ↔ .operator ∈ ps := by
  simp [hasScope]

/-- A credential with only `read` (and any snooze scopes) can't submit, cancel or re-key a job. -/
theorem S7_read_cannot_dispatch (ps : List Scope)
    (h : ∀ c ∈ ps, c = .read ∨ ∃ w, c = .snooze w) : hasScope ps .dispatch = false := by
  simp only [hasScope, List.any_eq_false]
  intro c hc
  rcases h c hc with rfl | ⟨w, rfl⟩ <;> simp

/-- Plain `dispatch` never runs a shell job: that takes `dispatch:shell` or the operator. -/
theorem S7_dispatch_cannot_shell (ps : List Scope) (h : canDispatchShell ps = true) :
    .operator ∈ ps ∨ .shell ∈ ps := by
  unfold canDispatchShell at h
  rw [Bool.or_eq_true, S7_operator_routes_need_operator] at h
  rcases h with h | h
  · exact Or.inl h
  · exact Or.inr (by simpa using h)

theorem S7_dispatch_read_no_shell : canDispatchShell [.dispatch, .read] = false := by decide

end Grove.Auth
