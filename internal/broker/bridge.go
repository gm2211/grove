package broker

import (
	"fmt"
	"net"
	"net/netip"
)

// DefaultGateway is the host's address on vmnet's shared network when the Mac's vmnet settings
// leave it at the default. Tart VMs, softnet ones included, get it as their DHCP gateway.
var DefaultGateway = netip.MustParseAddr("192.168.64.1")

// CheckListenAddr refuses any address but a private one (RFC 1918 or IPv6 ULA) outside the
// tailnet. The broker is for the VMs on this Mac's bridge: listening on a wildcard, loopback, the
// tailnet or a public address would offer it to machines that have no business with it.
func CheckListenAddr(a netip.Addr) error {
	a = a.Unmap()
	switch {
	case !a.IsValid():
		return fmt.Errorf("broker listen address is empty")
	case a.IsUnspecified():
		return fmt.Errorf("broker must not listen on every interface (%s); give it the VM bridge address", a)
	case a.IsLoopback():
		return fmt.Errorf("broker listen address %s is loopback, which job VMs can't reach", a)
	case netip.MustParsePrefix("100.64.0.0/10").Contains(a), netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(a):
		return fmt.Errorf("broker listen address %s is on the tailnet; give it the VM bridge address", a)
	case !a.IsPrivate():
		return fmt.Errorf("broker listen address %s is not a private address; give it the VM bridge address", a)
	}
	return nil
}

// HasLocalAddr reports whether a network interface on this machine holds a. vmnet's bridge (and so
// its address) exists only while at least one VM runs, so the broker waits for it.
func HasLocalAddr(a netip.Addr) (bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false, err
	}
	for _, ia := range addrs {
		if n, ok := ia.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == a.Unmap() {
				return true, nil
			}
		}
	}
	return false, nil
}
