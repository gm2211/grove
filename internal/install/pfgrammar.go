package install

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The VM firewall's rules grammar: what the root helper (roothelper.go) agrees to load into the pf
// anchor com.apple/grove-vm on behalf of the worker user. The user's grove renders the rules
// (hostFirewallRules); the helper, which runs as root and trusts nothing the user sends, loads them
// only if every line is one of:
//
//   - a blank line, or a comment starting with '#';
//   - grove_vm_if = "{ bridge100 … }": the vmnet bridges, nothing else;
//   - grove_<name> = "<address>" or "{ <address> … }", an address being an IPv4 address or CIDR,
//     or self;
//   - table <grove_<name>> const { <IPv4 CIDR> … };
//   - pass|block [return|drop] in|out quick on $grove_vm_if, followed only by filter words (inet,
//     inet6, proto, udp, tcp, icmp, ipv6-icmp, from, to, any, all, port, keep state, self, braces),
//     port numbers, addresses, and the macros and tables defined above.
//
// So the worst a request can do is loosen or tighten the fence around the user's own VMs, which
// the user can already do by running a VM without it. No rdr, nat, route-to, anchor, load, set,
// scrub or include, no other interface, no table file, no quoting or line continuation outside a
// macro's value: nothing that reaches the Mac's own traffic or reads a file.
//
// Freeze it: a grove whose rules need more than this needs a reinstalled helper
// (`sudo grove root-helper install`), and pfgrammar_test.go keeps today's rules inside it.

const (
	pfMaxLines    = 200
	pfMaxLineLen  = 512
	pfMaxRuleSize = 16 << 10
)

var (
	pfCommentRE = regexp.MustCompile(`^#[ -~]*$`)
	pfCharsRE   = regexp.MustCompile(`^[A-Za-z0-9 \t._:/<>${}"=#-]*$`)
	pfMacroRE   = regexp.MustCompile(`^(grove_[a-z0-9_]{1,32}) = "([^"]*)"$`)
	pfTableRE   = regexp.MustCompile(`^table <(grove_[a-z0-9_]{1,32})> const \{ ([0-9./ ]+) \}$`)
	pfBridgeSet = func() map[string]bool {
		m := map[string]bool{}
		for _, b := range vmnetBridges {
			m[b] = true
		}
		return m
	}()
	pfFilterWords = map[string]bool{
		"inet": true, "inet6": true, "proto": true, "udp": true, "tcp": true, "icmp": true,
		"ipv6-icmp": true, "from": true, "to": true, "any": true, "all": true, "port": true,
		"keep": true, "state": true, "self": true, "{": true, "}": true,
	}
)

// ValidateVMFirewallRules reports why rules are not inside the grammar above, or nil.
func ValidateVMFirewallRules(rules string) error {
	if len(rules) > pfMaxRuleSize {
		return fmt.Errorf("the rules are %d bytes, over %d", len(rules), pfMaxRuleSize)
	}
	lines := strings.Split(rules, "\n")
	if len(lines) > pfMaxLines {
		return fmt.Errorf("the rules are %d lines, over %d", len(lines), pfMaxLines)
	}
	macros := map[string]bool{} // address macros
	tables := map[string]bool{}
	haveIf := false
	rulesSeen := 0
	for i, raw := range lines {
		n := i + 1
		if len(raw) > pfMaxLineLen {
			return fmt.Errorf("line %d is over %d characters", n, pfMaxLineLen)
		}
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			if !pfCommentRE.MatchString(line) {
				return fmt.Errorf("line %d: a comment may hold only printable ASCII", n)
			}
			continue
		}
		if !pfCharsRE.MatchString(raw) {
			return fmt.Errorf("line %d has a character the grammar doesn't allow", n)
		}
		switch {
		case line == "":
			continue
		case strings.Contains(line, "#"):
			return fmt.Errorf("line %d: a comment must take the whole line", n)
		}
		if m := pfMacroRE.FindStringSubmatch(line); m != nil {
			name, value := m[1], m[2]
			if macros[name] || tables[name] || (name == "grove_vm_if" && haveIf) {
				return fmt.Errorf("line %d: %s is defined twice", n, name)
			}
			if name == "grove_vm_if" {
				if err := pfBridgeList(value); err != nil {
					return fmt.Errorf("line %d: %w", n, err)
				}
				haveIf = true
				continue
			}
			if err := pfAddressValue(value); err != nil {
				return fmt.Errorf("line %d: %s: %w", n, name, err)
			}
			macros[name] = true
			continue
		}
		if strings.HasPrefix(line, "table") {
			m := pfTableRE.FindStringSubmatch(line)
			if m == nil {
				return fmt.Errorf("line %d: a table must be `table <grove_name> const { CIDR … }`", n)
			}
			if macros[m[1]] || tables[m[1]] {
				return fmt.Errorf("line %d: %s is defined twice", n, m[1])
			}
			for _, c := range strings.Fields(m[2]) {
				if !pfIPv4(c) {
					return fmt.Errorf("line %d: %q is not an IPv4 address or CIDR", n, c)
				}
			}
			tables[m[1]] = true
			continue
		}
		if strings.Contains(line, `"`) || strings.Contains(line, "=") {
			return fmt.Errorf("line %d: only a grove_ macro may use quotes or =", n)
		}
		if err := pfRule(strings.Fields(line), haveIf, macros, tables); err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
		rulesSeen++
	}
	if rulesSeen == 0 {
		return fmt.Errorf("there are no rules")
	}
	return nil
}

func pfBridgeList(value string) error {
	items, ok := pfList(value)
	if !ok || len(items) == 0 {
		return fmt.Errorf(`grove_vm_if must be "{ bridge100 … }"`)
	}
	for _, it := range items {
		if !pfBridgeSet[it] {
			return fmt.Errorf("grove_vm_if may name only vmnet's bridges (bridge100 to bridge109), not %q", it)
		}
	}
	return nil
}

func pfAddressValue(value string) error {
	items, ok := pfList(value)
	if !ok {
		items = []string{value}
	}
	if len(items) == 0 {
		return fmt.Errorf("an empty address list")
	}
	for _, it := range items {
		if it != "self" && !pfIPv4(it) {
			return fmt.Errorf("%q is not an IPv4 address, CIDR or self", it)
		}
	}
	return nil
}

// pfList splits "{ a b c }" into its items; ok is false when value is not a braced list.
func pfList(value string) ([]string, bool) {
	f := strings.Fields(value)
	if len(f) < 2 || f[0] != "{" || f[len(f)-1] != "}" {
		return nil, false
	}
	items := f[1 : len(f)-1]
	if slices.Contains(items, "{") || slices.Contains(items, "}") {
		return nil, false
	}
	return items, true
}

func pfIPv4(s string) bool {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Addr().Is4()
	}
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4()
}

func pfRule(tok []string, haveIf bool, macros, tables map[string]bool) error {
	i := 0
	next := func() string {
		if i >= len(tok) {
			return ""
		}
		i++
		return tok[i-1]
	}
	action := next()
	if action != "pass" && action != "block" {
		return fmt.Errorf("a rule must start with pass or block, not %q", action)
	}
	dir := next()
	if action == "block" && (dir == "return" || dir == "drop") {
		dir = next()
	}
	if dir != "in" && dir != "out" {
		return fmt.Errorf("a rule must say in or out, not %q", dir)
	}
	if next() != "quick" || next() != "on" || next() != "$grove_vm_if" {
		return fmt.Errorf("a rule must read `%s … %s quick on $grove_vm_if`", action, dir)
	}
	if !haveIf {
		return fmt.Errorf("$grove_vm_if is used before it is defined")
	}
	depth := 0
	afterPort := false
	for _, t := range tok[i:] {
		switch {
		case t == "{":
			depth++
		case t == "}":
			depth--
			if depth < 0 {
				return fmt.Errorf("unbalanced braces")
			}
		}
		switch {
		case pfFilterWords[t]:
		case strings.HasPrefix(t, "$"):
			if !macros[t[1:]] {
				return fmt.Errorf("%s is not a defined address macro", t)
			}
		case strings.HasPrefix(t, "<") && strings.HasSuffix(t, ">"):
			if !tables[t[1:len(t)-1]] {
				return fmt.Errorf("%s is not a defined table", t)
			}
		case afterPort && pfPort(t):
		case pfIPv4(t):
		default:
			return fmt.Errorf("%q is not a word the grammar allows", t)
		}
		if t == "port" {
			afterPort = true
		} else if t != "{" && !(afterPort && pfPort(t)) {
			afterPort = false
		}
	}
	if depth != 0 {
		return fmt.Errorf("unbalanced braces")
	}
	return nil
}

func pfPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n < 65536 && strconv.Itoa(n) == s
}
