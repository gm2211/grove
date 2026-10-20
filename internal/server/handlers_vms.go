package server

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// handleRecycleVM deletes vm through Orchard and returns 202 right away. Deleting a sealed VM runs
// its shutdown script, which stops new jobs landing in it and waits for the running ones to finish
// (see internal/fleet's sealed shutdown script); the fleet reconciler then recreates it from its
// image.
func (s *Server) handleRecycleVM(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "vm name required", http.StatusBadRequest)
		return
	}
	vm, err := s.orchard.GetVM(r.Context(), name)
	if err != nil || vm == nil {
		http.Error(w, fmt.Sprintf("no vm %q", name), http.StatusNotFound)
		return
	}
	go s.deleteVM(name)
	s.writeJSON(w, http.StatusAccepted, map[string]any{"drainStarted": true})
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
