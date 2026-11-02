// Nomad identities for shared VMs (S3, S4, L8, L9 in spec/README.md): fleet.Reconciler.Enrol
// (internal/fleet/enrol.go), fleet.Reconciler.RollCall (internal/fleet/rollcall.go) and Nomad's
// strict client introduction (a client registers only with a certificate from grove's CA AND an
// introduction token bound to its name and node pool; nomadtls_integration_test.go checks Nomad
// really refuses the rest).

// --- S3 and S4: who can hold a Nomad identity, and what the roll call takes out ---
//
// Two workers: "good" matches pool prod, "evil" matches only pool dev and is compromised: its
// owner holds its Orchard credential (compute:write, so it can create VMs on itself, with any
// name and grove's exact spec) and root on the host, so it reads everything grove pushes into
// a VM running there. It can also run Nomad clients anywhere with any credential it has read.
module identity {
  // fixed: Enrol only enrols a VM on a worker its pool's selector matches (and that Plan would
  // not block), the way Plan decides where a pool's VMs go. Before: name, pin and spec only.
  const fixed: bool

  pure val pools = Set("prod", "dev")
  pure val selector = Map("prod" -> Set("good"), "dev" -> Set("evil"))
  pure val workers = Set("good", "evil")
  type Name = { pool: str, worker: str }
  type Node = { name: Name, pool: str, by: str, eligible: bool }   // by: "vm" | "attacker"
  pure val names = tuples(pools, workers).map(((p, w)) => { pool: p, worker: w })

  var vms: Set[Name]         // Orchard's VMs (all running; an attacker's VM boots instantly)
  var creds: Set[Name]       // node identities grove pushed (certificate + intro token for name.pool)
  var leaked: Set[Name]      // the identities the attacker has read
  var nodes: Set[Node]       // Nomad's live nodes
  var dirty: bool            // something changed since the last roll call

  // Plan: the VMs a pool wants (one per matching worker)
  val desired = names.filter(n => selector.get(n.pool).contains(n.worker))

  action init = all {
    vms' = desired, creds' = Set(), leaked' = Set(), nodes' = Set(), dirty' = false,
  }

  // fleet.Reconciler.Enrol: push an identity into every running VM that wantsEnrolled
  def wantsEnrolled(n: Name): bool = not(fixed) or selector.get(n.pool).contains(n.worker)
  action enrol = all {
    val pushed = vms.filter(n => wantsEnrolled(n))
    all {
      creds' = creds.union(pushed),
      leaked' = leaked.union(pushed.filter(n => n.worker == "evil")),
      vms' = vms, nodes' = nodes, dirty' = dirty,
    },
  }

  // a grove VM's own Nomad client registers with what grove pushed it
  action vmRegisters = {
    nondet n = creds.intersect(vms).oneOf()
    all {
      not(nodes.exists(x => x.name == n and x.by == "vm")),
      nodes' = nodes.union(Set({ name: n, pool: n.pool, by: "vm", eligible: true })),
      vms' = vms, creds' = creds, leaked' = leaked, dirty' = true,
    }
  }

  // Plan + Apply: delete undesired VMs (draining their nodes first), create missing ones
  action apply = all {
    val gone = vms.exclude(desired)
    all {
      vms' = desired,
      nodes' = nodes.map(x => if (gone.contains(x.name)) { ...x, eligible: false } else x),
      creds' = creds, leaked' = leaked, dirty' = dirty,
    },
  }

  // the attacker: create a VM on its own host, or register a Nomad client with a leaked identity,
  // claiming any node pool (Nomad refuses a pool the token isn't bound to)
  action attackerCreates = {
    nondet p = pools.oneOf()
    val n = { pool: p, worker: "evil" }
    all { not(vms.contains(n)), vms' = vms.union(Set(n)),
          creds' = creds, leaked' = leaked, nodes' = nodes, dirty' = true }
  }
  action attackerRegisters = {
    nondet n = leaked.oneOf()
    nondet p = pools.oneOf()
    all {
      p == n.pool,  // strict introduction: the token is bound to the node pool
      nodes' = nodes.union(Set({ name: n, pool: p, by: "attacker", eligible: true })),
      vms' = vms, creds' = creds, leaked' = leaked, dirty' = true,
    }
  }

  // fleet.Reconciler.RollCall: drain every live node no VM accounts for, every node sharing its VM
  // with another, every node outside its pool's node pool; delete the VM such a node claimed
  def claims(n: Name): Set[Node] = nodes.filter(x => x.eligible and x.name == n)
  def accounted(x: Node): bool = vms.contains(x.name) and claims(x.name).size() == 1 and x.pool == x.name.pool
  action rollCall = all {
    val bad = nodes.filter(x => x.eligible and not(accounted(x)))
    val badNames = bad.map(x => x.name)
    all {
      nodes' = nodes.map(x => if (bad.contains(x)) { ...x, eligible: false } else x),
      vms' = vms.exclude(badNames.intersect(vms)),
      creds' = creds, leaked' = leaked, dirty' = false,
    },
  }

  action step = any { enrol, vmRegisters, apply, attackerCreates, attackerRegisters, rollCall }

  // S3: no identity for a pool the compromised worker isn't in ever reaches it.
  val noForeignIdentity = leaked.forall(n => selector.get(n.pool).contains(n.worker))
  // S3, consequence: no Nomad node the attacker runs is eligible for prod's jobs.
  val noAttackerInProd = nodes.forall(x => not(x.by == "attacker" and x.pool == "prod" and x.eligible))
  // S4: right after a roll call, every eligible node is one a VM accounts for, alone, in its pool.
  val rollCallLeavesOnlyAccounted = not(dirty) implies nodes.forall(x => x.eligible implies accounted(x))
}

module identityFixed { import identity(fixed = true).* }
module identityBefore { import identity(fixed = false).* }

// --- L8: a running VM's certificate is renewed before it expires ---
// Units of one hour. NodeCertValidity = 24h, nodeCertRenewBefore = 12h, Enrol runs every 30s
// (several times per unit). Every pass in a unit may fail (Orchard exec down, CA unreadable): the
// model allows up to maxFailHours consecutive failed hours. Renewal is due for the last 12 hours,
// of which the model sees 11 whole ones: any outage shorter than that (10 failed hours) is
// survived, and the instance with 11 shows the bound is tight (expected to FAIL).
module certs {
  const maxFailHours: int
  pure val validity = 24
  pure val renewBefore = 12

  var left: int       // hours until the certificate's NotAfter
  var failing: int    // consecutive hours every enrol pass failed
  var renewedThisHour: bool

  action init = all { left' = validity, failing' = 0, renewedThisHour' = false }
  // Enrol: renew once now > NotAfter - 12h
  action renew = all {
    not(renewedThisHour), left < renewBefore,
    left' = validity, failing' = 0, renewedThisHour' = true,
  }
  action hour = any {
    // at least one pass this hour worked (or nothing was due)
    all { renewedThisHour or left >= renewBefore, left > 0,
          left' = left - 1, failing' = 0, renewedThisHour' = false },
    // every pass this hour failed
    all { not(renewedThisHour), left < renewBefore, failing < maxFailHours, left > 0,
          left' = left - 1, failing' = failing + 1, renewedThisHour' = false },
  }
  action step = any { renew, hour }
  val neverExpires = left > 0
}
module certsTolerant { import certs(maxFailHours = 10).* }
module certsTooManyFailures { import certs(maxFailHours = 11).* }

// --- L9: a genuine new VM eventually joins Nomad ---
// Units of 10 minutes; introTokenTTL = 30m = 3 units. The VM's Nomad client may be slow, but it
// keeps retrying (strong fairness: a registration possible infinitely often happens). A previous
// VM of the same name may still have a node draining (present, ineligible) until it goes down.
module join {
  pure val tokenTTL = 3
  var tokenAge: int     // units since the last introduction token (-1: none yet)
  var registered: bool
  var twin: bool        // the previous VM's node of the same name is still present

  action init = all { tokenAge' = -1, registered' = false, nondet t = Set(true, false).oneOf() twin' = t }
  // Enrol: first enrolment of this VM (new UID), or re-enrolment once its token is stale and no
  // node of its name is present
  action enrol = all {
    not(registered), tokenAge == -1 or (tokenAge > tokenTTL and not(twin)),
    tokenAge' = 0, registered' = registered, twin' = twin,
  }
  action register = all {
    not(registered), tokenAge >= 0, tokenAge < tokenTTL,
    registered' = true, tokenAge' = tokenAge, twin' = twin,
  }
  action time = all {
    tokenAge' = if (tokenAge >= 0 and tokenAge <= tokenTTL) tokenAge + 1 else tokenAge,
    registered' = registered, twin' = twin,
  }
  action twinGoesDown = all { twin, twin' = false, tokenAge' = tokenAge, registered' = registered }
  action step = any { enrol, register, time, twinGoesDown }

  temporal fairness = all {
    weakFair(enrol, (tokenAge, registered, twin)),
    strongFair(register, (tokenAge, registered, twin)),
    weakFair(time, (tokenAge, registered, twin)),
    weakFair(twinGoesDown, (tokenAge, registered, twin)),
  }
  temporal joins = fairness implies eventually(registered)
}
