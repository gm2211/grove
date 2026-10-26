package nomad

import (
	"context"
	"errors"
	"fmt"

	nomadapi "github.com/hashicorp/nomad/api"
)

// NodeSummary is the part of a Nomad node that one cheap list call returns: enough to find nodes
// that are gone without fetching every node and its allocations, as ListNodes does.
type NodeSummary struct {
	ID     string
	Name   string
	Status string // ready | down | initializing | disconnected
	// NodePool is the pool the node registered in. Under strict client introduction that is the
	// pool its introduction token was bound to, not something the node chose.
	NodePool string
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
		out = append(out, NodeSummary{ID: s.ID, Name: s.Name, Status: s.Status, NodePool: s.NodePool})
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

// NodePools creates Nomad node pools. Each shared fleet pool registers its VMs in a node pool of
// its own name (the introduction token binds a VM to it), and grove's jobs for that pool name it,
// so a node can only ever be handed its own pool's jobs. Needs node_pool:write (the management
// token). The concrete Client returned by New implements it; see AsNodePools.
type NodePools interface {
	// EnsureNodePool creates the pool if it doesn't exist. An existing pool is left as it is.
	EnsureNodePool(ctx context.Context, name, description string) error
}

// AsNodePools returns c's NodePools implementation, if it has one.
func AsNodePools(c Client) (NodePools, bool) {
	p, ok := c.(NodePools)
	return p, ok
}

func (c *client) EnsureNodePool(ctx context.Context, name, description string) error {
	if _, _, err := c.raw.NodePools().Info(name, qOpts(ctx)); err == nil {
		return nil
	} else if !errors.Is(wrapNotFound(err), ErrNotFound) {
		return fmt.Errorf("nomad: read node pool %s: %w", name, wrapDenied(err))
	}
	if _, err := c.raw.NodePools().Register(&nomadapi.NodePool{Name: name, Description: description}, wOpts(ctx)); err != nil {
		return fmt.Errorf("nomad: create node pool %s: %w", name, wrapDenied(err))
	}
	return nil
}

// NodeLoads reports which VMs' Nomad clients have joined and whether each runs a job, for the
// fleet autoscaler. The concrete Client returned by New implements it; see AsNodeLoads.
type NodeLoads interface {
	// VMLoads maps meta.vm of every ready node to whether it has a running allocation. Down and
	// initializing nodes, and nodes without meta.vm, are left out.
	VMLoads(ctx context.Context) (map[string]bool, error)
}

// AsNodeLoads returns c's NodeLoads implementation, if it has one.
func AsNodeLoads(c Client) (NodeLoads, bool) {
	l, ok := c.(NodeLoads)
	return l, ok
}

func (c *client) VMLoads(ctx context.Context) (map[string]bool, error) {
	stubs, _, err := c.raw.Nodes().List(qOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("nomad: list nodes: %w", wrapDenied(err))
	}
	out := map[string]bool{}
	for _, stub := range stubs {
		if stub == nil || stub.Status != "ready" {
			continue
		}
		node, _, err := c.raw.Nodes().Info(stub.ID, qOpts(ctx))
		if err != nil {
			return nil, fmt.Errorf("nomad: get node %s: %w", stub.ID, err)
		}
		vm := node.Meta["vm"]
		if vm == "" {
			continue
		}
		allocs, _, err := c.raw.Nodes().Allocations(stub.ID, qOpts(ctx))
		if err != nil {
			return nil, fmt.Errorf("nomad: list allocations for node %s: %w", stub.ID, err)
		}
		out[vm] = out[vm] || runningAllocCount(allocs) > 0
	}
	return out, nil
}
