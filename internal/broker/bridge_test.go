package broker

import (
	"net/netip"
	"testing"
)

func TestCheckListenAddr(t *testing.T) {
	for addr, ok := range map[string]bool{
		"192.168.64.1":      true,
		"10.0.5.1":          true,
		"fd00::1":           true,
		"0.0.0.0":           false,
		"::":                false,
		"127.0.0.1":         false,
		"100.101.102.103":   false,
		"fd7a:115c:a1e0::5": false,
		"8.8.8.8":           false,
	} {
		if err := CheckListenAddr(netip.MustParseAddr(addr)); (err == nil) != ok {
			t.Errorf("CheckListenAddr(%s) = %v, want ok=%t", addr, err, ok)
		}
	}
	if CheckListenAddr(netip.Addr{}) == nil {
		t.Error("empty address accepted")
	}
}

func TestHasLocalAddr(t *testing.T) {
	if ok, err := HasLocalAddr(netip.MustParseAddr("127.0.0.1")); err != nil || !ok {
		t.Fatalf("loopback: %t, %v", ok, err)
	}
	if ok, _ := HasLocalAddr(netip.MustParseAddr("203.0.113.77")); ok {
		t.Fatal("a documentation address is local?")
	}
}
