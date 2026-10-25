package fleet

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

type fakePurger struct {
	nodes  []nomad.NodeSummary
	purged []string
	err    error
}

func (f *fakePurger) ListNodeSummaries(context.Context) ([]nomad.NodeSummary, error) {
	return f.nodes, nil
}

func (f *fakePurger) PurgeNode(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	f.purged = append(f.purged, id)
	return nil
}

func TestNodeSweeper_PurgesOnlyNodesDownPastGrace(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	nodes := &fakePurger{nodes: []nomad.NodeSummary{
		{ID: "ready", Status: "ready"},
		{ID: "down1", Status: "down"},
		{ID: "down2", Status: "down"},
		{ID: "back", Status: "down"},
	}}
	s := &NodeSweeper{Nodes: nodes, Grace: 10 * time.Minute, Now: func() time.Time { return now }}

	if n, err := s.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("first sight of a down node must not purge it: n=%d err=%v", n, err)
	}

	// "back" recovers before the grace runs out; it must start over if it goes down again.
	nodes.nodes[3].Status = "ready"
	now = now.Add(5 * time.Minute)
	if n, _ := s.Sweep(context.Background()); n != 0 {
		t.Fatalf("purged %d before the grace ran out", n)
	}
	nodes.nodes[3].Status = "down"

	now = now.Add(6 * time.Minute)
	n, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(nodes.purged)
	if n != 2 || len(nodes.purged) != 2 || nodes.purged[0] != "down1" || nodes.purged[1] != "down2" {
		t.Fatalf("purged %v (n=%d), want down1 and down2 only", nodes.purged, n)
	}
}

func TestNodeSweeper_StopsOnPermissionDenied(t *testing.T) {
	now := time.Now()
	nodes := &fakePurger{
		nodes: []nomad.NodeSummary{{ID: "a", Status: "down"}, {ID: "b", Status: "down"}},
		err:   nomad.ErrPermissionDenied,
	}
	s := &NodeSweeper{Nodes: nodes, Grace: time.Nanosecond, Now: func() time.Time { return now }}
	s.Sweep(context.Background())
	now = now.Add(time.Second)
	if _, err := s.Sweep(context.Background()); !errors.Is(err, nomad.ErrPermissionDenied) {
		t.Fatalf("want ErrPermissionDenied, got %v", err)
	}
}
