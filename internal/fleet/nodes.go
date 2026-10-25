package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/gm2211/grove/internal/nomad"
)

// DefaultDownNodeGrace is how long a Nomad node must have been seen "down" before NodeSweeper
// purges it.
const DefaultDownNodeGrace = 10 * time.Minute

// NodeSweeper purges Nomad nodes that stay down. Each recycled or restarted VM boots a fresh Nomad
// client that registers as a new node, so without it the down ones pile up (into the thousands
// for a VM stuck restarting), slowing every /fleet call, which fetches each node.
//
// It remembers when it first saw each node down, in process memory only: after a restart a node
// waits Grace again before it's purged. A node that comes back is forgotten.
type NodeSweeper struct {
	Nodes nomad.NodePurger
	// Grace defaults to DefaultDownNodeGrace.
	Grace time.Duration
	Now   func() time.Time

	downSince map[string]time.Time
}

// Sweep lists nodes once and purges those down for at least Grace. It returns how many it purged.
// It stops at the first nomad.ErrPermissionDenied, since every other purge would fail the same way.
func (s *NodeSweeper) Sweep(ctx context.Context) (int, error) {
	if s.Nodes == nil {
		return 0, nil
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	grace := s.Grace
	if grace <= 0 {
		grace = DefaultDownNodeGrace
	}

	nodes, err := s.Nodes.ListNodeSummaries(ctx)
	if err != nil {
		return 0, err
	}
	seen := make(map[string]time.Time, len(nodes))
	for _, n := range nodes {
		if n.Status != "down" {
			continue
		}
		since, ok := s.downSince[n.ID]
		if !ok {
			since = now
		}
		seen[n.ID] = since
	}
	s.downSince = seen

	purged := 0
	var errs []error
	for id, since := range seen {
		if now.Sub(since) < grace {
			continue
		}
		if err := s.Nodes.PurgeNode(ctx, id); err != nil {
			if errors.Is(err, nomad.ErrPermissionDenied) {
				return purged, err
			}
			errs = append(errs, err)
			continue
		}
		delete(s.downSince, id)
		purged++
	}
	return purged, errors.Join(errs...)
}
