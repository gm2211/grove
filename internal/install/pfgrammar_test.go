package install

import (
	"net/netip"
	"strings"
	"testing"
)

// The grammar is frozen: today's rules must stay inside it, or a Mac with the root helper would
// need an admin again. A change that fails this test needs a new request version and a helper
// reinstall, never a quietly widened grammar.
func TestVMFirewallGrammarAcceptsGroveRules(t *testing.T) {
	gw := netip.MustParseAddr("192.168.64.1")
	for _, nomad := range []nomadTargets{nil, {netip.MustParseAddr("100.64.0.7")}, {netip.MustParseAddr("100.64.0.7"), netip.MustParseAddr("10.0.0.2")}} {
		if err := ValidateVMFirewallRules(hostFirewallRules(gw, nomad)); err != nil {
			t.Fatalf("grove's own rules (nomad %v) refused: %v", nomad, err)
		}
	}
}

func TestVMFirewallGrammarRefuses(t *testing.T) {
	good := hostFirewallRules(netip.MustParseAddr("192.168.64.1"), nil)
	cases := map[string]string{
		"rdr":                 `rdr pass on en0 inet proto tcp to any port 22 -> 127.0.0.1 port 22`,
		"nat":                 `nat on en0 from 192.168.64.0/24 to any -> (en0)`,
		"anchor":              `anchor "evil"`,
		"load anchor":         `load anchor "evil" from "/Users/argos/evil.conf"`,
		"include":             `include "/Users/argos/evil.conf"`,
		"set":                 `set skip on lo0`,
		"scrub":               `scrub in all`,
		"other interface":     `pass in quick on en0 all keep state`,
		"interface by name":   `pass in quick on bridge100 all keep state`,
		"route-to":            `pass out quick on $grove_vm_if route-to (en0 10.0.0.1) all keep state`,
		"undefined macro":     `pass in quick on $grove_vm_if from any to $nope keep state`,
		"interface as addr":   `pass in quick on $grove_vm_if from any to $grove_vm_if keep state`,
		"undefined table":     `block in quick on $grove_vm_if from any to <nope>`,
		"table file":          `table <grove_x> persist file "/etc/passwd"`,
		"macro injection":     `grove_x = "{ 1.2.3.4 } rdr pass"`,
		"macro interface":     `grove_vm_if2 = "{ en0 }"`,
		"bridge0":             `grove_vm_if = "{ bridge0 }"`,
		"redefined if":        `grove_vm_if = "{ bridge100 }"`,
		"redefined macro":     `grove_gateway = "10.0.0.1"`,
		"ipv6 address":        `grove_v6 = "{ fe80::1 }"`,
		"trailing comment":    `pass in quick on $grove_vm_if all keep state # hi`,
		"continuation":        "pass in quick on $grove_vm_if \\",
		"semicolon":           `pass in quick on $grove_vm_if all; rdr on en0`,
		"quote in rule":       `pass in quick on $grove_vm_if "all"`,
		"bare number":         `pass in quick on $grove_vm_if 22 keep state`,
		"unbalanced":          `pass in quick on $grove_vm_if proto { udp tcp keep state`,
		"label":               `pass in quick on $grove_vm_if all label x`,
		"tag":                 `pass in quick on $grove_vm_if all tag x`,
		"not quick":           `pass in on $grove_vm_if all keep state`,
		"no direction":        `pass quick on $grove_vm_if all keep state`,
		"port out of range":   `pass in quick on $grove_vm_if proto tcp from any to any port 70000 keep state`,
		"newline in macro":    "grove_y = \"{ 1.2.3.4\n} rdr\"",
		"user":                `pass in quick on $grove_vm_if proto tcp all user root keep state`,
		"negation":            `pass in quick on $grove_vm_if from ! 10.0.0.0/8 keep state`,
		"table not const":     `table <grove_t> { 10.0.0.0/8 }`,
		"table with hostname": `table <grove_t> const { example.com }`,
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateVMFirewallRules(good + bad + "\n"); err == nil {
				t.Fatalf("accepted %q", bad)
			}
		})
	}
	if err := ValidateVMFirewallRules("# nothing\n"); err == nil {
		t.Fatal("accepted rules with no rule")
	}
	if err := ValidateVMFirewallRules(strings.Repeat("# x\n", pfMaxLines+1) + "pass out quick on $grove_vm_if all\n"); err == nil {
		t.Fatal("accepted too many lines")
	}
}
