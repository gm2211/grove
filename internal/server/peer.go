package server

import (
	"context"
	"net"
	"net/http"
	"net/netip"

	"github.com/gm2211/grove/internal/tsauth"
)

// PeerIdentifier names the tailnet node behind a peer address, whether or not anything grants it
// a scope; *tsauth.Authenticator implements it. When Options.Tailscale also is one, a worker's
// broker credential is bound to its node as well as its address, and GET /join/pending shows
// which machine is asking.
type PeerIdentifier interface {
	Identify(ctx context.Context, remoteAddr string) (tsauth.Identity, bool)
}

// peerIP is the address r's connection came from, in canonical form (an IPv4-mapped IPv6 address
// as IPv4, no zone). It is "" on the local socket, which has no peer address, and for anything
// that isn't an IP. Nothing here reads a request header.
func peerIP(r *http.Request) string {
	if viaLocalSocket(r.Context()) {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	return addr.Unmap().WithZone("").String()
}

// identifyPeer asks Tailscale which node holds ip. ok is false with Tailscale identity off, for
// an address that isn't on the tailnet (loopback: a worker on the control plane's own Mac), or
// when tailscaled can't place it.
func (s *Server) identifyPeer(ctx context.Context, ip string) (tsauth.Identity, bool) {
	if ip == "" || s.opts.Tailscale == nil {
		return tsauth.Identity{}, false
	}
	identifier, ok := s.opts.Tailscale.(PeerIdentifier)
	if !ok {
		return tsauth.Identity{}, false
	}
	return identifier.Identify(ctx, ip)
}
