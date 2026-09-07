package server

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

const recycleDrainDeadline = 2 * time.Hour

// handleRecycleVM finds the Nomad node running vm (matched by meta.vm), drains it, and returns
// 202 immediately. A background goroutine deletes the VM via Orchard once the node has 0 running
// allocations, or once the drain deadline passes (whichever comes first).
func (s *Server) handleRecycleVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "vm name required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	nodes, err := s.nomad.ListNodes(ctx)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}
	var nodeID string
	found := false
	for _, n := range nodes {
		if n.Meta["vm"] == name {
			nodeID = n.ID
			found = true
			break
		}
	}
	if !found {
		http.Error(w, fmt.Sprintf("no nomad node found for vm %q", name), http.StatusNotFound)
		return
	}

	if err := s.nomad.DrainNode(ctx, nodeID, true, recycleDrainDeadline); err != nil {
		s.writeError(w, http.StatusBadGateway, err)
		return
	}

	go s.finishRecycle(name, nodeID)

	s.writeJSON(w, http.StatusAccepted, map[string]any{"drainStarted": true})
}

func (s *Server) finishRecycle(vmName, nodeID string) {
	ctx, cancel := context.WithTimeout(context.Background(), recycleDrainDeadline)
	defer cancel()

	ticker := time.NewTicker(s.opts.RecyclePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Warn("recycle: drain deadline reached, deleting vm anyway", "vm", vmName, "node", nodeID)
			s.deleteVM(vmName)
			return
		case <-ticker.C:
			nodes, err := s.nomad.ListNodes(ctx)
			if err != nil {
				s.log.Warn("recycle: list nodes failed", "vm", vmName, "err", err)
				continue
			}
			for _, n := range nodes {
				if n.ID != nodeID {
					continue
				}
				if n.RunningAllocs == 0 {
					s.deleteVM(vmName)
					return
				}
				break
			}
		}
	}
}

func (s *Server) deleteVM(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.orchard.DeleteVM(ctx, name); err != nil {
		s.log.Error("recycle: delete vm failed", "vm", name, "err", err)
	} else {
		s.log.Info("recycle: vm deleted", "vm", name)
	}
	if s.recycleDone != nil {
		s.recycleDone <- name
	}
}
