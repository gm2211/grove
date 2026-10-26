package tsauth

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// CheckListen reports an error unless every address `grove serve` would bind for listen is
// loopback or on the tailnet. Grove is never meant to be reachable from a LAN or the internet,
// so a wildcard ("0.0.0.0:6120", ":6120") or a LAN/public IP is refused rather than warned about.
// A host name is resolved with lookup and every address it maps to must pass.
func CheckListen(ctx context.Context, listen string, lookup func(ctx context.Context, host string) ([]netip.Addr, error)) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", listen, err)
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return fmt.Errorf("listen address %q binds every interface; use this machine's tailnet IP or 127.0.0.1", listen)
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		if lookup == nil {
			lookup = defaultLookup
		}
		if addrs, err = lookup(ctx, host); err != nil {
			return fmt.Errorf("listen address %q: resolve %s: %w", listen, host, err)
		}
		if len(addrs) == 0 {
			return fmt.Errorf("listen address %q: %s resolves to nothing", listen, host)
		}
	}
	for _, a := range addrs {
		a = a.Unmap()
		switch {
		case a.IsUnspecified():
			return fmt.Errorf("listen address %q binds every interface; use this machine's tailnet IP or 127.0.0.1", listen)
		case a.IsLoopback(), tailscaleV4.Contains(a), tailscaleV6.Contains(a):
		default:
			return fmt.Errorf("listen address %q includes %s, which is not loopback or a tailnet address; grove only listens on the tailnet", listen, a)
		}
	}
	return nil
}

func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}
