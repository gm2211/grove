package broker

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":              true,
		"140.82.112.6":         true,
		"2606:4700:4700::1111": true,
		"10.1.2.3":             false,
		"172.16.0.1":           false,
		"192.168.1.10":         false,
		"192.168.64.1":         false, // the VM bridge, i.e. the host
		"100.100.100.100":      false, // tailnet
		"100.64.0.1":           false,
		"127.0.0.1":            false,
		"169.254.169.254":      false,
		"0.0.0.0":              false,
		"224.0.0.1":            false,
		"255.255.255.255":      false,
		"198.18.0.1":           false,
		"::1":                  false,
		"::":                   false,
		"fd7a:115c:a1e0::1":    false, // tailnet
		"fc00::1":              false,
		"fe80::1":              false,
		"::ffff:10.0.0.1":      false,
		"::ffff:8.8.8.8":       true,
		"64:ff9b::a00:1":       false, // NAT64 of 10.0.0.1
		"2002:a00:1::1":        false, // 6to4 of 10.0.0.1
		"2001:0:4136:e378::1":  false, // Teredo
		"2001:db8::1":          false,
		"ff02::1":              false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %t, want %t", addr, got, want)
		}
	}
}

func TestDialerOnlyDialsPublicAddresses(t *testing.T) {
	var dialed []string
	d := &Dialer{
		Lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "mixed.test":
				return []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("93.184.216.34")}, nil
			case "private.test":
				return []netip.Addr{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("100.101.102.103")}, nil
			}
			return nil, errors.New("no such host")
		},
		Dial: func(_ context.Context, network, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			a, b := net.Pipe()
			b.Close()
			return a, nil
		},
	}
	ctx := context.Background()
	if c, err := d.DialContext(ctx, "tcp", "mixed.test:443"); err != nil {
		t.Fatalf("mixed: %v", err)
	} else {
		c.Close()
	}
	if len(dialed) != 1 || dialed[0] != "93.184.216.34:443" {
		t.Fatalf("dialed %v, want only the public address, by IP", dialed)
	}
	for _, addr := range []string{"private.test:443", "10.0.0.1:22", "[::1]:80", "localhost:80", "foo.localhost:80", "192.168.64.1:3128"} {
		if _, err := d.DialContext(ctx, "tcp", addr); !errors.Is(err, ErrNotPublic) {
			t.Fatalf("%s: err = %v, want ErrNotPublic", addr, err)
		}
	}
	if _, err := d.DialContext(ctx, "udp", "8.8.8.8:53"); err == nil {
		t.Fatal("udp dial allowed")
	}
	if len(dialed) != 1 {
		t.Fatalf("refused destinations were dialed: %v", dialed)
	}
}
