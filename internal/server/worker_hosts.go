package server

import (
	"context"
	"strings"
	"sync"
	"time"
)

// workerHostsTTL bounds how stale the Orchard worker list behind isWorkerHost may be; the same
// window as tsauth's whois cache.
const workerHostsTTL = 30 * time.Second

// workerHosts caches the Orchard worker names, normalized the way Tailscale derives a MagicDNS
// label from a hostname, so isWorkerHost can recognize a worker Mac's tailnet node.
type workerHosts struct {
	mu     sync.Mutex
	at     time.Time
	names  map[string]struct{}
	loaded bool
}

// isWorkerHost reports whether node (a MagicDNS name) is the Mac of an Orchard worker. A shared
// pool's VM reaches the control plane through that Mac's NAT, so the Mac's Tailscale identity is
// also every one of its jobs' identity, and must not authenticate. When Orchard can't be listed the
// last known list is used; with none yet, every peer counts as a worker host (fail closed).
func (s *Server) isWorkerHost(ctx context.Context, node string) bool {
	if s.orchard == nil {
		return false
	}
	label := hostLabel(node)
	h := s.workerHosts
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.loaded || time.Since(h.at) >= workerHostsTTL {
		workers, err := s.orchard.ListWorkers(ctx)
		if err == nil {
			h.names = make(map[string]struct{}, len(workers))
			for _, w := range workers {
				if l := hostLabel(w.Name); l != "" {
					h.names[l] = struct{}{}
				}
			}
			h.loaded = true
			h.at = time.Now()
		} else if !h.loaded {
			s.log.Warn("auth: cannot list Orchard workers to rule out worker hosts; refusing Tailscale identity", "err", err)
			return true
		}
	}
	_, ok := h.names[label]
	return ok
}

// hostLabel reduces a hostname or MagicDNS name to the label Tailscale would give it: the first
// DNS label, lowercased, with anything but letters, digits and '-' turned into '-'.
func hostLabel(name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
