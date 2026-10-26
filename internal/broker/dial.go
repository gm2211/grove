package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// nonPublic lists the ranges a job may never reach through the broker: the tailnet, the home LAN,
// the host itself and every other special-purpose block. A job VM is already fenced off these by
// its own firewall; the broker dials from the host, which is not, so it must refuse them itself.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT, and the tailnet
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: embeds an IPv4 address of any kind
	netip.MustParsePrefix("64:ff9b:1::/48"), // local NAT64
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"), // Teredo
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), // 6to4: embeds an IPv4 address of any kind
	netip.MustParsePrefix("fc00::/7"),  // ULA, and the tailnet
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// Public reports whether a is an address on the public internet, the only kind of address the
// broker connects a job to.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// ErrNotPublic is returned when a destination resolves to no public address.
var ErrNotPublic = errors.New("destination is not on the public internet")

// Dialer connects only to public addresses. It resolves a host name itself and dials the address
// it checked, so a name that re-resolves to a private address between check and dial (DNS
// rebinding) can't slip through.
type Dialer struct {
	// Lookup resolves a host name; nil means the system resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial opens the connection; nil means a net.Dialer with a 30-second timeout.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// DialContext dials addr ("host:port") over TCP if host is, or resolves to, a public address.
// Addresses that aren't public are skipped; if none is left, it fails with ErrNotPublic.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("broker: refusing %s connection", network)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	addrs, err := d.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	dial := d.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	var firstErr error
	for _, a := range addrs {
		conn, err := dial(ctx, "tcp", net.JoinHostPort(a.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// resolve returns host's public addresses, or ErrNotPublic when it has none.
func (d *Dialer) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
			return nil, fmt.Errorf("%w: %q", ErrNotPublic, host)
		}
		lookup := d.Lookup
		if lookup == nil {
			lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}
		}
		if addrs, err = lookup(ctx, host); err != nil {
			return nil, err
		}
	}
	public := addrs[:0:0]
	for _, a := range addrs {
		if Public(a) {
			public = append(public, a.Unmap())
		}
	}
	if len(public) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotPublic, host)
	}
	return public, nil
}
