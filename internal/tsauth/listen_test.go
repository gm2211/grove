package tsauth

import (
	"context"
	"net/netip"
	"testing"
)

func TestCheckListen(t *testing.T) {
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "grove-cp.tail1234.ts.net":
			return []netip.Addr{netip.MustParseAddr("100.101.102.103")}, nil
		case "studio.local":
			return []netip.Addr{netip.MustParseAddr("100.101.102.103"), netip.MustParseAddr("192.168.1.20")}, nil
		}
		return nil, nil
	}
	ok := []string{
		"127.0.0.1:6120",
		"[::1]:6120",
		"100.64.0.5:6120",
		"[fd7a:115c:a1e0::1]:6120",
		"grove-cp.tail1234.ts.net:6120",
	}
	for _, l := range ok {
		if err := CheckListen(context.Background(), l, lookup); err != nil {
			t.Errorf("CheckListen(%q) = %v, want nil", l, err)
		}
	}
	bad := []string{
		"0.0.0.0:6120",
		":6120",
		"[::]:6120",
		"192.168.1.20:6120",
		"203.0.113.7:6120",
		"studio.local:6120", // one tailnet address is not enough: every address must be private
		"nowhere.example:6120",
		"no-port",
	}
	for _, l := range bad {
		if err := CheckListen(context.Background(), l, lookup); err == nil {
			t.Errorf("CheckListen(%q) = nil, want an error", l)
		}
	}
}
