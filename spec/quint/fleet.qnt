// One shared VM slot of a pool over its lifetimes (L7, L10 in spec/README.md):
// fleet.Reconciler.Plan/Apply (internal/fleet/reconcile.go: create the missing VM, rebuild one
// that failed or boot-loops once FailedVMRetry has passed since it was created), the TTL drain in
// fleet.Reconciler.RollCall (internal/fleet/rollcall.go: drain the node ttlDrainLead before
// Orchard recycles the VM at its TTL), Orchard's TTL recycle, and Nomad placing jobs.
//
// Units of 30s (one grove serve tick). The pool's TTL is ttl units, ttlDrainLead = 2m = 4 units,
// FailedVMRetry = 10m = 20 units (scaled down to `retry` here: only its order relative to the TTL
// matters; LoginSessionRetry = 1h, for a macOS VM that failed only because its worker user has no
// login session, is the same finite wait, longer). DrainNode may fail on up to drainFails ticks in a row.
module fleet {
  const ttl: int
  const retry: int
  const lead: int
  const drainFails: int
  const badBoots: int        // how many boots in a row go wrong (failed or boot loop)

  var vm: str        // "none" | "booting" | "running" | "unhealthy" | "recycling"
  var age: int       // units since the VM was created (capped at ttl)
  var node: str      // "none" | "eligible" | "drained"
  var jobOnDying: bool   // Nomad placed a job on a VM Orchard was already recycling
  var failsLeft: int
  var badLeft: int
  var ran: Set[str]

  pure val controllers = Set("F", "R", "B")

  action init = all {
    vm' = "none", age' = 0, node' = "none", jobOnDying' = false,
    failsLeft' = drainFails, badLeft' = badBoots, ran' = Set(),
  }

  // Plan/Apply: create the missing VM; rebuild an unhealthy one once it is old enough
  // (drainBeforeDelete first, deletes before creates)
  action reconcile = all {
    not(ran.contains("F")), ran' = ran.union(Set("F")),
    if (vm == "none" or (vm == "unhealthy" and age >= retry))
      all { vm' = "booting", age' = 0, node' = "none" }
    else
      all { vm' = vm, age' = age, node' = node },
    jobOnDying' = jobOnDying, failsLeft' = failsLeft, badLeft' = badLeft,
  }

  // the VM boots and enrols (enrol.qnt covers how), or it fails or boot-loops
  action boot = all {
    not(ran.contains("B")), ran' = ran.union(Set("B")),
    any {
      all { vm == "booting", vm' = "running", node' = "eligible", badLeft' = badLeft },
      all { vm == "booting", badLeft > 0, vm' = "unhealthy", node' = node, badLeft' = badLeft - 1 },
      all { vm != "booting", vm' = vm, node' = node, badLeft' = badLeft },
    },
    age' = age, jobOnDying' = jobOnDying, failsLeft' = failsLeft,
  }

  // RollCall's TTL drain: within lead of the TTL, drain the eligible node (DrainNode may fail)
  action rollCall = all {
    not(ran.contains("R")), ran' = ran.union(Set("R")),
    val due = vm == "running" and node == "eligible" and age >= ttl - lead
    any {
      all { due, node' = "drained", failsLeft' = drainFails },
      all { due, failsLeft > 0, node' = node, failsLeft' = failsLeft - 1 },
      all { not(due), node' = node, failsLeft' = failsLeft },
    },
    vm' = vm, age' = age, jobOnDying' = jobOnDying, badLeft' = badLeft,
  }

  // Orchard recycles the VM at its TTL: its shutdown script waits out its jobs, then it is gone
  action recycle = all {
    vm == "running", age >= ttl, vm' = "recycling",
    age' = age, node' = node, jobOnDying' = jobOnDying, failsLeft' = failsLeft, badLeft' = badLeft, ran' = ran,
  }
  action gone = all {
    vm == "recycling", vm' = "none", node' = "none", age' = 0,
    jobOnDying' = jobOnDying, failsLeft' = failsLeft, badLeft' = badLeft, ran' = ran,
  }

  // Nomad places a job on an eligible node
  action place = all {
    node == "eligible",
    jobOnDying' = (jobOnDying or vm == "recycling"),
    vm' = vm, age' = age, node' = node, failsLeft' = failsLeft, badLeft' = badLeft, ran' = ran,
  }

  action tick = all {
    ran == controllers, ran' = Set(),
    age' = if (vm != "none" and age < ttl) age + 1 else age,
    vm' = vm, node' = node, jobOnDying' = jobOnDying, failsLeft' = failsLeft, badLeft' = badLeft,
  }

  action step = any { reconcile, boot, rollCall, recycle, gone, place, tick }

  // L7 (safety half): no job lands on a VM whose recycle has begun: the TTL drain came first.
  val noJobOnRecyclingVM = not(jobOnDying)
  val drainedBeforeRecycle = vm == "recycling" implies node != "eligible"

  temporal fairness = all {
    weakFair(reconcile, (vm, age, node, jobOnDying, failsLeft, badLeft, ran)),
    weakFair(boot, (vm, age, node, jobOnDying, failsLeft, badLeft, ran)),
    weakFair(rollCall, (vm, age, node, jobOnDying, failsLeft, badLeft, ran)),
    weakFair(recycle, (vm, age, node, jobOnDying, failsLeft, badLeft, ran)),
    weakFair(gone, (vm, age, node, jobOnDying, failsLeft, badLeft, ran)),
    weakFair(tick, (vm, age, node, jobOnDying, failsLeft, badLeft, ran)),
  }
  // L7 (liveness half) and L10: whatever happens to it (TTL recycle, failed boot, boot loop), the
  // pool's slot always gets a running VM with an eligible node again.
  temporal alwaysComesBack = fairness implies always(eventually(vm == "running" and node == "eligible"))
  // L10: an unhealthy VM is rebuilt.
  temporal unhealthyIsRebuilt = fairness implies always(vm == "unhealthy" implies eventually(vm == "booting"))
}

module fleetTTL { import fleet(ttl = 12, retry = 6, lead = 4, drainFails = 3, badBoots = 2).* }
// The TTL drain needs one success in its 4 ticks: 4 failures in a row let a job land (expected to FAIL).
module fleetTTLDrainDown { import fleet(ttl = 12, retry = 6, lead = 4, drainFails = 4, badBoots = 0).* }
