package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

type drainCall struct {
	id       string
	deadline time.Duration
}

type fakeDrainer struct {
	calls []drainCall
}

func (f *fakeDrainer) DrainNode(_ context.Context, nodeID string, enable bool, deadline time.Duration) error {
	if enable {
		f.calls = append(f.calls, drainCall{nodeID, deadline})
	}
	return nil
}

type rollCallFixture struct {
	r       *Reconciler
	client  *fakeOrchardClient
	nodes   *fakeNodeList
	drainer *fakeDrainer
	now     time.Time
}

func newRollCallFixture(vms ...string) *rollCallFixture {
	f := &rollCallFixture{nodes: &fakeNodeList{}, drainer: &fakeDrainer{}, now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	f.client = &fakeOrchardClient{}
	for _, name := range vms {
		f.client.vms = append(f.client.vms, orchard.VM{Name: name, Status: "running", CreatedAt: f.now.Add(-time.Hour)})
	}
	f.r = &Reconciler{Client: f.client, Spec: &Spec{Pools: []Pool{basePool()}}, Options: Options{
		NodeTLS: testNodeTLS, Nodes: f.nodes, NodeDrainer: f.drainer, Now: func() time.Time { return f.now },
	}}
	return f
}

func (f *rollCallFixture) node(id, name, pool string) {
	f.nodes.nodes = append(f.nodes.nodes, nomad.NodeSummary{ID: id, Name: name, Status: "ready", NodePool: pool, Eligible: true})
}

func TestRollCall_KnownNodesAreLeftAlone(t *testing.T) {
	f := newRollCallFixture("linux-mac1-0", "linux-mac2-0")
	f.node("n1", "linux-mac1-0", "linux")
	f.node("n2", "linux-mac2-0", "linux")
	f.node("cp", nomad.SandboxHostNodeName, "default")
	actions, err := f.r.RollCall(context.Background())
	if err != nil || len(actions) != 0 || len(f.drainer.calls) != 0 || len(f.client.deleted) != 0 {
		t.Fatalf("actions %+v, drains %+v, deleted %v, err %v", actions, f.drainer.calls, f.client.deleted, err)
	}
}

func TestRollCall_TakesOutNodesGroveDidNotMake(t *testing.T) {
	f := newRollCallFixture("linux-mac1-0", "linux-mac2-0")
	f.node("n1", "linux-mac1-0", "linux")
	f.node("ghost", "linux-mac9-0", "linux")  // its VM is gone (a copied identity)
	f.node("twin-a", "linux-mac2-0", "linux") // two nodes claim one VM
	f.node("twin-b", "linux-mac2-0", "linux")
	f.node("odd", "rogue", "linux") // not a VM name at all
	f.nodes.nodes = append(f.nodes.nodes,
		nomad.NodeSummary{ID: "old", Name: "linux-mac7-0", Status: "down", Eligible: true},    // the down-node sweeper's
		nomad.NodeSummary{ID: "done", Name: "linux-mac8-0", Status: "ready", Eligible: false}, // already drained
	)
	actions, err := f.r.RollCall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	drained := map[string]bool{}
	for _, c := range f.drainer.calls {
		drained[c.id] = true
		if c.deadline != drainDeadline {
			t.Errorf("drain of %s: deadline %v, want %v so a VM deleted outside grove keeps its running jobs", c.id, c.deadline, drainDeadline)
		}
	}
	for _, id := range []string{"ghost", "twin-a", "twin-b", "odd"} {
		if !drained[id] {
			t.Errorf("node %s was not taken out of service", id)
		}
	}
	for _, id := range []string{"n1", "old", "done"} {
		if drained[id] {
			t.Errorf("node %s was drained", id)
		}
	}
	if len(f.client.deleted) != 1 || f.client.deleted[0] != "linux-mac2-0" {
		t.Errorf("deleted %v, want only the VM two nodes claim, so it is rebuilt with a fresh identity", f.client.deleted)
	}
	if len(actions) != 4 {
		t.Fatalf("actions = %+v", actions)
	}
	for _, a := range actions {
		if a.Reason == "" || (a.Name == "linux-mac2-0") != (a.DeletedVM == "linux-mac2-0") {
			t.Errorf("action %+v", a)
		}
	}
}

func TestRollCall_ANodeOutsideItsNodePoolIsTakenOut(t *testing.T) {
	f := newRollCallFixture("linux-mac1-0")
	f.node("n1", "linux-mac1-0", "default")
	actions, err := f.r.RollCall(context.Background())
	if err != nil || len(actions) != 1 || !strings.Contains(actions[0].Reason, "node pool") {
		t.Fatalf("actions %+v, err %v", actions, err)
	}
	if len(f.client.deleted) != 1 {
		t.Fatalf("deleted %v", f.client.deleted)
	}
}

func TestRollCall_DrainsAVMAheadOfItsTTL(t *testing.T) {
	f := newRollCallFixture("linux-mac1-0")
	f.client.vms[0].TTL = time.Hour + ttlDrainLead + time.Minute
	f.node("n1", "linux-mac1-0", "linux")
	if _, err := f.r.RollCall(context.Background()); err != nil || len(f.drainer.calls) != 0 {
		t.Fatalf("drained %+v too early (%v)", f.drainer.calls, err)
	}
	f.now = f.now.Add(time.Minute)
	if _, err := f.r.RollCall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.drainer.calls) != 1 || f.drainer.calls[0] != (drainCall{"n1", drainDeadline}) {
		t.Fatalf("drains = %+v, want n1 drained gracefully before Orchard recycles it", f.drainer.calls)
	}
	if len(f.client.deleted) != 0 {
		t.Fatal("Orchard recycles a VM at its TTL; the roll call must not delete it")
	}
}

func TestRollCall_OffWithoutStrictIntroduction(t *testing.T) {
	f := newRollCallFixture()
	f.node("ghost", "linux-mac9-0", "linux")
	f.r.Options.NodeTLS = nil
	if actions, err := f.r.RollCall(context.Background()); err != nil || len(actions) != 0 || len(f.drainer.calls) != 0 {
		t.Fatalf("actions %+v, err %v", actions, err)
	}
}

func TestApply_DrainsADeletedVMsNodeFirst(t *testing.T) {
	f := newRollCallFixture("linux-mac1-0")
	f.node("n1", "linux-mac1-0", "linux")
	plan := Plan{Deletes: []PlannedDelete{{Name: "linux-mac1-0", Pool: "linux", Worker: "mac1", Reason: "spec changed"}}}
	if err := f.r.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(f.drainer.calls) != 1 || f.drainer.calls[0] != (drainCall{"n1", drainDeadline}) || len(f.client.deleted) != 1 {
		t.Fatalf("drains %+v, deleted %v: want n1 drained, then the VM deleted", f.drainer.calls, f.client.deleted)
	}
}
