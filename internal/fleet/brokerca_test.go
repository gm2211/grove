package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/orchard"
)

// A worker's broker that replaced its CA (a release added an inject host) leaves every VM booted
// before then trusting the old one, and each clone through the broker fails with "self signed
// certificate in certificate chain". The Studio's macOS pool sat like that for two days, because
// recycling the VMs was a step for the operator. Plan now rebuilds such a VM by itself, once idle.
func TestPlan_RebuildsAVMThatTrustsTheBrokersPreviousCA(t *testing.T) {
	pool := basePool()
	now := time.Date(2026, 11, 24, 8, 30, 0, 0, time.UTC)
	cas := &BrokerCAs{}
	r := &Reconciler{Spec: &Spec{Pools: []Pool{pool}}, Options: Options{Now: func() time.Time { return now }, BrokerCAs: cas}}
	name := VMName(pool.Name, "mac1", 0)
	vm := existingFrom(r.vmSpec(pool, "mac1", name))
	vm.Status = "running"
	vm.CreatedAt = time.Date(2026, 11, 22, 5, 15, 11, 0, time.UTC)
	client := &fakeOrchardClient{workers: []orchard.Worker{onlineWorker("mac1")}, vms: []orchard.VM{vm}}
	r.Client = client

	plan := func() Plan {
		t.Helper()
		p, err := r.Plan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	if p := plan(); !p.Empty() {
		t.Fatalf("no broker has reported its CA yet, so nothing is stale: %+v", p)
	}

	// The broker made its CA before the VM booted: the VM fetched that one.
	cas.Observe("mac1", vm.CreatedAt.Add(-time.Hour), now)
	if p := plan(); !p.Empty() {
		t.Fatalf("a VM that booted after its broker's CA was made trusts it: %+v", p)
	}

	// Another worker's broker says nothing about this VM.
	cas.Observe("mac2", now.Add(-time.Minute), now)
	if p := plan(); !p.Empty() {
		t.Fatalf("another worker's CA changed nothing here: %+v", p)
	}

	// The broker restarted on a release with a new CA, nine minutes after the VM booted.
	made := time.Date(2026, 11, 22, 5, 24, 5, 0, time.UTC)
	if !cas.Observe("mac1", made, now) {
		t.Fatal("a new CA time is a change")
	}
	if p := plan(); !p.Empty() {
		t.Fatalf("a tick that couldn't read which VMs are busy deletes nothing: %+v", p)
	}
	r.Options.Load = &JobLoad{Busy: map[string]bool{name: true}}
	if p := plan(); !p.Empty() {
		t.Fatalf("a VM running a job is never deleted under it, even one that can't clone: %+v", p)
	}
	r.Options.Load = &JobLoad{Busy: map[string]bool{name: false}}
	p := plan()
	if len(p.Deletes) != 1 || len(p.Creates) != 1 || p.Deletes[0].Name != name || p.Creates[0].Spec.Name != name {
		t.Fatalf("want the idle VM that trusts the old CA rebuilt, got %+v", p)
	}
	if !strings.Contains(p.Deletes[0].Reason, "previous CA") {
		t.Errorf("delete reason = %q, want it to say the VM trusts the broker's previous CA", p.Deletes[0].Reason)
	}

	// The rebuilt VM boots after the CA was made: nothing more to do.
	vm.CreatedAt = now
	client.vms[0] = vm
	if p := plan(); !p.Empty() {
		t.Fatalf("the rebuilt VM is current: %+v", p)
	}

	// A sealed pool's VMs have no broker.
	sealed := basePool()
	sealed.Network = NetworkIsolated
	if why := r.staleBrokerCA(sealed, "mac1", orchard.VM{Name: name, CreatedAt: made.Add(-time.Hour)}); why != "" {
		t.Errorf("a sealed VM was called stale: %q", why)
	}
}

// A reported time far in the future would make every VM look older than its broker's CA, and be
// rebuilt on every tick: it is ignored, as is a zero time and a nil store.
func TestBrokerCAs_IgnoresTimesItCannotTrust(t *testing.T) {
	now := time.Date(2026, 11, 24, 8, 30, 0, 0, time.UTC)
	cas := &BrokerCAs{}
	if cas.Observe("mac1", now.Add(time.Hour), now) {
		t.Error("a CA made an hour from now was recorded")
	}
	if cas.Observe("mac1", time.Time{}, now) {
		t.Error("a zero time was recorded")
	}
	if _, ok := cas.Since("mac1"); ok {
		t.Error("an ignored time is known")
	}
	if !cas.Observe("mac1", now.Add(time.Minute), now) {
		t.Error("a minute of clock skew is allowed")
	}
	if cas.Observe("mac1", now.Add(time.Minute), now) {
		t.Error("the same time again is no change")
	}
	var none *BrokerCAs
	if none.Observe("mac1", now, now) {
		t.Error("a nil store recorded something")
	}
	if _, ok := none.Since("mac1"); ok {
		t.Error("a nil store knows something")
	}
}
