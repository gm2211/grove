package nomad

import (
	"context"
	"errors"
	"fmt"
)

// NodeSummary is the part of a Nomad node that one cheap list call returns: enough to find nodes
// that are gone without fetching every node and its allocations, as ListNodes does.
type NodeSummary struct {
	ID     string
	Name   string
	Status string // ready | down | initializing | disconnected
}

// NodePurger finds and removes Nomad client nodes. Every recycled or restarted VM boots a fresh
// Nomad client, which registers as a new node, and the old one stays behind as "down" until the
// server garbage-collects it. Purging needs node:write. The concrete Client returned by New
// implements it; see AsNodePurger.
type NodePurger interface {
	// ListNodeSummaries lists every node with one API call.
	ListNodeSummaries(ctx context.Context) ([]NodeSummary, error)
	// PurgeNode deregisters a node. A node that is in fact alive registers itself again on its
	// next heartbeat, so purging is never destructive. An already-gone node is not an error.
	PurgeNode(ctx context.Context, nodeID string) error
}

// AsNodePurger returns c's NodePurger implementation, if it has one.
func AsNodePurger(c Client) (NodePurger, bool) {
	p, ok := c.(NodePurger)
	return p, ok
}

func (c *client) ListNodeSummaries(ctx context.Context) ([]NodeSummary, error) {
	stubs, _, err := c.raw.Nodes().List(qOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("nomad: list nodes: %w", wrapDenied(err))
	}
	out := make([]NodeSummary, 0, len(stubs))
	for _, s := range stubs {
		if s == nil {
			continue
		}
		out = append(out, NodeSummary{ID: s.ID, Name: s.Name, Status: s.Status})
	}
	return out, nil
}

func (c *client) PurgeNode(ctx context.Context, nodeID string) error {
	if _, _, err := c.raw.Nodes().Purge(nodeID, qOpts(ctx)); err != nil {
		err = wrapNotFound(err)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("nomad: purge node %s: %w", nodeID, wrapDenied(err))
	}
	return nil
}
